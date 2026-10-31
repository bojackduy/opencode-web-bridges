// Package relay implements the public gateway that bridges MCP clients
// (e.g. ChatGPT Web) with locally-connected agents over an outbound
// WebSocket tunnel.
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"chatgpt-proxy/internal/mcp"
)

// Config holds the relay's runtime settings.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// AgentToken authenticates agents connecting to /ws/agent.
	AgentToken string
	// ClientToken, when non-empty, is required as a Bearer token on /mcp.
	ClientToken string
	// AgentTimeout bounds how long we wait for an agent to answer a call.
	AgentTimeout time.Duration
}

// Server is the relay gateway.
type Server struct {
	cfg Config
	hub *hub
	up  websocket.Upgrader
	mux *http.ServeMux
}

// New builds a relay Server.
func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.AgentTimeout <= 0 {
		cfg.AgentTimeout = 5 * time.Minute
	}
	s := &Server{
		cfg: cfg,
		hub: newHub(),
		up: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		mux: http.NewServeMux(),
	}
	s.mux.HandleFunc("/ws/agent", s.handleAgentWS)
	s.mux.HandleFunc("/mcp", s.handleMCP)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"agents":%d}`, s.hub.count())))
	})
	return s
}

// Handler returns the root HTTP handler (useful for tests and embedding).
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe starts the relay and blocks.
func (s *Server) ListenAndServe() error {
	log.Printf("relay: listening on %s", s.cfg.Addr)
	return http.ListenAndServe(s.cfg.Addr, s.mux)
}

// --- Agent WebSocket endpoint ---

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	// Accept the token either as ?token= or as an Authorization: Bearer header.
	token := r.URL.Query().Get("token")
	if auth := r.Header.Get("Authorization"); auth != "" && token == "" {
		token = strings.TrimPrefix(auth, "Bearer ")
	}
	if s.cfg.AgentToken != "" && token != s.cfg.AgentToken {
		http.Error(w, "invalid agent token", http.StatusUnauthorized)
		return
	}
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("relay: agent upgrade failed: %v", err)
		return
	}

	id := newID()
	ac := &agentConn{
		ID:       id,
		Name:     "agent-" + id[:6],
		conn:     conn,
		inflight: map[string]chan []byte{},
	}
	s.hub.register(ac)
	defer s.hub.unregister(id)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// A hello message carries the agent's display name.
		var hello struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &hello) == nil && hello.Type == "hello" && hello.Name != "" {
			ac.mu.Lock()
			ac.Name = hello.Name
			ac.mu.Unlock()
			continue
		}
		ac.deliver(data)
	}
}

// --- MCP Streamable HTTP endpoint ---

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ClientToken != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != s.cfg.ClientToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var req mcp.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeResponse(w, r, mcp.NewError(nil, mcp.CodeParseError, "invalid JSON-RPC payload"))
		return
	}

	resp := s.dispatch(&req)

	// Session header for Streamable HTTP clients that track sessions.
	if req.Method == mcp.MethodInitialize && !req.IsNotification() {
		w.Header().Set("Mcp-Session-Id", newID())
	}

	if req.IsNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.writeResponse(w, r, resp)
}

func (s *Server) dispatch(req *mcp.Request) *mcp.Response {
	switch req.Method {
	case mcp.MethodInitialize:
		return s.handleInitialize(req)
	case mcp.MethodPing:
		return &mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}
	case mcp.MethodToolsList:
		return s.forward(req, mcp.MethodToolsList, nil)
	case mcp.MethodToolsCall:
		return s.forward(req, mcp.MethodToolsCall, req.Params)
	case mcp.MethodResourcesList, mcp.MethodResourcesTemplates, mcp.MethodPromptsList:
		// Not implemented: return empty collections so clients degrade gracefully.
		return &mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}
	default:
		return mcp.NewErrorf(req.ID, mcp.CodeMethodNotFound, "method not supported: %s", req.Method)
	}
}

func (s *Server) handleInitialize(req *mcp.Request) *mcp.Response {
	var params mcp.InitializeParams
	_ = json.Unmarshal(req.Params, &params)
	version := params.ProtocolVersion
	if version == "" {
		version = mcp.ProtocolVersion
	}
	result := mcp.InitializeResult{
		ProtocolVersion: version,
		Capabilities:    mcp.ServerCaps{Tools: &mcp.ToolsCaps{ListChanged: false}},
		ServerInfo:      mcp.ServerInfo{Name: "chatgpt-proxy", Version: "0.1.0"},
		Instructions:    "This server forwards tool calls to a local agent connected through an outbound tunnel. Available tools are listed by tools/list.",
	}
	resp, err := mcp.NewResponse(req.ID, result)
	if err != nil {
		return mcp.NewError(req.ID, mcp.CodeInternalError, err.Error())
	}
	return resp
}

// forward relays a request to a connected agent and returns its response.
func (s *Server) forward(req *mcp.Request, method string, params json.RawMessage) *mcp.Response {
	ac := s.hub.pick()
	if ac == nil {
		return mcp.NewError(req.ID, mcp.CodeInternalError, "no agent is connected to the tunnel")
	}
	var p any
	if len(params) > 0 {
		p = json.RawMessage(params)
	}
	resp, err := ac.call(method, p, s.cfg.AgentTimeout)
	if err != nil {
		return mcp.NewErrorf(req.ID, mcp.CodeInternalError, "agent call failed: %v", err)
	}
	// Re-wrap the agent's JSON-RPC response under the client's id.
	var agentResp struct {
		Result json.RawMessage `json:"result"`
		Error  *mcp.Error      `json:"error"`
	}
	if err := json.Unmarshal(resp, &agentResp); err != nil {
		return mcp.NewError(req.ID, mcp.CodeInternalError, "malformed agent response")
	}
	if agentResp.Error != nil {
		return &mcp.Response{JSONRPC: "2.0", ID: req.ID, Error: agentResp.Error}
	}
	return &mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: agentResp.Result}
}

// writeResponse emits the JSON-RPC response, as SSE when the client asked
// for it, otherwise as application/json.
func (s *Server) writeResponse(w http.ResponseWriter, r *http.Request, resp *mcp.Response) {
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		_, _ = w.Write([]byte("event: message\n"))
		_, _ = w.Write([]byte("data: " + string(body) + "\n\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
