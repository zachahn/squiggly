package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"github.com/zachahn/editor/internal/session"
)

func invoke(t *testing.T, tools []tool.BaseTool, name string, args map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, _ := candidate.Info(context.Background())
		if info.Name == name {
			result, err := candidate.(tool.InvokableTool).InvokableRun(context.Background(), string(encoded))
			if err != nil {
				t.Fatalf("%s returned a Go error: %v", name, err)
			}
			return result
		}
	}
	t.Fatalf("tool %s not found", name)
	return ""
}

func TestTools(t *testing.T) {
	snapshot := newSnapshot("one\ntwo\nthree\n")
	snapshot.Files["SCRATCH-5.txt"] = session.BaseFile{Writable: true}
	overlay := NewOverlay(snapshot)
	tools := NewTools(overlay)

	if got := invoke(t, tools, "write_file", map[string]any{"name": "SCRATCH-10.txt", "content": "two notes"}); strings.HasPrefix(got, "error") {
		t.Fatal(got)
	}
	invoke(t, tools, "write_file", map[string]any{"name": "SCRATCH-2.txt", "content": ""})

	if got, want := invoke(t, tools, "list_files", nil), "ORIGINAL.txt 14\nCURRENT.txt 14\nSCRATCH-2.txt 0\nSCRATCH-10.txt 9\n"; got != want {
		t.Errorf("list_files = %q, want %q", got, want)
	}
	if got := snapshot.Files["CURRENT.txt"].Content; got != "one\ntwo\nthree\n" {
		t.Errorf("base snapshot changed during run: %q", got)
	}

	if got, want := invoke(t, tools, "grep", map[string]any{"pattern": "two"}), "ORIGINAL.txt:2:two\nCURRENT.txt:2:two\nSCRATCH-10.txt:1:two notes\n"; got != want {
		t.Errorf("grep = %q, want %q", got, want)
	}
	if got, want := invoke(t, tools, "grep", map[string]any{"pattern": "^t", "name": "CURRENT.txt"}), "CURRENT.txt:2:two\nCURRENT.txt:3:three\n"; got != want {
		t.Errorf("grep with name = %q, want %q", got, want)
	}
	if got := invoke(t, tools, "grep", map[string]any{"pattern": "("}); !strings.Contains(got, "invalid pattern") {
		t.Errorf("grep invalid = %q", got)
	}
	if got := invoke(t, tools, "grep", map[string]any{"pattern": "x", "name": "SYSTEM.txt"}); !strings.Contains(got, "not allowed") {
		t.Errorf("grep SYSTEM.txt = %q", got)
	}

	if got := invoke(t, tools, "read_file", map[string]any{"name": "SCRATCH-3.txt"}); !strings.Contains(got, "does not exist") {
		t.Errorf("read missing = %q", got)
	}
	for _, name := range []string{"SYSTEM.txt", "./CURRENT.txt", "sub/CURRENT.txt", "CURRENT.txt.tmp", "SCRATCH-.txt", "SCRATCH-1.txt\n"} {
		if got := invoke(t, tools, "read_file", map[string]any{"name": name}); !strings.Contains(got, "not allowed") {
			t.Errorf("read %q = %q", name, got)
		}
	}

	if got := invoke(t, tools, "edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "", "new_string": "x"}); !strings.Contains(got, "must not be empty") {
		t.Errorf("empty old_string = %q", got)
	}
	if got := invoke(t, tools, "edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "four", "new_string": "x"}); !strings.Contains(got, "0 occurrences") {
		t.Errorf("missing old_string = %q", got)
	}
	if got := invoke(t, tools, "edit_file", map[string]any{"name": "ORIGINAL.txt", "old_string": "one", "new_string": "x"}); !strings.Contains(got, "read-only") {
		t.Errorf("edit ORIGINAL = %q", got)
	}
	if got := invoke(t, tools, "edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "t", "new_string": "x"}); !strings.Contains(got, "2 occurrences") {
		t.Errorf("ambiguous old_string = %q", got)
	}
	if got := invoke(t, tools, "edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "two", "new_string": "2"}); strings.HasPrefix(got, "error") {
		t.Fatal(got)
	}
	if got := invoke(t, tools, "read_file", map[string]any{"name": "CURRENT.txt"}); got != "one\n2\nthree\n" {
		t.Errorf("read after edit = %q", got)
	}
	if got := invoke(t, tools, "write_file", map[string]any{"name": "CURRENT.txt"}); !strings.Contains(got, "content") {
		t.Errorf("write without content = %q", got)
	}
	if got := invoke(t, tools, "read_file", map[string]any{"name": "SCRATCH-5.txt"}); !strings.Contains(got, "does not exist") {
		t.Errorf("read scratch without a current version = %q", got)
	}
	if got := invoke(t, tools, "write_file", map[string]any{"name": "SCRATCH-5.txt", "content": "again"}); strings.HasPrefix(got, "error") {
		t.Errorf("rewrite scratch without a current version = %q", got)
	}
}

func TestWritableFlagComesFromSnapshot(t *testing.T) {
	snapshot := newSnapshot("text")
	snapshot.Files["SCRATCH-1.txt"] = session.BaseFile{Writable: false, Content: "locked", HasVersion: true}
	tools := NewTools(NewOverlay(snapshot))
	if got := invoke(t, tools, "write_file", map[string]any{"name": "SCRATCH-1.txt", "content": "x"}); !strings.Contains(got, "read-only") {
		t.Errorf("write read-only scratch = %q", got)
	}
	if got := invoke(t, tools, "edit_file", map[string]any{"name": "SCRATCH-1.txt", "old_string": "locked", "new_string": "x"}); !strings.Contains(got, "read-only") {
		t.Errorf("edit read-only scratch = %q", got)
	}

	withoutCurrent := &session.Snapshot{Files: map[string]session.BaseFile{}}
	if got := invoke(t, NewTools(NewOverlay(withoutCurrent)), "write_file", map[string]any{"name": "CURRENT.txt", "content": "x"}); !strings.Contains(got, "cannot be created") {
		t.Errorf("create CURRENT.txt = %q", got)
	}
}

func TestCountOccurrencesCountsOverlaps(t *testing.T) {
	if n := countOccurrences("aaa", "aa"); n != 2 {
		t.Fatalf("got %d", n)
	}
}
