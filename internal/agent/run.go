package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ollama"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"github.com/zachahn/editor/internal/config"
	"github.com/zachahn/editor/internal/session"
)

const Preamble = `You are rewriting text inside a sandboxed session. The session consists of the files ORIGINAL.txt, CURRENT.txt, and any SCRATCH-<n>.txt files (for example SCRATCH-1.txt).

- ORIGINAL.txt holds the original text and is read-only.
- CURRENT.txt holds the working rewrite. The final rewrite MUST be left in CURRENT.txt.
- SCRATCH-<n>.txt files may be created for notes. Scratch files persist across revisions of the session.
- All text replies are dropped. Only the contents of CURRENT.txt are returned.
- The run ends when a reply is given without a tool call.

Use the tools list_files, read_file, write_file, edit_file, and grep to work with the files. Read CURRENT.txt before changing it.

The rewriting instructions follow.

`

func NewChatModel(ctx context.Context, cfg *config.Config) (model.ToolCallingChatModel, error) {
	timeout := cfg.RequestTimeout()
	httpClient := &http.Client{Timeout: timeout}
	switch cfg.Platform {
	case config.PlatformOllama:
		return ollama.NewChatModel(ctx, &ollama.ChatModelConfig{
			BaseURL:    cfg.BaseURL,
			Model:      cfg.Model,
			Timeout:    timeout,
			HTTPClient: httpClient,
		})
	case config.PlatformLlamaCpp, config.PlatformOpenAICompatible:
		apiKey := ""
		if cfg.Platform == config.PlatformOpenAICompatible {
			apiKey = cfg.APIKey
		}
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL:    cfg.BaseURL,
			APIKey:     apiKey,
			Model:      cfg.Model,
			Timeout:    timeout,
			HTTPClient: httpClient,
		})
	}
	return nil, fmt.Errorf("unknown platform %q", cfg.Platform)
}

type Runner struct {
	Model    model.ToolCallingChatModel
	MaxSteps int
}

var ErrEmptyResult = errors.New("the agent run ended with an empty CURRENT.txt")

func graphStepLimit(maxSteps int) int {
	return 2 * maxSteps
}

func (r *Runner) Run(ctx context.Context, sessionDir, systemPrompt, userMessage string) (string, error) {
	overlay := NewOverlay(sessionDir)

	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: r.Model,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools:               NewTools(overlay),
			ExecuteSequentially: true,
			UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
				return toolError(fmt.Errorf("unknown tool %q; available tools are list_files, read_file, write_file, edit_file, grep", name)), nil
			},
		},
		MaxStep: graphStepLimit(r.MaxSteps),
	})
	if err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}

	messages := []*schema.Message{
		schema.SystemMessage(Preamble + systemPrompt),
		schema.UserMessage(userMessage),
	}
	if _, err := agent.Generate(ctx, messages); err != nil {
		if errors.Is(err, compose.ErrExceedMaxSteps) {
			return "", fmt.Errorf("the agent run exceeded max_steps (%d) without finishing", r.MaxSteps)
		}
		return "", fmt.Errorf("agent run failed: %w", err)
	}

	current, err := overlay.Read(session.CurrentFile)
	if err != nil {
		return "", fmt.Errorf("read CURRENT.txt: %w", err)
	}
	if strings.TrimSpace(current) == "" {
		return "", ErrEmptyResult
	}
	if err := overlay.Commit(); err != nil {
		return "", fmt.Errorf("commit session files: %w", err)
	}
	return current, nil
}
