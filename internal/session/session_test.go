package session

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var testSettings = RequestSettings{Platform: "llamacpp", Model: "m", MaxSteps: 3, RequestTimeoutSeconds: 10}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "editor", "editor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewStore(db, testSettings)
	clock := time.UnixMilli(1_000_000)
	store.Now = func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}
	return store
}

func TestDBPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, _ := DBPath(); got != "/state/editor/editor.db" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, _ := DBPath(); got != "/home/u/.local/state/editor/editor.db" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenSetsModesPragmasAndSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space", "editor")
	path := filepath.Join(dir, "editor.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if info, _ := os.Stat(dir); info.Mode().Perm() != DirMode {
		t.Errorf("dir mode = %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != FileMode {
		t.Errorf("file mode = %v", info.Mode().Perm())
	}

	db.SetMaxOpenConns(3)
	for range 3 {
		var journal string
		var foreignKeys, busyTimeout, userVersion int
		db.QueryRow("PRAGMA journal_mode").Scan(&journal)
		db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys)
		db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout)
		db.QueryRow("PRAGMA user_version").Scan(&userVersion)
		if journal != "wal" || foreignKeys != 1 || busyTimeout < 5000 || userVersion != 1 {
			t.Fatalf("journal=%s foreign_keys=%d busy_timeout=%d user_version=%d", journal, foreignKeys, busyTimeout, userVersion)
		}
	}
	db.Close()

	reopened, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened.Close()
}

func TestCreateFindAndSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess, req, err := store.Create(ctx, "orig", "sys", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !IDPattern.MatchString(sess.PublicID) {
		t.Fatalf("public id = %q", sess.PublicID)
	}

	var status, platform, model string
	var startedAt, deadlineAt int64
	var finishedAt sql.NullInt64
	store.DB.QueryRow(`SELECT status, platform, model, started_at, deadline_at, finished_at FROM requests WHERE id = ?`, req.ID).
		Scan(&status, &platform, &model, &startedAt, &deadlineAt, &finishedAt)
	if status != StatusRunning || platform != "llamacpp" || model != "m" || finishedAt.Valid {
		t.Errorf("request = %s %s %s %v", status, platform, model, finishedAt)
	}
	if deadlineAt-startedAt != 30_000 || req.Deadline.UnixMilli() != deadlineAt {
		t.Errorf("deadline = %d - %d, request deadline %v", deadlineAt, startedAt, req.Deadline)
	}

	found, err := store.Find(ctx, sess.PublicID)
	if err != nil || found.ID != sess.ID || found.SystemPrompt != "sys" {
		t.Fatalf("Find = %+v, %v", found, err)
	}
	if _, err := store.Find(ctx, "00000000"); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("unknown err = %v", err)
	}

	snap, err := store.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]BaseFile{
		OriginalFile: {Writable: false, Content: "orig", HasVersion: true},
		CurrentFile:  {Writable: true, Content: "orig", HasVersion: true},
	}
	if snap.Version != 0 || len(snap.Files) != 2 || snap.Files[OriginalFile] != want[OriginalFile] || snap.Files[CurrentFile] != want[CurrentFile] {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestCreateRetriesPublicIDCollision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ids := []string{"aaaaaaaa", "aaaaaaaa", "aaaaaaaa", "bbbbbbbb"}
	store.NewID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	first, _, err := store.Create(ctx, "a", "s", `{}`)
	if err != nil || first.PublicID != "aaaaaaaa" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, _, err := store.Create(ctx, "b", "s", `{}`)
	if err != nil || second.PublicID != "bbbbbbbb" {
		t.Fatalf("second = %+v, %v", second, err)
	}
}

func TestCommitAndFailVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	sess, req, _ := store.Create(ctx, "orig", "sys", `{}`)

	if err := store.Commit(ctx, sess.ID, req.ID, 0, map[string]string{CurrentFile: "v1", "SCRATCH-1.txt": "notes"}); err != nil {
		t.Fatal(err)
	}

	failed, _ := store.StartRequest(ctx, sess.ID, ToolReviseRewrite, `{}`)
	if err := store.Fail(ctx, sess.ID, failed.ID, map[string]string{CurrentFile: "bad", "SCRATCH-2.txt": "lost"}, "boom"); err != nil {
		t.Fatal(err)
	}

	snap, _ := store.Snapshot(ctx, sess.ID)
	if snap.Version != 1 {
		t.Errorf("version = %d", snap.Version)
	}
	if got := snap.Files[CurrentFile]; got.Content != "v1" {
		t.Errorf("CURRENT = %+v", got)
	}
	if got := snap.Files["SCRATCH-1.txt"]; !got.Writable || got.Content != "notes" {
		t.Errorf("SCRATCH-1 = %+v", got)
	}
	if got := snap.Files["SCRATCH-2.txt"]; got.HasVersion || !got.Writable {
		t.Errorf("SCRATCH-2 from failed run = %+v", got)
	}

	var status, message string
	var finished sql.NullInt64
	store.DB.QueryRow(`SELECT status, error, finished_at FROM requests WHERE id = ?`, failed.ID).Scan(&status, &message, &finished)
	if status != StatusError || message != "boom" || !finished.Valid {
		t.Errorf("failed request = %s %q %v", status, message, finished)
	}
	var versions int
	store.DB.QueryRow(`SELECT COUNT(*) FROM file_versions`).Scan(&versions)
	if versions != 6 {
		t.Errorf("file_versions = %d, want 6", versions)
	}

	stale, _ := store.StartRequest(ctx, sess.ID, ToolReviseRewrite, `{}`)
	if err := store.Commit(ctx, sess.ID, stale.ID, 0, map[string]string{CurrentFile: "stale"}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale commit err = %v", err)
	}
	store.DB.QueryRow(`SELECT status FROM requests WHERE id = ?`, stale.ID).Scan(&status)
	if status != StatusRunning {
		t.Errorf("conflicting commit changed request status to %s", status)
	}
	store.DB.QueryRow(`SELECT COUNT(*) FROM file_versions`).Scan(&versions)
	if versions != 6 {
		t.Errorf("conflicting commit wrote versions: %d", versions)
	}
}

func TestRecordRejectedAndAppendMessage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.RecordRejected(ctx, ToolReviseRewrite, `{"session_id":"x"}`, "invalid"); err != nil {
		t.Fatal(err)
	}
	var sessionID sql.NullInt64
	var started, deadline, finished int64
	var status string
	store.DB.QueryRow(`SELECT session_id, status, started_at, deadline_at, finished_at FROM requests`).
		Scan(&sessionID, &status, &started, &deadline, &finished)
	if sessionID.Valid || status != StatusError || deadline != started || finished != started {
		t.Errorf("rejected row = %v %s %d %d %d", sessionID, status, started, deadline, finished)
	}

	_, req, _ := store.Create(ctx, "t", "s", `{}`)
	calls := `[{"id":"1","name":"read_file","arguments":"{}"}]`
	tokens := int64(7)
	if err := store.AppendMessage(ctx, req.ID, Message{Role: "assistant", ToolCalls: &calls, TotalTokens: &tokens}); err != nil {
		t.Fatal(err)
	}
	var role, content, reasoning string
	var storedCalls sql.NullString
	var prompt, total sql.NullInt64
	store.DB.QueryRow(`SELECT role, content, reasoning, tool_calls, prompt_tokens, total_tokens FROM messages`).
		Scan(&role, &content, &reasoning, &storedCalls, &prompt, &total)
	if role != "assistant" || content != "" || reasoning != "" || storedCalls.String != calls || prompt.Valid || total.Int64 != 7 {
		t.Errorf("message = %s %q %q %v %v %v", role, content, reasoning, storedCalls, prompt, total)
	}
}

func TestLockSerializesSameSession(t *testing.T) {
	store := &Store{}
	unlock := store.Lock("abcdef01")
	acquired := make(chan struct{})
	go func() {
		store.Lock("abcdef01")()
		close(acquired)
	}()
	otherUnlock := store.Lock("12345678")
	otherUnlock()
	select {
	case <-acquired:
		t.Fatal("second lock acquired while first was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-acquired
}
