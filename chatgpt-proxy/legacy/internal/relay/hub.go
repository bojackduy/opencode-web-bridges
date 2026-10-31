package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	errAgentDisconnected = errors.New("agent disconnected")
	errAgentTimeout      = errors.New("agent did not respond in time")
)

func timeoutCh(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// agentConn is one registered local agent connected through the tunnel.
type agentConn struct {
	ID   string
	Name string

	mu   sync.Mutex
	conn *websocket.Conn

	// inflight maps tunnel request IDs to response channels.
	inflightMu sync.Mutex
	inflight   map[string]chan []byte
}

// hub tracks all connected agents.
type hub struct {
	mu      sync.Mutex
	agents  map[string]*agentConn
	order   []string // insertion order for stable round-robin picking
	nextIdx int
}

func newHub() *hub {
	return &hub{agents: map[string]*agentConn{}}
}

func (h *hub) register(ac *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.agents[ac.ID]; exists {
		// Replace the old connection.
		h.agents[ac.ID].close()
	}
	h.agents[ac.ID] = ac
	h.order = append(h.order, ac.ID)
	log.Printf("relay: agent registered: %s (%s)", ac.Name, ac.ID)
}

func (h *hub) unregister(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.agents, id)
	for i, v := range h.order {
		if v == id {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
	log.Printf("relay: agent unregistered: %s", id)
}

// pick returns a connected agent, round-robin across registrations.
func (h *hub) pick() *agentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.order) == 0 {
		return nil
	}
	for i := 0; i < len(h.order); i++ {
		idx := (h.nextIdx + i) % len(h.order)
		if ac, ok := h.agents[h.order[idx]]; ok {
			h.nextIdx = (idx + 1) % len(h.order)
			return ac
		}
	}
	return nil
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.agents)
}

func (ac *agentConn) close() {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.conn != nil {
		_ = ac.conn.Close()
	}
}

// call sends a JSON-RPC request over the tunnel and waits for the response.
func (ac *agentConn) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	id := hex.EncodeToString(idBytes)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	ch := make(chan []byte, 1)
	ac.inflightMu.Lock()
	ac.inflight[id] = ch
	ac.inflightMu.Unlock()
	defer func() {
		ac.inflightMu.Lock()
		delete(ac.inflight, id)
		ac.inflightMu.Unlock()
	}()

	ac.mu.Lock()
	if ac.conn == nil {
		ac.mu.Unlock()
		return nil, errAgentDisconnected
	}
	err = ac.conn.WriteMessage(websocket.TextMessage, payload)
	ac.mu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-timeoutCh(timeout):
		return nil, errAgentTimeout
	}
}

// deliver routes an agent's response to the waiting caller.
func (ac *agentConn) deliver(data []byte) {
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &msg); err != nil || msg.ID == "" {
		return
	}
	ac.inflightMu.Lock()
	ch, ok := ac.inflight[msg.ID]
	ac.inflightMu.Unlock()
	if ok {
		select {
		case ch <- data:
		default:
		}
	}
}
