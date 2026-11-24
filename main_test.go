package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zachahn/editor/internal/session"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "editor")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

type fakeOpenAI struct {
	mu       sync.Mutex
	auth     []string
	requests int
	replies  []map[string]any
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	reply := f.replies[f.requests%len(f.replies)]
	f.requests++
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("cmpl-%d", f.requests),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "fake",
		"choices": []map[string]any{{"index": 0, "message": reply, "finish_reason": "stop"}},
	})
}

func toolCallReply(name, args string) map[string]any {
	return map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []map[string]any{{
			"id":       "call-" + name,
			"type":     "function",
			"function": map[string]any{"name": name, "arguments": args},
		}},
	}
}

func writeConfig(t *testing.T, configHome, body string) {
	t.Helper()
	dir := filepath.Join(configHome, "editor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func callText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content blocks = %d", len(res.Content))
	}
	return res.Content[0].(mcp.TextContent).Text
}

func TestMCPEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	fake := &fakeOpenAI{replies: []map[string]any{
		toolCallReply("edit_file", `{"name":"CURRENT.txt","old_string":"utilize","new_string":"use"}`),
		{"role": "assistant", "content": "done"},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	configHome, stateHome := t.TempDir(), t.TempDir()
	writeConfig(t, configHome, fmt.Sprintf("platform = \"openai-compatible\"\nbase_url = %q\napi_key = \"sk-test\"\nmodel = \"fake\"\nsystem_prompt = \"Be concise.\"\n", srv.URL+"/v1"))

	env := append(os.Environ(), "XDG_CONFIG_HOME="+configHome, "XDG_STATE_HOME="+stateHome)
	c, err := client.NewStdioMCPClient(bin, env, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatal(err)
	}

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "revise_rewrite,start_rewrite" && strings.Join(names, ",") != "start_rewrite,revise_rewrite" {
		t.Fatalf("tools = %v", names)
	}

	var start mcp.CallToolRequest
	start.Params.Name = "start_rewrite"
	start.Params.Arguments = map[string]any{"text": "We utilize tools."}
	res, err := c.CallTool(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal(callText(t, res))
	}
	header, body, _ := strings.Cut(callText(t, res), "\n\n")
	id := strings.TrimPrefix(header, "session_id: ")
	if len(id) != 8 || body != "We use tools." {
		t.Fatalf("start result = %q", callText(t, res))
	}
	if fake.auth[0] != "Bearer sk-test" {
		t.Errorf("Authorization = %q", fake.auth[0])
	}

	dbPath := filepath.Join(stateHome, "editor", "sessions.db")
	if info, err := os.Stat(dbPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database = %v, %v", info, err)
	}
	db, err := session.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var systemPrompt string
	if err := db.QueryRow(`SELECT system_prompt FROM sessions WHERE public_id = ?`, id).Scan(&systemPrompt); err != nil || systemPrompt != "Be concise." {
		t.Errorf("system_prompt = %q, %v", systemPrompt, err)
	}

	fake.mu.Lock()
	fake.replies = []map[string]any{
		toolCallReply("write_file", `{"name":"CURRENT.txt","content":"Tools are used."}`),
		{"role": "assistant", "content": "done"},
	}
	fake.requests = 0
	fake.mu.Unlock()

	var revise mcp.CallToolRequest
	revise.Params.Name = "revise_rewrite"
	revise.Params.Arguments = map[string]any{"session_id": id, "feedback": "Use passive voice."}
	res, err = c.CallTool(ctx, revise)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || callText(t, res) != "Tools are used." {
		t.Fatalf("revise result = %+v", res)
	}

	revise.Params.Arguments = map[string]any{"session_id": "deadbeef", "feedback": "x"}
	res, err = c.CallTool(ctx, revise)
	if err != nil || !res.IsError {
		t.Fatalf("unknown session result = %+v, %v", res, err)
	}

	srv.Close()
	res, err = c.CallTool(ctx, start)
	if err != nil || !res.IsError {
		t.Fatalf("unreachable platform result = %+v, %v", res, err)
	}
	failedID := strings.TrimPrefix(strings.SplitN(callText(t, res), "\n", 2)[0], "session_id: ")
	if len(failedID) != 8 || failedID == id {
		t.Errorf("failed start message = %q", callText(t, res))
	}

	var summary []string
	rows, err := db.Query(`SELECT r.tool, r.status, COALESCE(s.public_id, ''), (SELECT COUNT(*) FROM messages m WHERE m.request_id = r.id)
		FROM requests r LEFT JOIN sessions s ON s.id = r.session_id ORDER BY r.id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tool, status, publicID string
		var messages int
		rows.Scan(&tool, &status, &publicID, &messages)
		summary = append(summary, fmt.Sprintf("%s %s %v %d", tool, status, publicID == id, messages))
	}
	rows.Close()
	want := []string{
		"start_rewrite ok true 5",
		"revise_rewrite ok true 5",
		"revise_rewrite error false 0",
		"start_rewrite error false 2",
	}
	if strings.Join(summary, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(summary, "\n"), strings.Join(want, "\n"))
	}
	if _, err := c.ListTools(ctx, mcp.ListToolsRequest{}); err != nil {
		t.Fatalf("server stopped after tool error: %v", err)
	}
}

func TestMCPInvalidConfig(t *testing.T) {
	bin := buildBinary(t)
	configHome := t.TempDir()
	writeConfig(t, configHome, "platform = \"bogus\"\n")
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+configHome)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil || !strings.Contains(stderr.String(), "editor init") {
		t.Fatalf("err = %v, stderr = %q", err, stderr.String())
	}
}

func TestPruneRequiresFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"prune"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--older-than") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestInitRejectsNonTerminal(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "init")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "terminal") {
		t.Fatalf("err = %v, stderr = %q", err, stderr.String())
	}
}

func TestPruneMissingDatabaseCreatesNothing(t *testing.T) {
	bin := buildBinary(t)
	stateHome := t.TempDir()
	cmd := exec.Command(bin, "prune", "--older-than", "30d")
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
	out, err := cmd.Output()
	if err != nil || string(out) != "Deleted 0 sessions.\n" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if entries, _ := os.ReadDir(stateHome); len(entries) != 0 {
		t.Fatalf("state directory = %v", entries)
	}
}
