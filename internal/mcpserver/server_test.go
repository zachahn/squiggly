package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zachahn/squiggly/internal/agent"
	"github.com/zachahn/squiggly/internal/session"
)

type fakeRunner struct {
	mu        sync.Mutex
	err       error
	result    string
	changed   map[string]string
	calls     []string
	prompts   []string
	snapshots []*session.Snapshot
	deadlines []time.Time
	hold      chan struct{}
	started   chan struct{}
	beforeEnd func()
}

func (f *fakeRunner) Run(ctx context.Context, in agent.RunInput) (*agent.RunResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in.UserMessage)
	f.prompts = append(f.prompts, in.SystemPrompt)
	f.snapshots = append(f.snapshots, in.Snapshot)
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.hold != nil {
		<-f.hold
	}
	in.Record(ctx, schema.SystemMessage(in.SystemPrompt))
	in.Record(ctx, schema.UserMessage(in.UserMessage))
	in.Record(ctx, schema.AssistantMessage("done", nil))
	if f.beforeEnd != nil {
		f.beforeEnd()
	}
	changed := map[string]string{}
	for name, content := range f.changed {
		changed[name] = content
	}
	if f.err != nil {
		return &agent.RunResult{Changed: changed}, f.err
	}
	if _, ok := changed[session.CurrentFile]; !ok {
		changed[session.CurrentFile] = f.result
	}
	return &agent.RunResult{Current: f.result, Changed: changed}, nil
}

func request(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content blocks = %d", len(res.Content))
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T", res.Content[0])
	}
	return tc.Text
}

func newHandlers(t *testing.T, runner AgentRunner) *Handlers {
	t.Helper()
	db, err := session.Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := session.NewStore(db, session.RequestSettings{Platform: "ollama", Model: "m", MaxSteps: 2, RequestTimeoutSeconds: 30})
	return &Handlers{Store: store, Runner: runner, SystemPrompt: "config prompt"}
}

func sessionID(t *testing.T, body string) string {
	t.Helper()
	firstLine, _, _ := strings.Cut(body, "\n")
	id := strings.TrimPrefix(firstLine, "session_id: ")
	if !session.IDPattern.MatchString(id) {
		t.Fatalf("body = %q", body)
	}
	return id
}

type requestRow struct {
	sessionID  sql.NullInt64
	tool       string
	arguments  string
	status     string
	errMessage sql.NullString
	platform   string
	model      string
	startedAt  int64
	deadlineAt int64
	finishedAt sql.NullInt64
	messages   int
}

func requestRows(t *testing.T, h *Handlers) []requestRow {
	t.Helper()
	rows, err := h.Store.DB.Query(`
		SELECT session_id, tool, arguments, status, error, platform, model, started_at, deadline_at, finished_at,
		       (SELECT COUNT(*) FROM messages m WHERE m.request_id = r.id)
		FROM requests r ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []requestRow
	for rows.Next() {
		var r requestRow
		if err := rows.Scan(&r.sessionID, &r.tool, &r.arguments, &r.status, &r.errMessage, &r.platform, &r.model, &r.startedAt, &r.deadlineAt, &r.finishedAt, &r.messages); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func currentVersions(t *testing.T, h *Handlers, publicID, name string) []string {
	t.Helper()
	rows, err := h.Store.DB.Query(`
		SELECT v.content FROM file_versions v
		JOIN files f ON f.id = v.file_id JOIN sessions s ON s.id = f.session_id
		WHERE s.public_id = ? AND f.name = ? ORDER BY v.id`, publicID, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		rows.Scan(&content)
		out = append(out, content)
	}
	return out
}

func TestStartAndRevise(t *testing.T) {
	runner := &fakeRunner{result: "rewritten", changed: map[string]string{"SCRATCH-1.txt": "notes"}}
	h := newHandlers(t, runner)

	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "original"}))
	if res.IsError {
		t.Fatal(text(t, res))
	}
	body := text(t, res)
	id := sessionID(t, body)
	if body != "session_id: "+id+"\n"+ReviewReminder+"\n\nrewritten" {
		t.Fatalf("body = %q", body)
	}
	if runner.calls[0] != "Rewrite CURRENT.txt." || runner.prompts[0] != "config prompt" {
		t.Errorf("first call = %q with prompt %q", runner.calls[0], runner.prompts[0])
	}
	if got := runner.snapshots[0].Files[session.CurrentFile]; got.Content != "original" || !got.Writable {
		t.Errorf("snapshot CURRENT = %+v", got)
	}
	if got := runner.snapshots[0].Files[session.OriginalFile]; got.Content != "original" || got.Writable {
		t.Errorf("snapshot ORIGINAL = %+v", got)
	}

	h.SystemPrompt = "changed config prompt"
	runner.result = "revised"
	runner.changed = nil
	res, _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "shorter"}))
	if res.IsError || text(t, res) != "revised" {
		t.Fatalf("revise = %+v", res)
	}
	if runner.calls[1] != "Feedback on CURRENT.txt:\n\nshorter" || runner.prompts[1] != "config prompt" {
		t.Errorf("revise call = %q with prompt %q", runner.calls[1], runner.prompts[1])
	}
	if got := runner.snapshots[1].Files["SCRATCH-1.txt"]; got.Content != "notes" || !got.Writable {
		t.Errorf("scratch did not persist: %+v", got)
	}
	if runner.snapshots[1].Version != 1 {
		t.Errorf("revise snapshot version = %d", runner.snapshots[1].Version)
	}

	if got := currentVersions(t, h, id, session.CurrentFile); strings.Join(got, "|") != "original|rewritten|revised" {
		t.Errorf("CURRENT history = %v", got)
	}
	if got := currentVersions(t, h, id, session.OriginalFile); strings.Join(got, "|") != "original" {
		t.Errorf("ORIGINAL history = %v", got)
	}

	rows := requestRows(t, h)
	if len(rows) != 2 {
		t.Fatalf("requests = %+v", rows)
	}
	start, revise := rows[0], rows[1]
	if start.tool != "start_rewrite" || start.arguments != "{}" || start.status != "ok" || start.errMessage.Valid || !start.finishedAt.Valid || start.messages != 3 {
		t.Errorf("start row = %+v", start)
	}
	if start.platform != "ollama" || start.model != "m" || start.deadlineAt-start.startedAt != 60_000 {
		t.Errorf("start row settings = %+v", start)
	}
	if !runner.deadlines[0].Equal(time.UnixMilli(start.deadlineAt)) {
		t.Errorf("run deadline = %v, want %d", runner.deadlines[0], start.deadlineAt)
	}
	if revise.tool != "revise_rewrite" || revise.arguments != `{"feedback":"shorter","session_id":"`+id+`"}` || revise.status != "ok" || revise.sessionID != start.sessionID {
		t.Errorf("revise row = %+v", revise)
	}
}

func TestStartFailureKeepsSession(t *testing.T) {
	runner := &fakeRunner{err: errors.New("platform unreachable"), changed: map[string]string{session.CurrentFile: "partial"}}
	h := newHandlers(t, runner)
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "original"}))
	if !res.IsError {
		t.Fatalf("res = %+v", res)
	}
	msg := text(t, res)
	id := sessionID(t, msg)
	if msg != "session_id: "+id+"\n\nplatform unreachable" {
		t.Fatalf("message = %q", msg)
	}

	rows := requestRows(t, h)
	if len(rows) != 1 || rows[0].status != "error" || rows[0].errMessage.String != msg || !rows[0].finishedAt.Valid || !rows[0].sessionID.Valid {
		t.Fatalf("rows = %+v", rows)
	}
	if got := currentVersions(t, h, id, session.CurrentFile); strings.Join(got, "|") != "original|partial" {
		t.Errorf("CURRENT history = %v", got)
	}

	runner.err = nil
	runner.changed = nil
	runner.result = "recovered"
	res, _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "try again"}))
	if res.IsError || text(t, res) != "recovered" {
		t.Fatalf("revise after failure = %+v", res)
	}
	if got := runner.snapshots[1].Files[session.CurrentFile].Content; got != "original" {
		t.Errorf("failed version became current: %q", got)
	}
}

func TestVersionConflict(t *testing.T) {
	runner := &fakeRunner{result: "x"}
	h := newHandlers(t, runner)
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "t"}))
	id := sessionID(t, text(t, res))

	runner.beforeEnd = func() {
		h.Store.DB.Exec(`UPDATE sessions SET version = version + 1 WHERE public_id = ?`, id)
	}
	runner.result = "lost"
	res, _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "f"}))
	if !res.IsError || !strings.Contains(text(t, res), "revised by another process") {
		t.Fatalf("res = %+v", res)
	}
	rows := requestRows(t, h)
	if last := rows[len(rows)-1]; last.status != "error" || !strings.Contains(last.errMessage.String, "another process") {
		t.Errorf("row = %+v", last)
	}
	if got := currentVersions(t, h, id, session.CurrentFile); strings.Join(got, "|") != "t|x|lost" {
		t.Errorf("CURRENT history = %v", got)
	}
	runner.beforeEnd = nil
	if got := runner.snapshots[len(runner.snapshots)-1].Files[session.CurrentFile].Content; got != "x" {
		t.Errorf("snapshot = %q", got)
	}
}

func TestValidationIsRecorded(t *testing.T) {
	runner := &fakeRunner{result: "x"}
	h := newHandlers(t, runner)
	ctx := context.Background()
	cases := []struct {
		name    string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args    map[string]any
		want    string
	}{
		{"missing text", h.StartRewrite, map[string]any{}, "missing"},
		{"empty text", h.StartRewrite, map[string]any{"text": ""}, "empty"},
		{"non-string text", h.StartRewrite, map[string]any{"text": 5}, "string"},
		{"bad id", h.ReviseRewrite, map[string]any{"session_id": "../etc", "feedback": "f"}, "invalid session_id"},
		{"uppercase id", h.ReviseRewrite, map[string]any{"session_id": "ABCDEF12", "feedback": "f"}, "invalid session_id"},
		{"id with newline", h.ReviseRewrite, map[string]any{"session_id": "abcdef12\n", "feedback": "f"}, "invalid session_id"},
		{"empty feedback", h.ReviseRewrite, map[string]any{"session_id": "abcdef12", "feedback": ""}, "empty"},
		{"unknown session", h.ReviseRewrite, map[string]any{"session_id": "abcdef12", "feedback": "f"}, "unknown session"},
	}
	for _, c := range cases {
		res, err := c.handler(ctx, request(c.args))
		if err != nil || !res.IsError || !strings.Contains(text(t, res), c.want) {
			t.Errorf("%s: res = %+v, err = %v", c.name, res, err)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner called %d times", len(runner.calls))
	}

	rows := requestRows(t, h)
	if len(rows) != len(cases) {
		t.Fatalf("rows = %d, want %d", len(rows), len(cases))
	}
	for i, row := range rows {
		if row.sessionID.Valid || row.status != "error" || !row.errMessage.Valid || row.deadlineAt != row.startedAt || row.finishedAt.Int64 != row.startedAt || row.messages != 0 {
			t.Errorf("%s: row = %+v", cases[i].name, row)
		}
	}
	if rows[2].arguments != `{"text":5}` || rows[7].arguments != `{"feedback":"f","session_id":"abcdef12"}` {
		t.Errorf("arguments = %q, %q", rows[2].arguments, rows[7].arguments)
	}
}

func TestSameSessionCallsAreSerialized(t *testing.T) {
	runner := &fakeRunner{result: "x"}
	h := newHandlers(t, runner)
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "t"}))
	id := sessionID(t, text(t, res))

	runner.hold = make(chan struct{})
	runner.started = make(chan struct{}, 2)
	var wg sync.WaitGroup
	results := make([]*mcp.CallToolResult, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "f"}))
		}()
	}
	<-runner.started
	select {
	case <-runner.started:
		t.Fatal("second call started while first was running")
	case <-time.After(50 * time.Millisecond):
	}
	runner.hold <- struct{}{}
	<-runner.started
	runner.hold <- struct{}{}
	wg.Wait()
	for _, res := range results {
		if res.IsError {
			t.Errorf("serialized call failed: %s", text(t, res))
		}
	}
}

func TestFailureRecordingErrorIsReported(t *testing.T) {
	runner := &fakeRunner{result: "x"}
	h := newHandlers(t, runner)
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "t"}))
	id := sessionID(t, text(t, res))

	runner.err = errors.New("model exploded")
	runner.beforeEnd = func() { h.Store.DB.Close() }
	res, _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "f"}))
	msg := text(t, res)
	if !res.IsError || !strings.Contains(msg, "model exploded") || !strings.Contains(msg, "record failure") {
		t.Fatalf("message = %q", msg)
	}
}
