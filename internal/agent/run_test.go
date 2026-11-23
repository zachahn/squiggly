package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type scriptedModel struct {
	mu       sync.Mutex
	replies  []*schema.Message
	calls    int
	requests [][]*schema.Message
}

func (m *scriptedModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, input)
	if m.calls >= len(m.replies) {
		return nil, errors.New("script exhausted")
	}
	reply := m.replies[m.calls]
	m.calls++
	return reply, nil
}

func (m *scriptedModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *scriptedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func call(name string, args map[string]any) *schema.Message {
	encoded, _ := json.Marshal(args)
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       fmt.Sprintf("call-%s-%s", name, encoded),
		Type:     "function",
		Function: schema.FunctionCall{Name: name, Arguments: string(encoded)},
	}})
}

func done() *schema.Message {
	return schema.AssistantMessage("finished", nil)
}

func newSessionDir(t *testing.T, original string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"ORIGINAL.txt": original, "CURRENT.txt": original, "SYSTEM.txt": "sys"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func toolResults(req []*schema.Message) []string {
	var results []string
	for _, msg := range req {
		if msg.Role == schema.Tool {
			results = append(results, msg.Content)
		}
	}
	return results
}

func TestRunCommitsOverlay(t *testing.T) {
	dir := newSessionDir(t, "hello world")
	m := &scriptedModel{replies: []*schema.Message{
		call("write_file", map[string]any{"name": "SCRATCH-1.txt", "content": "notes"}),
		call("edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "world", "new_string": "there"}),
		done(),
	}}
	r := &Runner{Model: m, MaxSteps: 5}
	got, err := r.Run(context.Background(), dir, "be concise", "Rewrite CURRENT.txt.")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello there" {
		t.Fatalf("result = %q", got)
	}
	if readFile(t, dir, "CURRENT.txt") != "hello there" || readFile(t, dir, "SCRATCH-1.txt") != "notes" {
		t.Fatal("files were not committed")
	}
	info, err := os.Stat(filepath.Join(dir, "SCRATCH-1.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("scratch mode = %v, %v", info.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CURRENT.txt.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary file left behind")
	}
	system := m.requests[0][0]
	if system.Role != schema.System || !strings.HasPrefix(system.Content, Preamble) || !strings.HasSuffix(system.Content, "be concise") {
		t.Fatalf("system message = %q", system.Content)
	}
	if m.requests[0][1].Content != "Rewrite CURRENT.txt." {
		t.Fatalf("user message = %q", m.requests[0][1].Content)
	}
}

func TestRunStepLimit(t *testing.T) {
	read := call("read_file", map[string]any{"name": "CURRENT.txt"})

	t.Run("finishing on the last allowed step succeeds", func(t *testing.T) {
		dir := newSessionDir(t, "text")
		m := &scriptedModel{replies: []*schema.Message{read, read, done()}}
		r := &Runner{Model: m, MaxSteps: 3}
		if _, err := r.Run(context.Background(), dir, "p", "go"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("exceeding the limit fails and discards the overlay", func(t *testing.T) {
		dir := newSessionDir(t, "text")
		write := call("write_file", map[string]any{"name": "CURRENT.txt", "content": "changed"})
		m := &scriptedModel{replies: []*schema.Message{write, read, read, done()}}
		r := &Runner{Model: m, MaxSteps: 3}
		_, err := r.Run(context.Background(), dir, "p", "go")
		if err == nil || !strings.Contains(err.Error(), "max_steps") {
			t.Fatalf("err = %v", err)
		}
		if m.calls != 3 {
			t.Fatalf("model calls = %d, want 3", m.calls)
		}
		if readFile(t, dir, "CURRENT.txt") != "text" {
			t.Fatal("disk was modified by a failed run")
		}
	})
}

func TestRunEmptyCurrentFails(t *testing.T) {
	dir := newSessionDir(t, "text")
	m := &scriptedModel{replies: []*schema.Message{
		call("write_file", map[string]any{"name": "CURRENT.txt", "content": "  \n"}),
		done(),
	}}
	r := &Runner{Model: m, MaxSteps: 5}
	if _, err := r.Run(context.Background(), dir, "p", "go"); !errors.Is(err, ErrEmptyResult) {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, dir, "CURRENT.txt") != "text" {
		t.Fatal("disk was modified by a failed run")
	}
}

func TestRunToolErrorsDoNotFailRun(t *testing.T) {
	dir := newSessionDir(t, "a a b")
	m := &scriptedModel{replies: []*schema.Message{
		call("read_file", map[string]any{"name": "../SYSTEM.txt"}),
		call("write_file", map[string]any{"name": "ORIGINAL.txt", "content": "x"}),
		call("edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "a", "new_string": "c"}),
		call("nonexistent", map[string]any{}),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "bad", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: "{not json"}}}},
		done(),
	}}
	r := &Runner{Model: m, MaxSteps: 10}
	if _, err := r.Run(context.Background(), dir, "p", "go"); err != nil {
		t.Fatal(err)
	}
	results := toolResults(m.requests[len(m.requests)-1])
	if len(results) != 5 {
		t.Fatalf("tool results = %q", results)
	}
	for i, want := range []string{"not allowed", "read-only", "2 occurrences", "unknown tool", "invalid arguments"} {
		if !strings.HasPrefix(results[i], "error: ") || !strings.Contains(results[i], want) {
			t.Errorf("result %d = %q, want error containing %q", i, results[i], want)
		}
	}
}
