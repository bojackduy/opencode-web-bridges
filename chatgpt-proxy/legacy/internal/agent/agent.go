// Package agent implements the outbound tunnel client that runs on the
// user's machine. It opens a WebSocket connection *to* the relay (no
// inbound ports needed) and executes MCP tool calls locally.
package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"chatgpt-proxy/internal/mcp"
	"chatgpt-proxy/legacy/internal/tools"
)

// Agent is the local side of the tunnel.
type Agent struct {
	// RelayURL is the WebSocket endpoint, e.g. wss://relay.example.com/ws/agent.
	RelayURL string
	// Token authenticates this agent with the relay.
	Token string
	// Name identifies the agent to the relay (shown to MCP clients).
	Name string

	Tools *tools.Tools

	// ReconnectBase is the initial backoff delay between reconnects.
	ReconnectBase time.Duration

	mu       sync.Mutex
	conn     *websocket.Conn
	initOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

// init lazily creates the lifecycle channels so Stop/Run are safe to call
// in any order (and Run may never be reached).
func (a *Agent) init() {
	a.initOnce.Do(func() {
		a.stopCh = make(chan struct{})
		a.done = make(chan struct{})
	})
}

// Run connects to the relay and serves requests until Stop is called or the
// process exits. It reconnects automatically with exponential backoff.
func (a *Agent) Run() error {
	a.init()
	backoff := a.ReconnectBase
	if backoff <= 0 {
		backoff = time.Second
	}
	for {
		select {
		case <-a.stopCh:
			close(a.done)
			return nil
		default:
		}
		err := a.connectOnce()
		if err != nil {
			log.Printf("agent: connection lost: %v; reconnecting in %s", err, backoff)
			select {
			case <-a.stopCh:
				close(a.done)
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = a.ReconnectBase
	}
}

// Stop disconnects and stops the reconnect loop.
func (a *Agent) Stop() {
	a.init()
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	a.mu.Lock()
	if a.conn != nil {
		_ = a.conn.Close()
	}
	a.mu.Unlock()
}

func (a *Agent) connectOnce() error {
	header := map[string][]string{"Authorization": {"Bearer " + a.Token}}
	conn, _, err := websocket.DefaultDialer.Dial(a.RelayURL, header)
	if err != nil {
		return fmt.Errorf("dial %s: %w", a.RelayURL, err)
	}
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	log.Printf("agent: connected to relay %s", a.RelayURL)

	// Announce ourselves so the relay can register us immediately.
	hello, _ := json.Marshal(map[string]any{"type": "hello", "name": a.Name})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		return err
	}

	defer func() {
		a.mu.Lock()
		a.conn = nil
		a.mu.Unlock()
		_ = conn.Close()
	}()

	// Heartbeat: reply to pings and detect dead connections.
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-a.stopCh:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if err := a.handleMessage(data); err != nil {
			log.Printf("agent: handleMessage: %v", err)
		}
	}
}

// handleMessage processes one JSON-RPC request from the relay.
func (a *Agent) handleMessage(data []byte) error {
	var req mcp.Request
	if err := json.Unmarshal(data, &req); err != nil {
		// Not a request (could be a relay-level message); ignore.
		return nil
	}
	if req.Method == "" {
		return nil
	}
	if req.IsNotification() {
		return nil
	}

	var resp *mcp.Response
	switch req.Method {
	case mcp.MethodPing:
		resp = &mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}
	case mcp.MethodToolsList:
		resp = a.handleToolsList(req.ID)
	case mcp.MethodToolsCall:
		resp = a.handleToolsCall(req.ID, req.Params)
	default:
		resp = mcp.NewErrorf(req.ID, mcp.CodeMethodNotFound, "method not supported by agent: %s", req.Method)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("no connection")
	}
	return a.conn.WriteMessage(websocket.TextMessage, out)
}

func (a *Agent) handleToolsList(id json.RawMessage) *mcp.Response {
	defs := a.Tools.List()
	tools := make([]mcp.Tool, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, mcp.Tool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
		})
	}
	resp, err := mcp.NewResponse(id, mcp.ToolListResult{Tools: tools})
	if err != nil {
		return mcp.NewError(id, mcp.CodeInternalError, err.Error())
	}
	return resp
}

func (a *Agent) handleToolsCall(id json.RawMessage, params json.RawMessage) *mcp.Response {
	var p mcp.ToolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return mcp.NewErrorf(id, mcp.CodeInvalidParams, "invalid tools/call params: %v", err)
	}
	if p.Name == "" {
		return mcp.NewError(id, mcp.CodeInvalidParams, "tool name is required")
	}
	log.Printf("agent: tool call: %s", p.Name)
	resultJSON, err := a.Tools.Call(p.Name, p.Arguments)
	if err != nil {
		return mcp.NewErrorf(id, mcp.CodeInternalError, "tool dispatch failed: %v", err)
	}
	resp, err := mcp.NewResponse(id, json.RawMessage(resultJSON))
	if err != nil {
		return mcp.NewError(id, mcp.CodeInternalError, err.Error())
	}
	return resp
}
