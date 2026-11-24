package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zachahn/editor/internal/config"
	"github.com/zachahn/editor/internal/session"
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

func newSnapshot(original string) *session.Snapshot {
	return &session.Snapshot{Files: map[string]session.BaseFile{
		session.OriginalFile: {Writable: false, Content: original, HasVersion: true},
		session.CurrentFile:  {Writable: true, Content: original, HasVersion: true},
	}}
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

func input(snapshot *session.Snapshot, systemPrompt, userMessage string) RunInput {
	return RunInput{Snapshot: snapshot, SystemPrompt: systemPrompt, UserMessage: userMessage}
}

func TestRunReturnsOverlayChanges(t *testing.T) {
	snapshot := newSnapshot("hello world")
	m := &scriptedModel{replies: []*schema.Message{
		call("write_file", map[string]any{"name": "SCRATCH-1.txt", "content": "notes"}),
		call("edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "world", "new_string": "there"}),
		done(),
	}}
	r := &Runner{Model: m, MaxSteps: 5}
	got, err := r.Run(context.Background(), input(snapshot, "be concise", "Rewrite CURRENT.txt."))
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "hello there" {
		t.Fatalf("result = %q", got.Current)
	}
	if len(got.Changed) != 2 || got.Changed["CURRENT.txt"] != "hello there" || got.Changed["SCRATCH-1.txt"] != "notes" {
		t.Fatalf("changed = %v", got.Changed)
	}
	if snapshot.Files[session.CurrentFile].Content != "hello world" {
		t.Fatal("base snapshot was modified")
	}
	system := m.requests[0][0]
	if system.Role != schema.System || !strings.HasPrefix(system.Content, Preamble) || !strings.HasSuffix(system.Content, "be concise") {
		t.Fatalf("system message = %q", system.Content)
	}
	if m.requests[0][1].Content != "Rewrite CURRENT.txt." {
		t.Fatalf("user message = %q", m.requests[0][1].Content)
	}
}

func TestRunRecordsTranscriptAsProduced(t *testing.T) {
	reply := call("read_file", map[string]any{"name": "CURRENT.txt"})
	reply.ReasoningContent = "look first"
	reply.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}}
	final := schema.AssistantMessage("finished", nil)
	m := &scriptedModel{replies: []*schema.Message{reply, final}}

	var recorded []session.Message
	in := input(newSnapshot("text"), "p", "go")
	in.Record = func(_ context.Context, msg *schema.Message) error {
		record, err := TranscriptMessage(msg)
		recorded = append(recorded, record)
		return err
	}
	if _, err := (&Runner{Model: m, MaxSteps: 5}).Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	var roles []string
	for _, msg := range recorded {
		roles = append(roles, msg.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,assistant" {
		t.Fatalf("roles = %v", roles)
	}
	assistant := recorded[2]
	if assistant.Reasoning != "look first" || assistant.ToolCalls == nil || *assistant.TotalTokens != 12 || *assistant.PromptTokens != 10 || *assistant.CompletionTokens != 2 {
		t.Errorf("assistant = %+v", assistant)
	}
	var calls []map[string]string
	json.Unmarshal([]byte(*assistant.ToolCalls), &calls)
	if len(calls) != 1 || calls[0]["name"] != "read_file" || calls[0]["arguments"] != `{"name":"CURRENT.txt"}` || calls[0]["id"] == "" {
		t.Errorf("tool_calls = %s", *assistant.ToolCalls)
	}
	tool := recorded[3]
	if tool.Content != "text" || *tool.ToolName != "read_file" || *tool.ToolCallID != calls[0]["id"] || tool.ToolCalls != nil || tool.TotalTokens != nil {
		t.Errorf("tool = %+v", tool)
	}
	if last := recorded[4]; last.Content != "finished" || last.ToolCalls != nil || last.TotalTokens != nil {
		t.Errorf("final = %+v", last)
	}
	if recorded[0].ToolCallID != nil || recorded[1].ToolName != nil {
		t.Errorf("system/user tool fields = %+v %+v", recorded[0], recorded[1])
	}
}

func TestRunFailsWhenRecordingFails(t *testing.T) {
	m := &scriptedModel{replies: []*schema.Message{done()}}
	in := input(newSnapshot("text"), "p", "go")
	in.Record = func(context.Context, *schema.Message) error { return errors.New("disk full") }
	_, err := (&Runner{Model: m, MaxSteps: 5}).Run(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
}

type blockingModel struct{ scriptedModel }

func (m *blockingModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestRunFailsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := (&Runner{Model: &blockingModel{}, MaxSteps: 5}).Run(ctx, input(newSnapshot("text"), "p", "go"))
	if !errors.Is(err, ErrDeadlineReached) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunStepLimit(t *testing.T) {
	read := call("read_file", map[string]any{"name": "CURRENT.txt"})

	t.Run("finishing on the last allowed step succeeds", func(t *testing.T) {
		m := &scriptedModel{replies: []*schema.Message{read, read, done()}}
		r := &Runner{Model: m, MaxSteps: 3}
		if _, err := r.Run(context.Background(), input(newSnapshot("text"), "p", "go")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("exceeding the limit fails and returns the overlay changes", func(t *testing.T) {
		write := call("write_file", map[string]any{"name": "CURRENT.txt", "content": "changed"})
		m := &scriptedModel{replies: []*schema.Message{write, read, read, done()}}
		r := &Runner{Model: m, MaxSteps: 3}
		got, err := r.Run(context.Background(), input(newSnapshot("text"), "p", "go"))
		if err == nil || !strings.Contains(err.Error(), "max_steps") {
			t.Fatalf("err = %v", err)
		}
		if m.calls != 3 {
			t.Fatalf("model calls = %d, want 3", m.calls)
		}
		if got.Changed["CURRENT.txt"] != "changed" {
			t.Fatalf("changed = %v", got.Changed)
		}
	})
}

func TestRunEmptyCurrentFails(t *testing.T) {
	m := &scriptedModel{replies: []*schema.Message{
		call("write_file", map[string]any{"name": "CURRENT.txt", "content": "  \n"}),
		done(),
	}}
	r := &Runner{Model: m, MaxSteps: 5}
	got, err := r.Run(context.Background(), input(newSnapshot("text"), "p", "go"))
	if !errors.Is(err, ErrEmptyResult) {
		t.Fatalf("err = %v", err)
	}
	if got.Changed["CURRENT.txt"] != "  \n" {
		t.Fatalf("changed = %v", got.Changed)
	}
}

func TestRunToolErrorsDoNotFailRun(t *testing.T) {
	m := &scriptedModel{replies: []*schema.Message{
		call("read_file", map[string]any{"name": "../SYSTEM.txt"}),
		call("write_file", map[string]any{"name": "ORIGINAL.txt", "content": "x"}),
		call("edit_file", map[string]any{"name": "CURRENT.txt", "old_string": "a", "new_string": "c"}),
		call("nonexistent", map[string]any{}),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "bad", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: "{not json"}}}},
		done(),
	}}
	r := &Runner{Model: m, MaxSteps: 10}
	if _, err := r.Run(context.Background(), input(newSnapshot("a a b"), "p", "go")); err != nil {
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

func TestNewChatModelUsesConfiguredRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)

	cfg := &config.Config{Platform: config.PlatformLlamaCpp, BaseURL: slow.URL + "/v1", Model: "m", RequestTimeoutSeconds: 1}
	chatModel, err := NewChatModel(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("request ended after %v, want about 1s", elapsed)
	}
}
