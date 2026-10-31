// Package mcp implements the subset of the Model Context Protocol (MCP)
// needed for gemini-proxy: JSON-RPC 2.0 framing, initialization, and the
// tools/list + tools/call lifecycle. Identical framing to chatgpt-proxy so
// clients speak the same shape to either engine.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP protocol version we advertise. Clients send
// their own version in initialize; we echo back the latest we support.
const ProtocolVersion = "2025-06-18"

// JSON-RPC method names used by MCP.
const (
	MethodInitialize         = "initialize"
	MethodInitializedNotif   = "notifications/initialized"
	MethodPing               = "ping"
	MethodToolsList          = "tools/list"
	MethodToolsCall          = "tools/call"
	MethodResourcesList      = "resources/list"
	MethodResourcesTemplates = "resources/templates/list"
	MethodPromptsList        = "prompts/list"
	MethodLoggingSetLevel    = "logging/setLevel"
	MethodCompletionComplete = "completion/complete"
	MethodRootsList          = "roots/list"
)

// Error codes per the JSON-RPC 2.0 / MCP specification.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// MCP-specific codes.
	CodeMethodNotSupported = -32004
)

// Request is a JSON-RPC request message.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request has no id (a notification).
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0
}

// Response is a JSON-RPC response message.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// --- Server messages ---

// InitializeParams is the params of an initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// ClientInfo identifies the MCP client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the result of an initialize request.
type InitializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    ServerCaps `json:"capabilities"`
	ServerInfo      ServerInfo `json:"serverInfo"`
	Instructions    string     `json:"instructions,omitempty"`
}

// ServerCaps advertises the capabilities the server supports.
type ServerCaps struct {
	Tools *ToolsCaps `json:"tools,omitempty"`
}

// ToolsCaps advertises tools support and whether list changes are notified.
type ToolsCaps struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool describes a callable tool exposed over MCP.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolListResult is the result of a tools/list request.
type ToolListResult struct {
	Tools []Tool `json:"tools"`
}

// ToolCallParams is the params of a tools/call request.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Content is a content block inside a tool result.
type Content struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// ToolResult is the result of a tools/call request.
type ToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// TextResult builds a successful text-only tool result.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []Content{{Type: "text", Text: text}}}
}

// ErrorResult builds a failing tool result (tool-level error, not RPC error).
func ErrorResult(text string) ToolResult {
	return ToolResult{Content: []Content{{Type: "text", Text: text}}, IsError: true}
}

// --- Helpers ---

// NewResponse builds a JSON-RPC response with the given result.
func NewResponse(id json.RawMessage, result any) (*Response, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Response{JSONRPC: "2.0", ID: id, Result: raw}, nil
}

// NewError builds a JSON-RPC error response.
func NewError(id json.RawMessage, code int, message string) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: message}}
}

// NewErrorf builds a JSON-RPC error response with a formatted message.
func NewErrorf(id json.RawMessage, code int, format string, args ...any) *Response {
	return NewError(id, code, fmt.Sprintf(format, args...))
}

// ParseErrorResponse extracts a JSON-RPC error from raw bytes, or nil.
func ParseErrorResponse(data []byte) *Error {
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return &Error{Code: CodeParseError, Message: "invalid response: " + err.Error()}
	}
	return resp.Error
}
