package prune

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zachahn/squiggly/internal/session"
)

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":   30 * 24 * time.Hour,
		"12h":   12 * time.Hour,
		"90m":   90 * time.Minute,
		"1d12h": 36 * time.Hour,
		"1.5d":  36 * time.Hour,
	}
	for input, want := range cases {
		got, err := ParseDuration(input)
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "d", "abc", "-1d", "5x"} {
		if _, err := ParseDuration(input); err == nil {
			t.Errorf("%q: expected error", input)
		}
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := session.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(db, session.RequestSettings{Platform: "ollama", Model: "m", MaxSteps: 1, RequestTimeoutSeconds: 1})
	now := time.UnixMilli(10 * 3600 * 1000)
	at := func(hoursAgo float64) {
		store.Now = func() time.Time { return now.Add(-time.Duration(hoursAgo * float64(time.Hour))) }
	}

	create := func(publicID string, createdHoursAgo float64) *session.Session {
		at(createdHoursAgo)
		store.NewID = func() (string, error) { return publicID, nil }
		sess, req, err := store.Create(ctx, "t", "s", "{}")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(ctx, req.ID, session.Message{Role: "user"}); err != nil {
			t.Fatal(err)
		}
		if err := store.Commit(ctx, sess.ID, req.ID, 0, map[string]string{"CURRENT.txt": "r"}); err != nil {
			t.Fatal(err)
		}
		return sess
	}
	create("aaaaaaaa", 5)
	revived := create("bbbbbbbb", 5)
	create("cccccccc", 0.5)
	create("dddddddd", 3)

	at(0.25)
	if _, err := store.StartRequest(ctx, revived.ID, session.ToolReviseRewrite, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM requests WHERE session_id = (SELECT id FROM sessions WHERE public_id = 'dddddddd')`); err != nil {
		t.Fatal(err)
	}
	at(2)
	store.RecordRejected(ctx, session.ToolReviseRewrite, "{}", "old rejection")
	at(0.1)
	store.RecordRejected(ctx, session.ToolReviseRewrite, "{}", "new rejection")
	db.Close()

	var out bytes.Buffer
	if err := Run(ctx, path, time.Hour, now, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "aaaaaaaa\ndddddddd\nDeleted 2 sessions.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	db, err = session.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	counts := map[string]int{}
	for _, table := range []string{"sessions", "files", "file_versions", "requests", "messages"} {
		var n int
		db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		counts[table] = n
	}
	want := map[string]int{"sessions": 2, "files": 4, "file_versions": 6, "requests": 4, "messages": 2}
	for table, n := range want {
		if counts[table] != n {
			t.Errorf("%s = %d, want %d", table, counts[table], n)
		}
	}
	var rejected string
	db.QueryRow(`SELECT error FROM requests WHERE session_id IS NULL`).Scan(&rejected)
	if rejected != "new rejection" {
		t.Errorf("remaining rejection = %q", rejected)
	}
}

func TestRunMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "sessions.db")
	var out bytes.Buffer
	if err := Run(context.Background(), path, time.Hour, time.Now(), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Deleted 0 sessions.\n" {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database directory was created: %v", err)
	}
}
