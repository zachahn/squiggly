package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathUsesXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, _ := Path(); got != "/xdg/editor/config.toml" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, _ := Path(); got != "/home/u/.config/editor/config.toml" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "editor", "config.toml")
	cfg := &Config{
		Platform:     PlatformOpenAICompatible,
		BaseURL:      "https://example.com/v1",
		APIKey:       "secret",
		Model:        "m",
		MaxSteps:     25,
		SystemPrompt: DefaultSystemPrompt,
	}
	if err := Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	dirInfo, _ := os.Stat(filepath.Dir(path))
	fileInfo, _ := os.Stat(path)
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes = %v %v", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `system_prompt = """`) {
		t.Errorf("system_prompt not written as multi-line string:\n%s", data)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *loaded != *cfg {
		t.Fatalf("loaded %+v, want %+v", loaded, cfg)
	}
}

func TestParseDefaultsMaxSteps(t *testing.T) {
	cfg, err := Parse([]byte("platform = \"ollama\"\nbase_url = \"http://x\"\nmodel = \"m\"\nsystem_prompt = \"p\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSteps != 25 {
		t.Fatalf("MaxSteps = %d", cfg.MaxSteps)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{
		"platform":      "platform = \"other\"\nbase_url = \"u\"\nmodel = \"m\"\nsystem_prompt = \"p\"\n",
		"base_url":      "platform = \"ollama\"\nmodel = \"m\"\nsystem_prompt = \"p\"\n",
		"model":         "platform = \"ollama\"\nbase_url = \"u\"\nsystem_prompt = \"p\"\n",
		"system_prompt": "platform = \"ollama\"\nbase_url = \"u\"\nmodel = \"m\"\n",
		"max_steps":     "platform = \"ollama\"\nbase_url = \"u\"\nmodel = \"m\"\nsystem_prompt = \"p\"\nmax_steps = 0\n",
	}
	for field, input := range cases {
		cfg, err := Parse([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	if _, err := Parse([]byte("not toml = = =")); err == nil {
		t.Error("expected parse error")
	}
}
