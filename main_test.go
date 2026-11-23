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

	matches, _ := filepath.Glob(filepath.Join(stateHome, "editor", "sessions", id+"-*"))
	if len(matches) != 1 {
		t.Fatalf("session dirs = %v", matches)
	}
	if data, _ := os.ReadFile(filepath.Join(matches[0], "SYSTEM.txt")); string(data) != "Be concise." {
		t.Errorf("SYSTEM.txt = %q", data)
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
	entries, _ := os.ReadDir(filepath.Join(stateHome, "editor", "sessions"))
	if len(entries) != 1 {
		t.Errorf("failed start left sessions: %v", entries)
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
