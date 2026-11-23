package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const (
	PlatformOllama           = "ollama"
	PlatformLlamaCpp         = "llamacpp"
	PlatformOpenAICompatible = "openai-compatible"

	DefaultMaxSteps = 25
)

var Platforms = []string{PlatformOllama, PlatformLlamaCpp, PlatformOpenAICompatible}

const DefaultSystemPrompt = `Rewrite the text for clarity and concision. Preserve the meaning, tone, and any technical details. Do not add new information. Do not imagine a scenario; restate factually.

- Use American English.
- Avoid anthropomorphizing inanimate/software objects.
- Avoid using the present tense verb "names" and "carries" as it's ambiguous and anthropomorphizing.
- Avoid the phrase "$verb it".

<example>
<summary>Avoid the phrase "$verb it". Avoid anthropomorphizing inanimate/software objects.</summary>
<comparison>
- BAD: ... before the bank settled it
- GOOD: ... before it is settled by the bank
</comparison>
</example>
`

type Config struct {
	Platform     string `toml:"platform"`
	BaseURL      string `toml:"base_url"`
	APIKey       string `toml:"api_key"`
	Model        string `toml:"model"`
	MaxSteps     int    `toml:"max_steps"`
	SystemPrompt string `toml:"system_prompt,multiline"`
}

type fileConfig struct {
	Platform     string `toml:"platform"`
	BaseURL      string `toml:"base_url"`
	APIKey       string `toml:"api_key"`
	Model        string `toml:"model"`
	MaxSteps     *int   `toml:"max_steps"`
	SystemPrompt string `toml:"system_prompt"`
}

func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "editor"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "editor"), nil
}

func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

func IsValidPlatform(platform string) bool {
	for _, known := range Platforms {
		if platform == known {
			return true
		}
	}
	return false
}

func Parse(data []byte) (*Config, error) {
	var raw fileConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg := &Config{
		Platform:     raw.Platform,
		BaseURL:      raw.BaseURL,
		APIKey:       raw.APIKey,
		Model:        raw.Model,
		MaxSteps:     DefaultMaxSteps,
		SystemPrompt: raw.SystemPrompt,
	}
	if raw.MaxSteps != nil {
		cfg.MaxSteps = *raw.MaxSteps
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	var problems []error
	if !IsValidPlatform(c.Platform) {
		problems = append(problems, fmt.Errorf("unknown platform %q (expected one of ollama, llamacpp, openai-compatible)", c.Platform))
	}
	if c.BaseURL == "" {
		problems = append(problems, errors.New("base_url is empty"))
	}
	if c.Model == "" {
		problems = append(problems, errors.New("model is empty"))
	}
	if c.SystemPrompt == "" {
		problems = append(problems, errors.New("system_prompt is empty"))
	}
	if c.MaxSteps < 1 {
		problems = append(problems, fmt.Errorf("max_steps is %d but must be at least 1", c.MaxSteps))
	}
	return errors.Join(problems...)
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func Write(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("set config directory mode: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat config directory: %w", err)
	}

	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set config mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
