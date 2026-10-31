package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// RunStdio serves MCP over stdin/stdout (newline-delimited JSON), the
// transport most MCP clients (Hermes, Claude Desktop, etc.) use for local
// servers. All logs must go to stderr.
func (h *Handler) RunStdio(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		resp := h.Handle(ctx, line)
		if resp != nil {
			if _, err := os.Stdout.Write(append(resp, '\n')); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// NewHTTPHandler builds the Streamable HTTP surface: POST /mcp for JSON-RPC,
// GET /healthz for liveness.
func (h *Handler) NewHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true}`)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handleMCPPost(w, r)
		case http.MethodDelete:
			// MCP requires DELETE to terminate a session; we are stateless.
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (h *Handler) handleMCPPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp := h.Handle(r.Context(), body)
	if resp == nil {
		// Notification — nothing to return.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	acceptsSSE := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if acceptsSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Mcp-Session-Id", newID())
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", newID())
	w.Write(resp)
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "session"
	}
	return hex.EncodeToString(b)
}

// jsonOnly is a tiny helper used by tests.
func jsonOnly(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
