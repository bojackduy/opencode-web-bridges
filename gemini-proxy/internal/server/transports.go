package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bojackduy/gemini-proxy/internal/mcp"
)

// RunStdio serves MCP over stdin/stdout (newline-delimited JSON-RPC).
// Logs go to stderr; stdout carries only protocol frames.
func (h *Handler) RunStdio(ctx context.Context) error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if resp := h.Handle(ctx, line); resp != nil {
			out.Write(resp)
			out.WriteByte('\n')
			out.Flush()
		}
	}
	return sc.Err()
}

// NewHTTPHandler serves Streamable-HTTP MCP plus health endpoints:
//
//	POST /mcp      JSON-RPC (accepts a single object or SSE-wrapped reply)
//	GET  /mcp      SSE hello (for clients that probe the stream first)
//	GET  /healthz  liveness ("ok")
func (h *Handler) NewHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Minimal SSE hello so probing clients see an event stream.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", `{"jsonrpc":"2.0","method":"hello","params":{"server":"gemini-proxy"}}`)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(mcp.NewError(nil, mcp.CodeParseError, "parse error: "+err.Error())))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		resp := h.Handle(ctx, raw)
		accept := r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(accept, "text/event-stream") && resp != nil {
			// SSE envelope: one message event carrying the JSON-RPC reply.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(resp))
			return
		}
		w.WriteHeader(http.StatusOK)
		if resp != nil {
			_, _ = w.Write(resp)
		} else {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{}}`))
		}
	})
	return mux
}
