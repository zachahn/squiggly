package initcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/zachahn/editor/internal/config"
)

var ErrNotTerminal = errors.New("editor init is interactive and requires stdin to be a terminal")

var defaultBaseURLs = map[string]string{
	config.PlatformOllama:   "http://localhost:11434",
	config.PlatformLlamaCpp: "http://localhost:8080/v1",
}

type Options struct {
	In         io.Reader
	Out        io.Writer
	IsTerminal bool
	ReadSecret func() (string, error)
	Path       string
	HTTPClient *http.Client
}

type prompter struct {
	in         *bufio.Reader
	out        io.Writer
	readSecret func() (string, error)
}

func (p *prompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		if errors.Is(err, io.EOF) {
			return "", errors.New("input ended before all prompts were answered")
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (p *prompter) ask(label, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, fallback)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	answer, err := p.readLine()
	if err != nil {
		return "", err
	}
	if answer == "" {
		return fallback, nil
	}
	return answer, nil
}

func (p *prompter) askRequired(label, fallback string) (string, error) {
	for {
		answer, err := p.ask(label, fallback)
		if err != nil {
			return "", err
		}
		if answer != "" {
			return answer, nil
		}
		fmt.Fprintln(p.out, "A value is required.")
	}
}

func (p *prompter) confirm(label string) (bool, error) {
	for {
		fmt.Fprintf(p.out, "%s [y/N]: ", label)
		answer, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			return true, nil
		case "", "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.out, "Please answer y or n.")
	}
}

func (p *prompter) pick(label string, choices []string, defaultIndex int) (string, error) {
	for i, choice := range choices {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, choice)
	}
	fallback := ""
	if defaultIndex >= 0 {
		fallback = strconv.Itoa(defaultIndex + 1)
	}
	for {
		answer, err := p.ask(label, fallback)
		if err != nil {
			return "", err
		}
		n, convErr := strconv.Atoi(answer)
		if convErr == nil && n >= 1 && n <= len(choices) {
			return choices[n-1], nil
		}
		fmt.Fprintf(p.out, "Enter a number from 1 to %d.\n", len(choices))
	}
}

func (p *prompter) askPlatform(fallback string) (string, error) {
	for i, platform := range config.Platforms {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, platform)
	}
	for {
		answer, err := p.ask("Platform", fallback)
		if err != nil {
			return "", err
		}
		if config.IsValidPlatform(answer) {
			return answer, nil
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(config.Platforms) {
			return config.Platforms[n-1], nil
		}
		fmt.Fprintf(p.out, "Enter a number from 1 to %d or a platform name.\n", len(config.Platforms))
	}
}

func (p *prompter) askAPIKey(existing string) (string, error) {
	if existing != "" {
		fmt.Fprint(p.out, "API key [press Enter to keep the existing key]: ")
	} else {
		fmt.Fprint(p.out, "API key (optional): ")
	}
	var answer string
	var err error
	if p.readSecret != nil {
		answer, err = p.readSecret()
		fmt.Fprintln(p.out)
	} else {
		answer, err = p.readLine()
	}
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return existing, nil
	}
	return answer, nil
}

func indexOf(items []string, target string) int {
	for i, item := range items {
		if item == target {
			return i
		}
	}
	return -1
}

func Run(ctx context.Context, opts Options) error {
	if !opts.IsTerminal {
		return ErrNotTerminal
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	p := &prompter{in: bufio.NewReader(opts.In), out: opts.Out, readSecret: opts.ReadSecret}

	var existing *config.Config
	data, err := os.ReadFile(opts.Path)
	switch {
	case err == nil:
		overwrite, err := p.confirm(fmt.Sprintf("A config file already exists at %s. Overwrite it?", opts.Path))
		if err != nil {
			return err
		}
		if !overwrite {
			fmt.Fprintln(opts.Out, "The config file was left unchanged.")
			return nil
		}
		parsed, parseErr := config.Parse(data)
		if parseErr != nil {
			fmt.Fprintf(opts.Out, "The existing config could not be parsed (%v). Defaults are used instead.\n", parseErr)
		} else {
			existing = parsed
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("read config: %w", err)
	}

	cfg := &config.Config{SystemPrompt: config.DefaultSystemPrompt, MaxSteps: config.DefaultMaxSteps}
	previous := &config.Config{}
	if existing != nil {
		previous = existing
		if existing.SystemPrompt != "" {
			cfg.SystemPrompt = existing.SystemPrompt
		}
		if existing.MaxSteps >= 1 {
			cfg.MaxSteps = existing.MaxSteps
		}
	}

	platformFallback := ""
	if config.IsValidPlatform(previous.Platform) {
		platformFallback = previous.Platform
	}
	cfg.Platform, err = p.askPlatform(platformFallback)
	if err != nil {
		return err
	}
	samePlatform := cfg.Platform == previous.Platform

	baseURLFallback := defaultBaseURLs[cfg.Platform]
	if samePlatform && previous.BaseURL != "" {
		baseURLFallback = previous.BaseURL
	}
	cfg.BaseURL, err = p.askRequired("Base URL", baseURLFallback)
	if err != nil {
		return err
	}

	if cfg.Platform == config.PlatformOpenAICompatible {
		existingKey := ""
		if samePlatform {
			existingKey = previous.APIKey
		}
		cfg.APIKey, err = p.askAPIKey(existingKey)
		if err != nil {
			return err
		}
	}

	modelFallback := ""
	if samePlatform {
		modelFallback = previous.Model
	}
	models, discoverErr := DiscoverModels(ctx, client, cfg.Platform, cfg.BaseURL, cfg.APIKey)
	if discoverErr == nil && len(models) == 0 {
		discoverErr = errors.New("the platform returned no models")
	}
	if discoverErr != nil {
		fmt.Fprintf(opts.Out, "Could not list models: %v\n", discoverErr)
		cfg.Model, err = p.askRequired("Model name", modelFallback)
	} else {
		defaultIndex := indexOf(models, modelFallback)
		if defaultIndex < 0 && len(models) == 1 {
			defaultIndex = 0
		}
		fmt.Fprintln(opts.Out, "Available models:")
		cfg.Model, err = p.pick("Model", models, defaultIndex)
	}
	if err != nil {
		return err
	}

	if err := config.Write(opts.Path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "Wrote %s\n", opts.Path)
	return nil
}
