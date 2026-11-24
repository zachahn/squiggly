package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ollama"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	ub "github.com/cloudwego/eino/utils/callbacks"

	"github.com/zachahn/squiggly/internal/config"
	"github.com/zachahn/squiggly/internal/session"
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

type RunInput struct {
	Snapshot     *session.Snapshot
	SystemPrompt string
	UserMessage  string
	Record       func(context.Context, *schema.Message) error
}

type RunResult struct {
	Current string
	Changed map[string]string
}

var (
	ErrEmptyResult      = errors.New("the agent run ended with an empty CURRENT.txt")
	ErrDeadlineReached  = errors.New("the agent run reached its deadline before finishing")
	errRecordingStopped = errors.New("recording the transcript failed")
)

func graphStepLimit(maxSteps int) int {
	return 2 * maxSteps
}

func (r *Runner) Run(ctx context.Context, in RunInput) (*RunResult, error) {
	overlay := NewOverlay(in.Snapshot)
	result := &RunResult{}
	defer func() { result.Changed = overlay.Changed() }()

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	recordCtx := context.WithoutCancel(ctx)
	record := func(msg *schema.Message) {
		if in.Record == nil || errors.Is(context.Cause(runCtx), errRecordingStopped) {
			return
		}
		if err := in.Record(recordCtx, msg); err != nil {
			cancel(fmt.Errorf("%w: %w", errRecordingStopped, err))
		}
	}

	reactAgent, err := react.NewAgent(runCtx, &react.AgentConfig{
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
		return result, fmt.Errorf("create agent: %w", err)
	}

	messages := []*schema.Message{
		schema.SystemMessage(Preamble + in.SystemPrompt),
		schema.UserMessage(in.UserMessage),
	}
	for _, msg := range messages {
		record(msg)
	}
	if cause := context.Cause(runCtx); cause != nil {
		return result, cause
	}

	_, err = reactAgent.Generate(runCtx, messages, transcriptOption(record))
	if cause := context.Cause(runCtx); errors.Is(cause, errRecordingStopped) {
		return result, cause
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, ErrDeadlineReached
	}
	if err != nil {
		if errors.Is(err, compose.ErrExceedMaxSteps) {
			return result, fmt.Errorf("the agent run exceeded max_steps (%d) without finishing", r.MaxSteps)
		}
		return result, fmt.Errorf("agent run failed: %w", err)
	}

	current, err := overlay.Read(session.CurrentFile)
	if err != nil {
		return result, fmt.Errorf("read CURRENT.txt: %w", err)
	}
	if strings.TrimSpace(current) == "" {
		return result, ErrEmptyResult
	}
	result.Current = current
	return result, nil
}

func transcriptOption(record func(*schema.Message)) agent.AgentOption {
	handler := ub.NewHandlerHelper().
		ChatModel(&ub.ModelCallbackHandler{
			OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
				if output != nil && output.Message != nil {
					record(output.Message)
				}
				return ctx
			},
		}).
		ToolsNode(&ub.ToolsNodeCallbackHandlers{
			OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, output []*schema.Message) context.Context {
				for _, msg := range output {
					record(msg)
				}
				return ctx
			},
		}).
		Handler()
	return agent.WithComposeOptions(compose.WithCallbacks(handler))
}

type transcriptToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func TranscriptMessage(msg *schema.Message) (session.Message, error) {
	record := session.Message{
		Role:      string(msg.Role),
		Content:   msg.Content,
		Reasoning: msg.ReasoningContent,
	}
	switch msg.Role {
	case schema.Assistant:
		if len(msg.ToolCalls) > 0 {
			calls := make([]transcriptToolCall, len(msg.ToolCalls))
			for i, call := range msg.ToolCalls {
				calls[i] = transcriptToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments}
			}
			encoded, err := json.Marshal(calls)
			if err != nil {
				return record, fmt.Errorf("encode tool calls: %w", err)
			}
			toolCalls := string(encoded)
			record.ToolCalls = &toolCalls
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			usage := msg.ResponseMeta.Usage
			prompt, completion, total := int64(usage.PromptTokens), int64(usage.CompletionTokens), int64(usage.TotalTokens)
			record.PromptTokens, record.CompletionTokens, record.TotalTokens = &prompt, &completion, &total
		}
	case schema.Tool:
		callID, toolName := msg.ToolCallID, msg.ToolName
		record.ToolCallID, record.ToolName = &callID, &toolName
	}
	return record, nil
}
