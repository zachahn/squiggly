package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zachahn/editor/internal/session"
)

const (
	firstRunMessage = "Rewrite CURRENT.txt."
	Version         = "0.1.0"
)

var sessionDirInPath = regexp.MustCompile(`([/\\][0-9a-f]{8})-[0-9]+`)

func toolError(message string) *mcp.CallToolResult {
	return mcp.NewToolResultError(sessionDirInPath.ReplaceAllString(message, "$1"))
}

type AgentRunner interface {
	Run(ctx context.Context, sessionDir, systemPrompt, userMessage string) (string, error)
}

type Handlers struct {
	Store        *session.Store
	Runner       AgentRunner
	SystemPrompt string
}

func New(h *Handlers) *server.MCPServer {
	s := server.NewMCPServer("editor", Version, server.WithToolCapabilities(false), server.WithRecovery())

	s.AddTool(mcp.NewTool("start_rewrite",
		mcp.WithDescription("Start a rewriting session. The text is rewritten by an LLM agent. The result begins with a session_id line, followed by a blank line and the rewritten text. Pass the session_id to revise_rewrite to request revisions."),
		mcp.WithString("text", mcp.Required(), mcp.MinLength(1), mcp.Description("Text to rewrite.")),
	), h.StartRewrite)

	s.AddTool(mcp.NewTool("revise_rewrite",
		mcp.WithDescription("Revise the rewrite of an existing session with feedback. The result contains only the revised text."),
		mcp.WithString("session_id", mcp.Required(), mcp.Pattern(`^[0-9a-f]{8}$`), mcp.Description("Session ID returned by start_rewrite.")),
		mcp.WithString("feedback", mcp.Required(), mcp.MinLength(1), mcp.Description("Feedback on the current rewrite.")),
	), h.ReviseRewrite)

	return s
}

func stringArg(req mcp.CallToolRequest, key string) (string, error) {
	value, ok := req.GetArguments()[key]
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

func (h *Handlers) StartRewrite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	text, err := stringArg(req, "text")
	if err != nil {
		return toolError(err.Error()), nil
	}

	sess, err := h.Store.Create(text, h.SystemPrompt)
	if err != nil {
		return toolError("could not create session: " + err.Error()), nil
	}

	unlock := h.Store.Lock(sess.ID)
	defer unlock()

	current, err := h.Runner.Run(ctx, sess.Dir, h.SystemPrompt, firstRunMessage)
	if err != nil {
		if removeErr := sess.Delete(); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("delete failed session: %w", removeErr))
		}
		return toolError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("session_id: %s\n\n%s", sess.ID, current)), nil
}

func (h *Handlers) ReviseRewrite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := stringArg(req, "session_id")
	if err != nil {
		return toolError(err.Error()), nil
	}
	if !session.IDPattern.MatchString(id) {
		return toolError(fmt.Sprintf("invalid session_id %q: must be 8 lowercase hexadecimal characters", id)), nil
	}
	feedback, err := stringArg(req, "feedback")
	if err != nil {
		return toolError(err.Error()), nil
	}

	unlock := h.Store.Lock(id)
	defer unlock()

	sess, err := h.Store.Find(id)
	if err != nil {
		return toolError(err.Error()), nil
	}
	systemPrompt, err := sess.ReadFile(session.SystemFile)
	if err != nil {
		return toolError(err.Error()), nil
	}

	current, err := h.Runner.Run(ctx, sess.Dir, systemPrompt, "Feedback on CURRENT.txt:\n\n"+feedback)
	if err != nil {
		return toolError(err.Error()), nil
	}
	return mcp.NewToolResultText(current), nil
}
