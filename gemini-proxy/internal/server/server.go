// Package server implements the MCP server surface of gemini-proxy:
// JSON-RPC dispatch for initialize / ping / tools.list / tools.call, and
// the two tools agents actually use — ask_gemini and gemini_status.
//
// Transport-agnostic: the same Handler serves both the stdio transport
// (newline-delimited JSON) and Streamable HTTP (POST /mcp).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bojackduy/gemini-proxy/internal/gemini"
	"github.com/bojackduy/gemini-proxy/internal/mcp"
)

// Version of this server, reported in initialize.
const Version = "0.1.0"

// Tool names.
const (
	ToolAsk    = "ask_gemini"
	ToolStatus = "gemini_status"
)

// Gemini is the slice of the browser client the server needs.
type Gemini interface {
	Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error)
	Status(ctx context.Context) (*gemini.Status, error)
}

// Handler dispatches JSON-RPC requests to the Gemini-backed tools.
type Handler struct {
	gm         Gemini
	profileDir string
}

// NewHandler builds a Handler.
func NewHandler(gm Gemini, profileDir string) *Handler {
	return &Handler{gm: gm, profileDir: profileDir}
}

// Handle processes one JSON-RPC request and returns the response bytes, or
// nil when the message is a notification (no response expected).
func (h *Handler) Handle(ctx context.Context, raw []byte) []byte {
	var req mcp.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return mustJSON(mcp.NewError(nil, mcp.CodeParseError, "parse error: "+err.Error()))
	}
	if req.JSONRPC != "2.0" {
		return mustJSON(mcp.NewError(req.ID, mcp.CodeInvalidRequest, "jsonrpc must be 2.0"))
	}
	if req.IsNotification() {
		return nil
	}
	resp := h.dispatch(ctx, &req)
	return mustJSON(resp)
}

func (h *Handler) dispatch(ctx context.Context, req *mcp.Request) *mcp.Response {
	switch req.Method {
	case mcp.MethodInitialize:
		var p mcp.InitializeParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcp.NewError(req.ID, mcp.CodeInvalidParams, "invalid initialize params: "+err.Error())
		}
		return mustResp(mcp.NewResponse(req.ID, mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			Capabilities:    mcp.ServerCaps{Tools: &mcp.ToolsCaps{}},
			ServerInfo:      mcp.ServerInfo{Name: "gemini-proxy", Version: Version},
			Instructions: "I bridge an AI agent to Gemini Web. Use ask_gemini to " +
				"send a prompt and get the answer back (free, via the user's Gemini " +
				"session in Chrome); use gemini_status to check the connection first.",
		}))

	case mcp.MethodPing:
		return mustResp(mcp.NewResponse(req.ID, map[string]any{}))

	case mcp.MethodToolsList:
		return mustResp(mcp.NewResponse(req.ID, mcp.ToolListResult{Tools: tools()}))

	case mcp.MethodToolsCall:
		return h.callTool(ctx, req)

	default:
		return mcp.NewErrorf(req.ID, mcp.CodeMethodNotFound, "method not found: %s", req.Method)
	}
}

func (h *Handler) callTool(ctx context.Context, req *mcp.Request) *mcp.Response {
	var p mcp.ToolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return mcp.NewError(req.ID, mcp.CodeInvalidParams, "invalid tools/call params: "+err.Error())
	}
	var result mcp.ToolResult
	switch p.Name {
	case ToolAsk:
		result = h.askGemini(ctx, p.Arguments)
	case ToolStatus:
		result = h.status(ctx)
	default:
		return mcp.NewErrorf(req.ID, mcp.CodeMethodNotFound, "unknown tool: %s", p.Name)
	}
	return mustResp(mcp.NewResponse(req.ID, result))
}

// --- tools -------------------------------------------------------------------

func (h *Handler) askGemini(ctx context.Context, args json.RawMessage) mcp.ToolResult {
	var a struct {
		Prompt     string `json:"prompt"`
		NewSession *bool  `json:"new_session"`
		Timeout    int    `json:"timeout_seconds"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return mcp.ErrorResult("invalid arguments: " + err.Error())
		}
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return mcp.ErrorResult("prompt is required")
	}
	newSession := true
	if a.NewSession != nil {
		newSession = *a.NewSession
	}
	timeout := time.Duration(a.Timeout) * time.Second
	if timeout <= 0 {
		timeout = gemini.DefaultTimeout
	}
	answer, err := h.gm.Ask(ctx, a.Prompt, newSession, timeout)
	if err != nil {
		if errors.Is(err, gemini.ErrNotLoggedIn) {
			return mcp.ErrorResult("not logged in to gemini.google.com — run gemini-proxy -wait-login and sign in with Google once")
		}
		return mcp.ErrorResult("ask_gemini: " + err.Error())
	}
	return mcp.TextResult(answer)
}

func (h *Handler) status(ctx context.Context) mcp.ToolResult {
	st, err := h.gm.Status(ctx)
	if err != nil {
		if errors.Is(err, gemini.ErrNotLoggedIn) {
			return mcp.ErrorResult("not logged in to gemini.google.com — run gemini-proxy -wait-login and sign in with Google once")
		}
		return mcp.ErrorResult("gemini_status: " + err.Error())
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return mcp.ErrorResult("gemini_status: " + err.Error())
	}
	return mcp.TextResult(string(raw))
}

// --- tool schemas ------------------------------------------------------------

func tools() []mcp.Tool {
	boolPtr := false
	_ = boolPtr
	return []mcp.Tool{
		{
			Name:        ToolAsk,
			Description: "Send a prompt to Gemini Web (gemini.google.com) and return the answer as Markdown text.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt":          map[string]any{"type": "string", "description": "The prompt to send."},
					"new_session":     map[string]any{"type": "boolean", "description": "Start a fresh chat each call. False continues the previous one.", "default": true},
					"timeout_seconds": map[string]any{"type": "integer", "description": "Max seconds to wait.", "default": 600},
				},
				"required": []string{"prompt"},
			},
		},
		{
			Name:        ToolStatus,
			Description: "Check that the Gemini bridge is ready (logged in, composer present, profile path).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// --- helpers -------------------------------------------------------------------

func mustResp(resp *mcp.Response, err error) *mcp.Response {
	if err != nil {
		return mcp.NewError(nil, mcp.CodeInternalError, err.Error())
	}
	return resp
}

func mustJSON(resp *mcp.Response) []byte {
	raw, err := json.Marshal(resp)
	if err != nil {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":%d,"message":"marshal: %s"}}`, mcp.CodeInternalError, err.Error()))
	}
	return raw
}
