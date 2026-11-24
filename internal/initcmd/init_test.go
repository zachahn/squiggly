package initcmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachahn/squiggly/internal/config"
	"github.com/zachahn/squiggly/internal/prose"
)

func runInit(t *testing.T, path, input string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		In:         strings.NewReader(input),
		Out:        &out,
		IsTerminal: true,
		Path:       path,
	})
	return out.String(), err
}

func TestRequiresTerminal(t *testing.T) {
	err := Run(context.Background(), Options{In: strings.NewReader(""), Out: &bytes.Buffer{}, Path: filepath.Join(t.TempDir(), "c.toml")})
	if err != ErrNotTerminal {
		t.Fatalf("err = %v", err)
	}
}

func TestNewConfigWithModelPickList(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"alpha"},{"id":"beta"}]}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "squiggly", "config.toml")
	out, err := runInit(t, path, "3\n\n"+srv.URL+"/v1\nsk-test\n0\n2\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(out, "A value is required.") || !strings.Contains(out, "  2) beta") || !strings.Contains(out, "Wrote "+path) {
		t.Errorf("output:\n%s", out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{Platform: "openai-compatible", BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "beta", MaxSteps: 25, RequestTimeoutSeconds: 600, SystemPrompt: prose.DefaultSystemPrompt}
	if *cfg != want {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestSingleModelIsDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"models":[{"name":"llama3.2:latest"}]}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.toml")
	if out, err := runInit(t, path, "ollama\n"+srv.URL+"\n\n"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "llama3.2:latest" || cfg.APIKey != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestDiscoveryFailureFallsBackToTypedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.toml")
	out, err := runInit(t, path, "2\n"+srv.URL+"/v1\n\nmy-model\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Could not list models") || !strings.Contains(out, "500") || !strings.Contains(out, "A value is required.") {
		t.Errorf("output:\n%s", out)
	}
	cfg, _ := config.Load(path)
	if cfg.Platform != "llamacpp" || cfg.Model != "my-model" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestExistingConfigDeclined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "garbage = = ="
	os.WriteFile(path, []byte(original), 0o600)
	out, err := runInit(t, path, "n\n")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original || !strings.Contains(out, "Overwrite") {
		t.Fatalf("file changed or no prompt: %q\n%s", data, out)
	}
}

func TestExistingConfigPrefillsAndPreserves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.toml")
	existing := &config.Config{Platform: "openai-compatible", BaseURL: srv.URL, APIKey: "old-key", Model: "c", MaxSteps: 7, RequestTimeoutSeconds: 45, SystemPrompt: "custom prompt"}
	if err := config.Write(path, existing); err != nil {
		t.Fatal(err)
	}
	out, err := runInit(t, path, "y\n\n\n\n\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Platform [openai-compatible]") || !strings.Contains(out, "Base URL ["+srv.URL+"]") || !strings.Contains(out, "Model [3]") {
		t.Errorf("prompts not pre-filled:\n%s", out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *cfg != *existing {
		t.Fatalf("cfg = %+v, want %+v", cfg, existing)
	}
}

func TestUnparseableExistingConfigUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("garbage = = ="), 0o600)
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	out, err := runInit(t, path, "y\n1\n"+unreachable.URL+"\nm\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "could not be parsed") || !strings.Contains(out, "Base URL [http://localhost:11434]") {
		t.Errorf("output:\n%s", out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SystemPrompt != prose.DefaultSystemPrompt || cfg.MaxSteps != 25 || cfg.RequestTimeoutSeconds != 600 {
		t.Fatalf("cfg = %+v", cfg)
	}
}
