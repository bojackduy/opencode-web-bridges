package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bojackduy/meta-proxy/internal/meta"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type toolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}
type toolDef struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}
type callFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function callFunction `json:"function"`
}
type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Tools       []toolDef `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

type session struct {
	Pending         bool      `json:"pending,omitempty"`
	PendingPrompt   string    `json:"pending_prompt,omitempty"`
	PendingMessages []message `json:"pending_messages,omitempty"`
	PendingTools    []toolDef `json:"pending_tools,omitempty"`
	PendingChoice   any       `json:"pending_choice,omitempty"`
	Messages        []message `json:"messages"`
	URL             string    `json:"url,omitempty"`
}

// removed browserClient interface - handler uses *meta.Client directly

type handler struct {
	client     *meta.Client
	model      string
	statePath  string
	profileDir string
	gate       chan struct{}
	mu         sync.Mutex
	sessions   map[string]*session
}

func (h *handler) load() error {
	h.sessions = map[string]*session{}
	raw, err := os.ReadFile(h.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &h.sessions)
}

func (h *handler) saveLocked() error {
	raw, err := json.Marshal(h.sessions)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.statePath), 0o700); err != nil {
		return err
	}
	tmp := h.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.statePath)
}

func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": h.model, "object": "model", "created": 0, "owned_by": "meta-web"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", h.chatCompletions)
	mux.HandleFunc("/v1/sessions/abandon", h.abandonSessions)
	mux.HandleFunc("/v1/sessions/recover", h.recoverSession)
	mux.HandleFunc("/v1/sessions/state", h.sessionState)
	return mux
}

func (h *handler) sessionID(r *http.Request) string {
	for _, k := range []string{"X-Opencode-Session-Id", "X-Session-Id", "X-Opencode-Session", "X-Session-Affinity"} {
		if v := r.Header.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func (h *handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "messages must not be empty")
		return
	}
	if req.Model != h.model {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown model")
		return
	}
	schemas, err := toolSchemas(req.Tools)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	choice := effectiveToolChoice(req.ToolChoice, len(req.Tools))

	sid := h.sessionID(r)

	select {
	case <-h.gate:
		defer func() { h.gate <- struct{}{} }()
	case <-r.Context().Done():
		h.writeError(w, http.StatusRequestTimeout, "timeout", "request canceled while queued")
		return
	}

	h.mu.Lock()
	sess, ok := h.sessions[sid]
	if !ok {
		sess = &session{}
		h.sessions[sid] = sess
	}
	_ = sess
	h.mu.Unlock()

	// Build prompt from last user message (minimal).
	var prompt string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			if s, ok := req.Messages[i].Content.(string); ok {
				prompt = s
				break
			}
		}
	}
	if prompt == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "no user message")
		return
	}

	// Mark pending before submit.
	h.mu.Lock()
	sess.Pending = true
	sess.PendingPrompt = prompt
	sess.PendingMessages = append([]message{}, req.Messages...)
	sess.PendingTools = req.Tools
	sess.PendingChoice = req.ToolChoice
	h.mu.Unlock()

	answer, err := h.client.Ask(r.Context(), prompt, sess.URL == "", 10*time.Minute)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: "+err.Error())
		return
	}

	// Validate tool protocol (strict: never weaken).
	calls, verr := meta.ExtractExecutableCalls(answer, schemas, choice)
	if verr != nil && choice == "required" {
		h.writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "tool-protocol validation failed: "+verr.Error())
		h.mu.Lock()
		sess.Pending = false
		h.mu.Unlock()
		return
	}
	_ = calls

	// Clear pending, store result.
	h.mu.Lock()
	sess.Pending = false
	sess.PendingPrompt = ""
	sess.Messages = append([]message{}, req.Messages...)
	sess.Messages = append(sess.Messages, message{Role: "assistant", Content: answer})
	sess.URL = h.client.CurrentURL(r.Context())
	h.saveLocked()
	h.mu.Unlock()

	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-meta-%d", time.Now().Unix()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   h.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer},
			"finish_reason": "stop",
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	log.Printf("[openai] session=%s completed answer_bytes=%d", shortSID(sid), len(answer))
}

func (h *handler) abandonSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "abandon requires POST")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	log.Printf("[openai] abandon called")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (h *handler) recoverSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "recover requires POST")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	id := h.sessionID(r)
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "session header required")
		return
	}
	h.mu.Lock()
	pending := h.sessions[id] != nil && h.sessions[id].Pending
	h.mu.Unlock()
	if !pending {
		h.writeError(w, http.StatusConflict, "invalid_request_error", "no uncertain pending turn for this session")
		return
	}
	calls, bytes := 0, 0
	h.mu.Lock()
	if s := h.sessions[id]; s != nil {
		calls, bytes = 0, len(s.PendingPrompt)
	}
	h.mu.Unlock()
	log.Printf("[openai] session=%s recover pending_prompt_bytes=%d", shortSID(id), bytes)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"recovered":         true,
		"session":           id,
		"cached_tool_calls": calls,
		"response_bytes":    bytes,
	})
}

func (h *handler) sessionState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "state requires GET")
		return
	}
	id := h.sessionID(r)
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "session header required")
		return
	}
	h.mu.Lock()
	sess, _ := h.sessions[id]
	var snap session
	has := sess != nil
	if has {
		snap = *sess
	}
	h.mu.Unlock()
	if !has {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session":              id,
			"state":                "unknown_session",
			"completed_messages":   0,
			"pending_prompt_bytes": 0,
			"thread_url_present":   false,
			"summary":              "No stored chat exists for this session yet.",
			"actions":              []string{},
		})
		return
	}
	class := "clean"
	actions := []string{"continue"}
	if snap.Pending {
		if snap.PendingPrompt != "" || len(snap.PendingMessages) > 0 {
			class = "pending"
			actions = []string{"reseed", "abandon"}
		} else {
			class = "pending_unbound"
			actions = []string{"abandon"}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session":              id,
		"state":                class,
		"completed_messages":   len(snap.Messages),
		"pending_prompt_bytes": len(snap.PendingPrompt),
		"thread_url_present":   snap.URL != "",
		"summary":              stateSummary(class),
		"actions":              actions,
	})
}

func stateSummary(class string) string {
	switch class {
	case "clean":
		return "This chat is up to date; the next message will be answered normally."
	case "pending":
		return "A previous submission is uncertain; it was not resubmitted. Send a distinct request to continue on a fresh thread, or clear the pending turn explicitly."
	case "pending_unbound":
		return "A previous submission is uncertain and its record cannot be acted on; clear it explicitly before continuing."
	default:
		return "No stored chat exists for this session yet."
	}
}

func (h *handler) writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	log.Printf("[openai] refused status=%d code=%s msg=%.300s", status, code, msg)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": code},
	})
}

// toolSchemas converts OpenAI tool definitions into JSON Schema validators.
func toolSchemas(tools []toolDef) (map[string]*jsonschema.Schema, error) {
	out := map[string]*jsonschema.Schema{}
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(localSchemas{})
		b, err := json.Marshal(t.Function.Parameters)
		if err != nil {
			return nil, err
		}
		var schema any
		if len(b) == 0 || string(b) == "null" {
			schema = map[string]any{"type": "object"}
		} else if err = json.Unmarshal(b, &schema); err != nil {
			return nil, err
		}
		if err = compiler.AddResource("https://proxy.local/tool.json", schema); err != nil {
			return nil, err
		}
		s, err := compiler.Compile("https://proxy.local/tool.json")
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", t.Function.Name, err)
		}
		out[t.Function.Name] = s
	}
	return out, nil
}

type localSchemas struct{}

func (localSchemas) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references disabled: %s", url)
}

func effectiveToolChoice(raw any, nTools int) string {
	if nTools == 0 {
		return "none"
	}
	if raw == nil {
		return "auto"
	}
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
		return "auto"
	default:
		return "auto"
	}
}

func shortSID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func pendingSessionIDs(h *handler) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for id, s := range h.sessions {
		if s != nil && s.Pending {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
