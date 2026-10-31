// Package server implements the MCP server surface of chatgpt-proxy:
// JSON-RPC dispatch for initialize / ping / tools.list / tools.call, and
// the two tools agents actually use — ask_chatgpt and chatgpt_status.
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

	"chatgpt-proxy/internal/chatgpt"
	"chatgpt-proxy/internal/mcp"
)

// Version of this server, reported in initialize.
const Version = "0.2.0"

// Tool names.
const (
	ToolAsk    = "ask_chatgpt"
	ToolStatus = "chatgpt_status"
)

// Chatgpt is the slice of the browser client the server needs.
type Chatgpt interface {
	Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error)
	Status(ctx context.Context) (*chatgpt.Status, error)
}

// Handler dispatches JSON-RPC requests to the ChatGPT-backed tools.
type Handler struct {
	cg         Chatgpt
	profileDir string // Chrome profile dir; scopes the cross-process turn lock
}

// NewHandler builds a Handler.
func NewHandler(cg Chatgpt, profileDir string) *Handler {
	return &Handler{cg: cg, profileDir: profileDir}
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
			ServerInfo:      mcp.ServerInfo{Name: "chatgpt-proxy", Version: Version},
			Instructions: "I bridge an AI agent to ChatGPT Web. Use ask_chatgpt to " +
				"send a prompt and get the answer back (free, via the user's ChatGPT " +
				"session in Chrome); use chatgpt_status to check the connection first.",
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
		result = h.askChatgpt(ctx, p.Arguments)
	case ToolStatus:
		result = h.status(ctx)
	default:
		return mcp.NewErrorf(req.ID, mcp.CodeMethodNotFound, "unknown tool: %s", p.Name)
	}
	return mustResp(mcp.NewResponse(req.ID, result))
}

// --- tools -------------------------------------------------------------------

func (h *Handler) askChatgpt(ctx context.Context, args json.RawMessage) mcp.ToolResult {
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
		return mcp.ErrorResult("ask_chatgpt requires a non-empty 'prompt'")
	}
	newSession := true
	if a.NewSession != nil {
		newSession = *a.NewSession
	}
	timeout := time.Duration(a.Timeout) * time.Second
	if a.Timeout <= 0 {
		timeout = chatgpt.DefaultTimeout
	}

	// Cross-process turn lock: the :8788 adapter drives the same Chrome
	// from another process. Read-only probes skip it; whole turns hold it.
	lock := chatgpt.LockTurn
	if owned, ok := h.cg.(interface{ UsesOwnedTabs() bool }); ok && owned.UsesOwnedTabs() {
		lock = chatgpt.LockOwnedTurn
	}
	unlock, err := lock(ctx, h.profileDir)
	if err != nil {
		return mcp.ErrorResult("bridge busy: " + err.Error())
	}
	defer unlock()

	text, err := h.cg.Ask(ctx, a.Prompt, newSession, timeout)
	if err != nil {
		return mcp.ErrorResult(agentMessage(err))
	}
	return mcp.TextResult(text)
}

func (h *Handler) status(ctx context.Context) mcp.ToolResult {
	lock := chatgpt.LockTurn
	if owned, ok := h.cg.(interface{ UsesOwnedTabs() bool }); ok && owned.UsesOwnedTabs() {
		lock = chatgpt.LockOwnedTurn
	}
	unlock, err := lock(ctx, h.profileDir)
	if err != nil {
		return mcp.ErrorResult("bridge busy: " + err.Error())
	}
	defer unlock()

	st, err := h.cg.Status(ctx)
	if err != nil {
		return mcp.ErrorResult(agentMessage(err))
	}
	raw, _ := json.Marshal(st)
	return mcp.TextResult(string(raw))
}

// agentMessage turns a chatgpt error into something an agent can act on.
func agentMessage(err error) string {
	switch {
	case errors.Is(err, chatgpt.ErrChromeNotFound):
		return "ChatGPT proxy: no Chrome/Chromium binary found on this machine."
	case errors.Is(err, chatgpt.ErrNotLoggedIn):
		return "ChatGPT proxy: not logged in. A Chrome window with the dedicated " +
			"profile should have opened — log in at chatgpt.com there once, then retry."
	case errors.Is(err, chatgpt.ErrNoComposer):
		return "ChatGPT proxy: the chatgpt.com composer was not found. The page may " +
			"be showing a modal or a different layout — open the proxy Chrome window " +
			"and check, then retry."
	case errors.Is(err, chatgpt.ErrNoNewChat):
		return "ChatGPT proxy: could not start a new chat (New chat button not found)."
	case errors.Is(err, chatgpt.ErrNoResponse):
		return "ChatGPT proxy: timed out waiting for an answer. Try a shorter prompt " +
			"or increase timeout_seconds."
	default:
		return "ChatGPT proxy error: " + err.Error()
	}
}

// tools lists the MCP tool definitions.
func tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: ToolAsk,
			Description: "Send a prompt to ChatGPT Web (the user's logged-in session, " +
				"free) and return the assistant's answer as text. Best used for " +
				"reasoning, drafting, summarizing, or any task that benefits from a " +
				"second LLM. Each call starts a fresh conversation by default.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "The prompt to send to ChatGPT.",
					},
					"new_session": map[string]any{
						"type": "boolean",
						"description": "Start a fresh conversation (default true). " +
							"Set false to continue the previous conversation.",
					},
					"timeout_seconds": map[string]any{
						"type":        "number",
						"description": "Max seconds to wait for the answer (default 600).",
					},
				},
				"required": []string{"prompt"},
			},
		},
		{
			Name: ToolStatus,
			Description: "Check whether the ChatGPT Web connection is ready: logged in, " +
				"composer present, Chrome profile in use. Run this first to verify " +
				"the bridge before calling ask_chatgpt.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// --- helpers -----------------------------------------------------------------

func mustResp(r *mcp.Response, err error) *mcp.Response {
	if err != nil {
		return mcp.NewError(nil, mcp.CodeInternalError, err.Error())
	}
	return r
}

func mustJSON(r *mcp.Response) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":%d,"message":"marshal failed"}}`, mcp.CodeInternalError))
	}
	return b
}
