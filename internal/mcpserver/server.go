package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zachahn/squiggly/internal/agent"
	"github.com/zachahn/squiggly/internal/prose"
	"github.com/zachahn/squiggly/internal/session"
)

const Version = "0.1.0"

type AgentRunner interface {
	Run(ctx context.Context, in agent.RunInput) (*agent.RunResult, error)
}

type Handlers struct {
	Store        *session.Store
	Runner       AgentRunner
	SystemPrompt string
}

func New(h *Handlers) *server.MCPServer {
	s := server.NewMCPServer("squiggly", Version, server.WithToolCapabilities(false), server.WithRecovery(), server.WithInstructions(prose.ServerInstructions))

	s.AddTool(mcp.NewTool(session.ToolStartRewrite,
		mcp.WithDescription(prose.StartRewriteDescription),
		mcp.WithString("text", mcp.Required(), mcp.MinLength(1), mcp.Description(prose.StartRewriteTextDescription)),
		mcp.WithString("instructions", mcp.Description(prose.StartRewriteInstructionsDescription)),
	), h.StartRewrite)

	s.AddTool(mcp.NewTool(session.ToolReviseRewrite,
		mcp.WithDescription(prose.ReviseRewriteDescription),
		mcp.WithString("session_id", mcp.Required(), mcp.Pattern(`^[0-9a-f]{8}$`), mcp.Description(prose.ReviseRewriteSessionIDDescription)),
		mcp.WithString("feedback", mcp.Required(), mcp.MinLength(1), mcp.Description(prose.ReviseRewriteFeedbackDescription)),
	), h.ReviseRewrite)

	return s
}

func stringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required parameter %q", key)
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("parameter %q must be a string", key)
	}
	if str == "" {
		return "", fmt.Errorf("parameter %q must not be empty", key)
	}
	return str, nil
}

func optionalStringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", nil
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("parameter %q must be a string", key)
	}
	return str, nil
}

func encodeArguments(raw any, omit ...string) string {
	if raw == nil {
		return "{}"
	}
	if args, ok := raw.(map[string]any); ok && len(omit) > 0 {
		kept := make(map[string]any, len(args))
		for key, value := range args {
			kept[key] = value
		}
		for _, key := range omit {
			delete(kept, key)
		}
		raw = kept
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		fallback, _ := json.Marshal(map[string]string{"unencodable_arguments": fmt.Sprintf("%v", raw)})
		return string(fallback)
	}
	return string(encoded)
}

func (h *Handlers) reject(ctx context.Context, tool string, req mcp.CallToolRequest, message string) *mcp.CallToolResult {
	if err := h.Store.RecordRejected(context.WithoutCancel(ctx), tool, encodeArguments(req.Params.Arguments), message); err != nil {
		message = errors.Join(errors.New(message), err).Error()
	}
	return mcp.NewToolResultError(message)
}

func (h *Handlers) StartRewrite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	text, err := stringArg(args, "text")
	if err != nil {
		return h.reject(ctx, session.ToolStartRewrite, req, err.Error()), nil
	}
	instructions, err := optionalStringArg(args, "instructions")
	if err != nil {
		return h.reject(ctx, session.ToolStartRewrite, req, err.Error()), nil
	}

	sess, request, err := h.Store.Create(ctx, session.NewSession{
		Text:         text,
		Instructions: instructions,
		SystemPrompt: h.SystemPrompt,
		Arguments:    encodeArguments(req.Params.Arguments, "text", "instructions"),
	})
	if err != nil {
		return h.reject(ctx, session.ToolStartRewrite, req, "could not create session: "+err.Error()), nil
	}

	unlock := h.Store.Lock(sess.PublicID)
	defer unlock()

	sessionLine := fmt.Sprintf("session_id: %s\n", sess.PublicID)
	current, message := h.run(ctx, sess, request, prose.FirstRunMessage, sessionLine+"\n")
	if message != "" {
		return mcp.NewToolResultError(message), nil
	}
	return mcp.NewToolResultText(sessionLine + prose.ReviewReminder + "\n\n" + current), nil
}

func (h *Handlers) ReviseRewrite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	publicID, err := stringArg(args, "session_id")
	if err != nil {
		return h.reject(ctx, session.ToolReviseRewrite, req, err.Error()), nil
	}
	if !session.IDPattern.MatchString(publicID) {
		return h.reject(ctx, session.ToolReviseRewrite, req, fmt.Sprintf("invalid session_id %q: must be 8 lowercase hexadecimal characters", publicID)), nil
	}
	feedback, err := stringArg(args, "feedback")
	if err != nil {
		return h.reject(ctx, session.ToolReviseRewrite, req, err.Error()), nil
	}

	sess, err := h.Store.Find(ctx, publicID)
	if err != nil {
		return h.reject(ctx, session.ToolReviseRewrite, req, err.Error()), nil
	}

	unlock := h.Store.Lock(sess.PublicID)
	defer unlock()

	request, err := h.Store.StartRequest(ctx, sess.ID, session.ToolReviseRewrite, encodeArguments(req.Params.Arguments))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	current, message := h.run(ctx, sess, request, prose.FeedbackPrefix+feedback, "")
	if message != "" {
		return mcp.NewToolResultError(message), nil
	}
	return mcp.NewToolResultText(current), nil
}

func (h *Handlers) run(ctx context.Context, sess *session.Session, request *session.Request, userMessage, errorPrefix string) (string, string) {
	snapshot, err := h.Store.Snapshot(ctx, sess.ID)
	if err != nil {
		return "", h.fail(ctx, sess, request, nil, err, errorPrefix)
	}

	runCtx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	result, err := h.Runner.Run(runCtx, agent.RunInput{
		Snapshot:     snapshot,
		SystemPrompt: sess.SystemPrompt,
		Instructions: sess.Instructions,
		UserMessage:  userMessage,
		Record: func(ctx context.Context, msg *schema.Message) error {
			record, err := agent.TranscriptMessage(msg)
			if err != nil {
				return err
			}
			return h.Store.AppendMessage(ctx, request.ID, record)
		},
	})
	var changed map[string]string
	if result != nil {
		changed = result.Changed
	}
	if err != nil {
		return "", h.fail(ctx, sess, request, changed, err, errorPrefix)
	}

	if err := h.Store.Commit(context.WithoutCancel(ctx), sess.ID, request.ID, snapshot.Version, changed); err != nil {
		return "", h.fail(ctx, sess, request, changed, err, errorPrefix)
	}
	return result.Current, ""
}

func (h *Handlers) fail(ctx context.Context, sess *session.Session, request *session.Request, changed map[string]string, runErr error, prefix string) string {
	message := prefix + runErr.Error()
	if err := h.Store.Fail(context.WithoutCancel(ctx), sess.ID, request.ID, changed, message); err != nil {
		return prefix + errors.Join(runErr, err).Error()
	}
	return message
}
