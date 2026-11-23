package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zachahn/editor/internal/session"
)

type fakeRunner struct {
	mu      sync.Mutex
	err     error
	result  string
	calls   []string
	prompts []string
	hold    chan struct{}
	started chan struct{}
}

func (f *fakeRunner) Run(_ context.Context, dir, systemPrompt, userMessage string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, userMessage)
	f.prompts = append(f.prompts, systemPrompt)
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.hold != nil {
		<-f.hold
	}
	if f.err != nil {
		return "", f.err
	}
	return f.result, nil
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

func newHandlers(t *testing.T, runner AgentRunner) (*Handlers, string) {
	root := filepath.Join(t.TempDir(), "sessions")
	return &Handlers{Store: session.NewStore(root), Runner: runner, SystemPrompt: "config prompt"}, root
}

func TestStartAndRevise(t *testing.T) {
	runner := &fakeRunner{result: "rewritten"}
	h, root := newHandlers(t, runner)

	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "original"}))
	if res.IsError {
		t.Fatal(text(t, res))
	}
	body := text(t, res)
	header, rest, _ := strings.Cut(body, "\n\n")
	id := strings.TrimPrefix(header, "session_id: ")
	if !session.IDPattern.MatchString(id) || rest != "rewritten" {
		t.Fatalf("body = %q", body)
	}
	if runner.calls[0] != "Rewrite CURRENT.txt." {
		t.Errorf("first message = %q", runner.calls[0])
	}

	matches, _ := filepath.Glob(filepath.Join(root, id+"-*"))
	if len(matches) != 1 {
		t.Fatalf("session dirs = %v", matches)
	}
	os.WriteFile(filepath.Join(matches[0], "SYSTEM.txt"), []byte("session prompt"), 0o600)

	runner.result = "revised"
	res, _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "shorter"}))
	if res.IsError || text(t, res) != "revised" {
		t.Fatalf("revise = %+v", res)
	}
	if runner.calls[1] != "Feedback on CURRENT.txt:\n\nshorter" || runner.prompts[1] != "session prompt" {
		t.Errorf("revise call = %q with prompt %q", runner.calls[1], runner.prompts[1])
	}
}

func TestStartFailureDeletesSession(t *testing.T) {
	h, root := newHandlers(t, &fakeRunner{err: errors.New("platform unreachable")})
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "original"}))
	if !res.IsError || !strings.Contains(text(t, res), "platform unreachable") || strings.Contains(text(t, res), "session_id") {
		t.Fatalf("res = %+v", res)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("session left behind: %v", entries)
	}
}

func TestValidation(t *testing.T) {
	runner := &fakeRunner{result: "x"}
	h, _ := newHandlers(t, runner)
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
}

func TestSameSessionCallsAreSerialized(t *testing.T) {
	runner := &fakeRunner{result: "x"}
	h, _ := newHandlers(t, runner)
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "t"}))
	id := strings.TrimPrefix(strings.SplitN(text(t, res), "\n", 2)[0], "session_id: ")

	runner.hold = make(chan struct{})
	runner.started = make(chan struct{}, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "f"}))
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
}

func TestErrorsOmitSessionUnixtime(t *testing.T) {
	h, root := newHandlers(t, &fakeRunner{result: "x"})
	h.Store.Now = func() time.Time { return time.Unix(1700000000, 0) }
	res, _ := h.StartRewrite(context.Background(), request(map[string]any{"text": "t"}))
	id := strings.TrimPrefix(strings.SplitN(text(t, res), "\n", 2)[0], "session_id: ")

	h.Runner = &fakeRunner{err: errors.New("open " + filepath.Join(root, id+"-1700000000", "CURRENT.txt") + ": denied")}
	res, _ = h.ReviseRewrite(context.Background(), request(map[string]any{"session_id": id, "feedback": "f"}))
	msg := text(t, res)
	if !res.IsError || strings.Contains(msg, "1700000000") || !strings.Contains(msg, id) {
		t.Fatalf("message = %q", msg)
	}
}
