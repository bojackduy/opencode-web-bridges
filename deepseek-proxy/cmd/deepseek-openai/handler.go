package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bojackduy/deepseek-proxy/internal/deepseek"
)

const maxBodyBytes = 64 << 20
const turnTimeout = 15 * time.Minute

type completionsRequest struct {
	Model      string        `json:"model"`
	Messages   []chatMessage `json:"messages"`
	Stream     bool          `json:"stream"`
	Tools      []toolDef     `json:"tools,omitempty"`
	ToolChoice any           `json:"tool_choice,omitempty"`
	User       string        `json:"user,omitempty"`
}

type server struct {
	client  *deepseek.Client
	journal *journal
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/v1/chat/completions", s.completions)
	mux.HandleFunc("/v1/sessions/state", s.state)
	mux.HandleFunc("/v1/sessions/reset", s.reset)
	return mux
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "auth": s.client.HasToken()})
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(deepseek.Models()))
	for _, m := range deepseek.Models() {
		data = append(data, map[string]any{"id": m.ID, "object": "model", "owned_by": "deepseek-web"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session")
	if id == "" {
		id = sessionID(r, "")
	}
	snap := s.journal.snapshot(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_present": snap.UpdatedAt.Unix() > 0,
		"messages":        len(snap.Messages),
		"has_remote":      snap.Remote != "",
		"pending":         snap.Pending != "",
		"sessions":        s.journal.count(),
	})
}

func (s *server) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only", nil)
		return
	}
	var body struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	if body.Session == "" {
		body.Session = sessionID(r, "")
	}
	if err := s.journal.reset(body.Session); err != nil {
		writeError(w, http.StatusInternalServerError, "journal write failed", nil)
		return
	}
	log.Printf("[deepseek] session=%s journal reset", body.Session)
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func (s *server) completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only", nil)
		return
	}
	var req completionsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", nil)
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty", nil)
		return
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported role %q", m.Role), nil)
			return
		}
	}
	msgs := canonicalMessages(req.Messages)
	id := sessionID(r, req.User)
	if !s.journal.acquire(id) {
		writeError(w, http.StatusConflict, "a turn is already running for this session", nil)
		return
	}
	defer s.journal.release(id)

	snap := s.journal.snapshot(id)
	model := deepseek.ResolveModel(req.Model)
	p := planTurn(snap, msgs, req.Tools, req.ToolChoice)
	log.Printf("[deepseek] session=%s plan=%s messages=%d delta=%d tools=%d prompt_bytes=%d stream=%v model=%s",
		id, p.Mode, len(msgs), len(msgs)-len(snap.Messages), len(req.Tools), len(p.Prompt), req.Stream, model.ID)

	if p.Mode == planRetry {
		s.retryPending(w, r, id, snap, msgs, req, model)
		return
	}
	s.runTurn(w, r, id, snap, msgs, req, model, p)
}

func (s *server) runTurn(w http.ResponseWriter, r *http.Request, id string, snap session, msgs []chatMessage, req completionsRequest, model deepseek.ModelConfig, p plan) {
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()

	remote := p.Remote
	if p.Mode == planReseed {
		cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
		var err error
		remote, err = s.client.CreateSession(cctx)
		ccancel()
		if err != nil {
			s.fail(w, id, "create_session", err, false)
			return
		}
	}

	// The pending marker is written before submit so a crash cannot look like a
	// clean journal.
	snap.Pending = p.Seal
	snap.Remote = remote
	if err := s.journal.put(id, snap); err != nil {
		writeError(w, http.StatusInternalServerError, "journal write failed", nil)
		return
	}

	chunks, errs := s.client.Complete(ctx, deepseek.CompleteRequest{
		Prompt:    p.Prompt,
		SessionID: remote,
		Parent:    p.Parent,
		Model:     model,
		Thinking:  model.Thinking,
		Search:    model.Search,
	})

	var reply, reasoning strings.Builder
	var answerID string
	var streamErr error
	for chunks != nil || errs != nil {
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			chunks, errs = nil, nil
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			reply.WriteString(c.Content)
			reasoning.WriteString(c.ReasoningContent)
			if c.MessageID != "" {
				answerID = c.MessageID
			}
		case e, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if e != nil {
				streamErr = e
			}
		}
	}

	if streamErr != nil {
		// Keep the pending marker: a repeated identical request is answered
		// from the remote transcript instead of being resent.
		s.fail(w, id, "completion", streamErr, answerID != "")
		return
	}

	parsed, err := parseAnswer(reply.String(), req.Tools, req.ToolChoice)
	if err != nil {
		log.Printf("[deepseek] session=%s reject=protocol answer_bytes=%d calls=%d", id, reply.Len(), 0)
		writeError(w, http.StatusUnprocessableEntity, "the model reply could not be used as a tool call: "+err.Error(), nil)
		return
	}

	finish := "stop"
	if len(parsed.Calls) > 0 {
		finish = "tool_calls"
	}
	if err := s.journal.commit(id, snap, msgs, parsed.Text, parsed.Calls, remote, answerID); err != nil {
		writeError(w, http.StatusInternalServerError, "journal write failed", nil)
		return
	}
	log.Printf("[deepseek] session=%s done tool_calls=%d answer_bytes=%d reasoning_bytes=%d finish=%s",
		id, len(parsed.Calls), len(parsed.Text), reasoning.Len(), finish)

	s.respond(w, req, model, parsed, reasoning.String(), finish)
}

// retryPending answers a repeated request from the remote transcript instead of
// resending the prompt.
func (s *server) retryPending(w http.ResponseWriter, r *http.Request, id string, snap session, msgs []chatMessage, req completionsRequest, model deepseek.ModelConfig) {
	if snap.Remote == "" {
		writeError(w, http.StatusConflict, "the previous turn is unresolved and had no remote thread; send a new message to continue", map[string]any{"pending": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	hist, err := s.client.History(ctx, snap.Remote)
	if err != nil {
		log.Printf("[deepseek] session=%s retry=unavailable reason=history_error", id)
		writeError(w, http.StatusConflict, "the previous turn is unresolved and could not be read back; send a new message to continue", map[string]any{"pending": true})
		return
	}
	var last *deepseek.HistoryMessage
	for i := range hist {
		if hist[i].Role == "assistant" {
			last = &hist[i]
		}
	}
	if last == nil || last.ID == snap.Parent || strings.TrimSpace(last.Content) == "" {
		log.Printf("[deepseek] session=%s retry=unavailable reason=no_new_answer", id)
		writeError(w, http.StatusConflict, "the previous turn is still unresolved; send a new message to continue", map[string]any{"pending": true})
		return
	}
	parsed, err := parseAnswer(last.Content, req.Tools, req.ToolChoice)
	if err != nil {
		writeError(w, http.StatusConflict, "the previous turn could not be read back as a usable reply; send a new message to continue", map[string]any{"pending": true})
		return
	}
	finish := "stop"
	if len(parsed.Calls) > 0 {
		finish = "tool_calls"
	}
	if err := s.journal.commit(id, snap, msgs, parsed.Text, parsed.Calls, snap.Remote, last.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "journal write failed", nil)
		return
	}
	log.Printf("[deepseek] session=%s retry=recovered tool_calls=%d answer_bytes=%d", id, len(parsed.Calls), len(parsed.Text))
	s.respond(w, req, model, parsed, "", finish)
}

func (s *server) fail(w http.ResponseWriter, id, stage string, err error, pending bool) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, deepseek.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, deepseek.ErrRateLimited):
		status = http.StatusTooManyRequests
	}
	kind := classify(err)
	log.Printf("[deepseek] session=%s stage=%s failed kind=%s status=%d pending=%v", id, stage, kind, status, pending)
	extra := map[string]any{}
	if pending {
		extra["pending"] = true
	}
	msg := "deepseek web request failed (" + kind + ")"
	if status == http.StatusUnauthorized {
		msg = "deepseek web token rejected; run deepseek-openai -login"
	}
	if status == http.StatusTooManyRequests {
		msg = "deepseek web rate limit; wait before the next turn"
	}
	writeError(w, status, msg, extra)
}

// classify turns an upstream failure into a content-free reason code.
func classify(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, deepseek.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, deepseek.ErrRateLimited):
		return "rate_limited"
	}
	var se *deepseek.StatusError
	if errors.As(err, &se) {
		return fmt.Sprintf("http_%d", se.Status)
	}
	var ae *deepseek.APIError
	if errors.As(err, &ae) {
		return fmt.Sprintf("api_%d", ae.Code)
	}
	return "transport"
}

func (s *server) respond(w http.ResponseWriter, req completionsRequest, model deepseek.ModelConfig, a answer, reasoning, finish string) {
	respID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": a.Text}
		if reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		if len(a.Calls) > 0 {
			msg["tool_calls"] = a.Calls
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": respID, "object": "chat.completion", "created": created, "model": model.ID,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage": map[string]any{
				"prompt_tokens":     0,
				"completion_tokens": len(a.Text) / 4,
				"total_tokens":      len(a.Text) / 4,
			},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	send := func(delta map[string]any, finishReason *string) {
		chunk := map[string]any{
			"id": respID, "object": "chat.completion.chunk", "created": created, "model": model.ID,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"role": "assistant"}, nil)
	for _, part := range splitRunes(reasoning, 400) {
		send(map[string]any{"reasoning_content": part}, nil)
	}
	for _, part := range splitRunes(a.Text, 400) {
		send(map[string]any{"content": part}, nil)
	}
	if len(a.Calls) > 0 {
		calls := make([]any, 0, len(a.Calls))
		for i, c := range a.Calls {
			calls = append(calls, map[string]any{"index": i, "id": c.ID, "type": c.Type, "function": c.Function})
		}
		send(map[string]any{"tool_calls": calls}, nil)
	}
	send(map[string]any{}, &finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// splitRunes cuts s into chunks of at most n bytes without splitting a rune.
func splitRunes(s string, n int) []string {
	if s == "" {
		return nil
	}
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			_, size := utf8.DecodeRuneInString(s)
			cut = size
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func sessionID(r *http.Request, fallback string) string {
	for _, name := range []string{"x-opencode-session-id", "X-Session-Id", "x-opencode-session", "x-session-affinity"} {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, extra map[string]any) {
	body := map[string]any{"error": map[string]any{"message": msg, "type": "bridge_error", "code": status}}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}
