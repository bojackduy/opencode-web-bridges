// gemini-openai translates Gemini Web replies into OpenAI chat completions.
//
// Minimal Phase-1 adapter: stateful sessions (X-Session-Id affinity),
// non-streaming JSON plus buffered SSE, and the exact <opencode_bridge_call>
// tool protocol from chatgpt-proxy so the plugin treats both engines
// identically. Buffered SSE means errors before validation are HTTP JSON
// errors, never partial streams.
//
// Stateful thread architecture (thread = context): Gemini's server keeps the
// conversation. Continuation sends only new messages (delta) when the incoming
// history extends the stored canonical prefix; divergent history starts a new
// chat and replays the history as bounded turns (per-turn cap -max-chars,
// single-submission ceiling -max-paste-bytes): history as inert ACK imports,
// then one final answer-seeking turn.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bojackduy/bridgecore"
	"github.com/bojackduy/gemini-proxy/internal/gemini"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8789", "listen address")
	port := flag.Int("chrome-port", gemini.DefaultChromePort, "Chrome debugging port")
	profile := flag.String("profile", defaultProfile(), "Chrome profile")
	chrome := flag.String("chrome", "", "Chrome binary")
	background := flag.Bool("background", false, "minimize Chrome")
	model := flag.String("model", "gemini-web", "model ID")
	maxChars := flag.Int("max-chars", 200000, "maximum bytes per Gemini turn")
	maxPaste := flag.Int("max-paste-bytes", 0, "maximum bytes one submission may carry; above this the bridge refuses before sending anything (0 disables, per-turn budget alone applies)")
	maxImages := flag.Int("max-images", gemini.MaxResponseImages, "maximum output images carried as image_url parts per turn")
	maxImageBytes := flag.Int("max-image-bytes", gemini.MaxResponseImageBytes, "maximum decoded bytes accepted per output image")
	state := flag.String("state", filepath.Join(defaultProfile(), "openai-sessions.json"), "persistent session state")
	flag.Parse()

	// Output-image budgets (documented defaults live beside the extractor).
	// Oversized/over-count images degrade to omit-notes in the text; the
	// turn itself never fails for image reasons.
	gemini.MaxResponseImages = *maxImages
	gemini.MaxResponseImageBytes = *maxImageBytes

	cg := gemini.New(*port, *profile, *chrome)
	cg.Background = *background
	defer cg.Close()
	h := &handler{client: cg, model: *model, maxChars: *maxChars, maxPaste: *maxPaste, statePath: *state, profileDir: *profile, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	if err := h.load(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *addr, Handler: h.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("[openai] listening=%s model=%s state=%s chrome_port=%d", *addr, *model, *state, *port)
	log.Print("[openai] REPAIR: sign in in ~/.gemini-proxy Chrome; CDP: localhost:9223/json/version; DOM selectors: internal/gemini/gemini.go")
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func defaultProfile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini-proxy")
}

// --- wire types (OpenAI-compatible subset) ------------------------------------

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
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// --- session state ------------------------------------------------------------

// session stores one client's thread: canonical history prefix (what Gemini
// has already seen), the last answer, and the last completion identity for
// exact-retry duplicate-cache.
type session struct {
	Pending      bool                   `json:"pending,omitempty"`
	Messages     []message              `json:"messages"`
	LastAnswer   string                 `json:"last_answer,omitempty"`
	LastImages   []gemini.ResponseImage `json:"last_images,omitempty"`
	LastReqHash  string                 `json:"last_req_hash,omitempty"`
	LastRespID   string                 `json:"last_resp_id,omitempty"`
	LastCreated  int64                  `json:"last_created,omitempty"`
	LastToolIDs  []string               `json:"last_tool_ids,omitempty"`
	NewSessionAt int64                  `json:"new_session_at"`
	// URL is the owned Gemini chat/thread URL submissions went to. It
	// ties sent-log entries and future recovery reads to the thread.
	URL string `json:"url,omitempty"`
	// ToolsFP fingerprints the tool set whose schemas were last taught to
	// this thread. A regenerated system prompt with a CHANGED tool set is
	// mechanical client regen (delta-continue with a reminder); the same
	// tools with a rewritten system is an authored rewrite (reseed).
	ToolsFP string `json:"tools_fp,omitempty"`
	// PendingPrompt is the exact prompt bytes of the uncertain
	// submission. Its promptHash is the lineage key the sent-log
	// diagnosis and any future adoption comparison use.
	PendingPrompt string `json:"pending_prompt,omitempty"`
	// PendingMessages/Tools/Choice/ReqHash bind the uncertain turn to
	// its exact original request, so recovery can validate a thread
	// answer against the pending (not current) schemas and resume the
	// exact-retry duplicate cache after a transplant.
	PendingMessages []message `json:"pending_messages,omitempty"`
	PendingTools    []toolDef `json:"pending_tools,omitempty"`
	PendingChoice   any       `json:"pending_choice,omitempty"`
	PendingReqHash  string    `json:"pending_req_hash,omitempty"`
	// AutoResyncEvidence records a hash-lineage auto-resync transplant:
	// what was transplanted, from which thread, and why it was deemed
	// safe. Persisted so the evidence survives adapter restart.
	AutoResyncEvidence string `json:"auto_resync_evidence,omitempty"`
	// ReseedFrom/ReseedEvidence record a history-rewrite reseed: the
	// abandoned thread and the classification + carried/dropped lists.
	// Persisted so the linkage survives adapter restart.
	ReseedFrom     string `json:"reseed_from,omitempty"`
	ReseedEvidence string `json:"reseed_evidence,omitempty"`
}

// browserClient is the browser surface the adapter drives: submission plus
// the read-only recovery readers. *gemini.Client satisfies it; tests inject
// fakes. Recovery readers never type, submit, navigate, or close tabs.
type browserClient interface {
	Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error)
	CurrentURL(ctx context.Context) string
	LoginState(ctx context.Context) (*gemini.Status, error)
	ReadThreadTurn(ctx context.Context, url, prompt string) (gemini.TurnResult, error)
	ReadLatestThreadTurn(ctx context.Context, url string) (string, gemini.TurnResult, error)
}

// imageAsker is the optional rich-submission surface: *gemini.Client
// implements it, returning the answer text plus retrievable output-image
// parts. Handlers prefer it when available and fall back to Ask otherwise,
// so older fakes keep working. Collection is read-only DOM/CDP (same channel
// discipline as the rest of the bridge: no new navigation or session-state
// side effects).
type imageAsker interface {
	AskWithImages(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, []gemini.ResponseImage, error)
}

// askTurn submits one turn via the richest surface the client offers.
func (h *handler) askTurn(ctx context.Context, prompt string, fresh bool) (string, []gemini.ResponseImage, error) {
	return h.askTurnOnThread(ctx, prompt, "", fresh)
}

// threadRichAsker is the thread-bound rich-submission surface:
// *gemini.Client implements it. Handlers prefer it when available so every
// continuation chunk is restored to and verified on the saved thread inside
// the same tab lock that submits.
type threadRichAsker interface {
	AskWithImagesThread(ctx context.Context, prompt, threadURL string, newSession bool, timeout time.Duration) (string, []gemini.ResponseImage, error)
}

// threadPlainAsker is the thread-bound plain-submission surface.
type threadPlainAsker interface {
	AskThread(ctx context.Context, prompt, threadURL string, newSession bool, timeout time.Duration) (string, error)
}

// askTurnOnThread submits one turn bound to threadURL. Fresh turns ignore
// the binding (the browser mints the thread); continuation turns require a
// valid binding and a thread-aware client — otherwise it fails closed with
// zero typing and zero sends, never falling back to an unbound submit.
func (h *handler) askTurnOnThread(ctx context.Context, prompt, threadURL string, fresh bool) (string, []gemini.ResponseImage, error) {
	if fresh {
		// Fresh chats mint their thread. The rich surface wins when
		// offered (it mints exactly like the thread-aware fresh path);
		// otherwise the thread-aware surfaces mint so later chunks can
		// bind the minted URL.
		if ia, ok := h.client.(imageAsker); ok {
			return ia.AskWithImages(ctx, prompt, true, 10*time.Minute)
		}
		if ia, ok := h.client.(threadRichAsker); ok {
			return ia.AskWithImagesThread(ctx, prompt, "", true, 10*time.Minute)
		}
		if pa, ok := h.client.(threadPlainAsker); ok {
			answer, err := pa.AskThread(ctx, prompt, "", true, 10*time.Minute)
			return answer, nil, err
		}
		answer, err := h.client.Ask(ctx, prompt, true, 10*time.Minute)
		return answer, nil, err
	}
	if !gemini.ValidThreadURL(threadURL) {
		return "", nil, fmt.Errorf("thread binding missing-or-invalid-target: %w", gemini.ErrThreadBinding)
	}
	if ia, ok := h.client.(threadRichAsker); ok {
		return ia.AskWithImagesThread(ctx, prompt, threadURL, false, 10*time.Minute)
	}
	if pa, ok := h.client.(threadPlainAsker); ok {
		answer, err := pa.AskThread(ctx, prompt, threadURL, false, 10*time.Minute)
		return answer, nil, err
	}
	return "", nil, fmt.Errorf("thread binding binding-unsupported: %w", gemini.ErrThreadBinding)
}

// assistantContent builds the assistant message payload: plain string for
// text-only turns (wire-identical to before), multipart text + image_url
// parts when retrievable output images exist.
func assistantContent(answer string, images []gemini.ResponseImage) any {
	if len(images) == 0 {
		return answer
	}
	parts := []any{map[string]any{"type": "text", "text": answer}}
	for _, img := range images {
		if strings.TrimSpace(img.URL) == "" {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": img.URL},
		})
	}
	if len(parts) == 1 {
		return answer
	}
	return parts
}

// streamTextOf extracts the text deltas for buffered SSE. Image parts ride
// only the non-streaming completion; the streamed text already carries the
// fixtures and notes describing them.
func streamTextOf(content any) []string {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	raw, _ := json.Marshal(content)
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return out
}

type handler struct {
	client     browserClient
	model      string
	maxChars   int
	maxPaste   int
	statePath  string
	profileDir string

	mu       sync.Mutex // guards sessions + persistence
	sessions map[string]*session

	// sentLog is the bounded in-memory ring of the append-only sent-log
	// (cmd/gemini-openai/sentlog.go). The file is the durable record;
	// diagnosis falls back to its tail after a restart.
	sentLog []sentEntry

	// adoptWait bounds Dirty-path recovery polling; zero means the 90s
	// production default. Tests shrink it for fast ambiguous cases.
	adoptWait time.Duration

	gate      chan struct{} // whole-turn serial gate (Phase 1: strictly serial)
	activeSID string
	hasActive bool
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

// saveLocked persists sessions atomically (write temp + rename, mode 0600).
// Callers must hold h.mu.
func (h *handler) saveLocked() error {
	raw, err := json.Marshal(h.sessions)
	if err != nil {
		log.Printf("[openai] save marshal: %v", err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.statePath), 0o700); err != nil {
		log.Printf("[openai] save mkdir: %v", err)
		return err
	}
	tmp := h.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[openai] save write: %v", err)
		return err
	}
	if err := os.Rename(tmp, h.statePath); err != nil {
		log.Printf("[openai] save rename: %v", err)
		return err
	}
	return nil
}

// --- routes --------------------------------------------------------------------

func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// /readyz probes the tab WITHOUT navigating: logged-in + composer
	// present. 503 while the page hydrates or the user is signed out.
	//
	// An uncertain pending turn is journal truth about ONE identity, so it is
	// reported as such: the provider stays ready for every other identity and
	// says honestly how many sessions are pending (that blanket refusal is
	// what made one unresolvable record look like a provider-wide outage).
	// A probe that names a pending identity gets a not-ready answer for that
	// identity. The pending set needs no browser, so it is read before the
	// gate.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		pendingIDs := h.pendingSessionIDs()
		if sid := readySessionID(r); sid != "" && containsString(pendingIDs, sid) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(readyBody(false, "pending uncertain turn; inspect owned chat", pendingIDs))
			return
		}
		// Do not wait behind a browser turn and misreport its deadline as auth
		// failure. Readiness is a probe, never a competing browser operation.
		select {
		case <-h.gate:
			defer func() { h.gate <- struct{}{} }()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(readyBody(false, "browser turn busy", pendingIDs))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		st, err := h.client.LoginState(ctx)
		if err != nil || !st.LoggedIn || !st.Composer {
			w.WriteHeader(http.StatusServiceUnavailable)
			detail := "not ready"
			if err != nil {
				detail = err.Error()
			} else if !st.LoggedIn {
				detail = "not logged in to gemini.google.com"
			}
			_ = json.NewEncoder(w).Encode(readyBody(false, detail, pendingIDs))
			return
		}
		_ = json.NewEncoder(w).Encode(readyBody(true, readyPendingDetail(pendingIDs), pendingIDs))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id": h.model, "object": "model",
				"created": time.Now().Unix(), "owned_by": "gemini-web",
			}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", h.chatCompletions)
	mux.HandleFunc("/v1/sessions/abandon", h.abandonSessions)
	mux.HandleFunc("/v1/sessions/recover", h.recoverSession)
	mux.HandleFunc("/v1/sessions/state", h.sessionState)
	return mux
}

// pendingSessionIDs lists the identities holding an uncertain pending turn,
// sorted so readiness bodies are stable. It takes h.mu; callers hold nothing.
func (h *handler) pendingSessionIDs() []string {
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

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// readySessionID resolves the identity a readiness probe asks about. The GUI
// sends x-opencode-session-id; the completion path also honours X-Session-Id /
// X-Opencode-Session / X-Session-Affinity. A probe using any of them is
// answered for its own identity instead of for the provider as a whole. Empty
// means "no identity given": the provider answers for itself.
func readySessionID(r *http.Request) string {
	return sessionID(r)
}

// readyBody reports readiness without hiding pending sessions: the count rides
// every answer and the identities are named, so a client can never be told
// everything is fine while an uncertain turn sits in the journal.
func readyBody(ready bool, detail string, pendingIDs []string) map[string]any {
	if detail == "" {
		detail = "no pending sessions"
	}
	body := map[string]any{"ready": ready, "pending": len(pendingIDs), "detail": detail}
	if len(pendingIDs) > 0 {
		body["pending_sessions"] = pendingIDs
	}
	return body
}

// readyPendingDetail describes a serving provider that still holds pending
// identities: each of them refuses only its own requests.
func readyPendingDetail(pendingIDs []string) string {
	if len(pendingIDs) == 0 {
		return "no pending sessions"
	}
	return fmt.Sprintf("%d session(s) hold an uncertain pending turn and refuse only their own requests; inspect the owned chat, or clear an unresolvable record explicitly via POST /v1/sessions/abandon", len(pendingIDs))
}

// currentThreadURL is a best-effort read-only probe of the owned tab's URL
// for sent-log thread linkage. It never types, submits, or navigates;
// "" means the browser was unreachable, not that no thread exists.
func (h *handler) currentThreadURL(ctx context.Context) string {
	if h.client == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return h.client.CurrentURL(ctx)
}

// sessionID resolves the actual child identity first. OpenCode v2 sends the
// child in x-opencode-session-id; the other headers may identify its parent
// or legacy affinity and must not silently merge separate child transcripts.
// Empty means isolated fresh chat.
func sessionID(r *http.Request) string {
	for _, k := range []string{"X-Opencode-Session-Id", "X-Session-Id", "X-Opencode-Session", "X-Session-Affinity"} {
		if v := strings.TrimSpace(r.Header.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func reqHash(req *chatRequest) string {
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// toolsFingerprint identifies a tool set for change detection: a
// regenerated system prompt with a different fingerprint is mechanical
// client regen, not an authored rewrite.
func toolsFingerprint(tools []toolDef) string {
	raw, _ := json.Marshal(tools)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func textOf(m message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		// SDK text-part arrays: concatenate text parts, ignore media here
		// (Phase 1 is text-only; media parts are rejected below).
		raw, _ := json.Marshal(c)
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return string(raw)
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || (p.Type == "" && p.Text != "") {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
}

func hasMedia(req *chatRequest) bool {
	for _, m := range req.Messages {
		if s, ok := m.Content.(string); ok || m.Content == nil {
			_ = s
			continue
		}
		raw, _ := json.Marshal(m.Content)
		var parts []struct {
			Type     string `json:"type"`
			ImageURL any    `json:"image_url"`
			VideoURL any    `json:"video_url"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.ImageURL != nil || p.VideoURL != nil ||
				p.Type == "image_url" || p.Type == "input_image" || p.Type == "input_video" {
				return true
			}
		}
	}
	return false
}

func toolSchemas(tools []toolDef) (map[string]*jsonschema.Schema, error) {
	out := map[string]*jsonschema.Schema{}
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(localSchemas{})
		// Normalize struct/map schemas to JSON values expected by the validator.
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

// localSchemas disables external $ref downloads; local references work.
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
		return v // "auto" | "none" | "required"
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
		return "auto"
	default:
		rawJSON, _ := json.Marshal(raw)
		var m map[string]any
		if json.Unmarshal(rawJSON, &m) == nil {
			if fn, ok := m["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					return name
				}
			}
		}
		return "auto"
	}
}

// canonicalEqual compares two messages the way the adapter's stored prefix
// does: role + name + text + tool identity, ignoring equivalent JSON
// argument encodings.
func canonicalEqual(a, b message) bool {
	if a.Role != b.Role || a.Name != b.Name || a.ToolCallID != b.ToolCallID {
		return false
	}
	if textOf(a) != textOf(b) {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID ||
			a.ToolCalls[i].Function.Name != b.ToolCalls[i].Function.Name ||
			!jsonEqual(a.ToolCalls[i].Function.Arguments, b.ToolCalls[i].Function.Arguments) {
			return false
		}
	}
	return true
}

func jsonEqual(a, b string) bool {
	if a == b {
		return true
	}
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return false
	}
	ra, _ := json.Marshal(va)
	rb, _ := json.Marshal(vb)
	return string(ra) == string(rb)
}

// systemPrompt renders the tool-teaching preamble for a fresh thread.
func systemPrompt(tools []toolDef, choice string) string {
	if len(tools) == 0 {
		return ""
	}
	raw, _ := json.Marshal(tools)
	return "You are a tool-using assistant. Available functions (JSON schemas):\n" + string(raw) +
		"\ntool_choice: " + choice + "\n" +
		"To call a function, reply with ONLY one or more <opencode_bridge_call>{\"name\":\"...\",\"arguments\":{...}}</opencode_bridge_call> wrappers and no other text. " +
		"Otherwise reply in plain text."
}

func (h *handler) writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Every refusal lands one terminal line: status plus reason. Refusals
	// used to be silent, leaving only user-visible text to diagnose from.
	log.Printf("[openai] refused status=%d code=%s msg=%.300s", status, code, msg)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": code},
	})
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
	if hasMedia(&req) {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "media input is not supported in Phase 1 (text only)")
		return
	}
	schemas, err := toolSchemas(req.Tools)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	choice := effectiveToolChoice(req.ToolChoice, len(req.Tools))

	sid := sessionID(r)
	hash := reqHash(&req)

	// Whole-turn serial gate: one browser transaction at a time.
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
	{
		thread := "none"
		if sess.URL != "" {
			thread = "stored"
		}
		log.Printf("[openai] session=%s request in=%d saved=%d thread=%s pending=%v", shortSID(sid), len(req.Messages), len(sess.Messages), thread, sess.Pending)
	}
	// Exact last-request retry: same bytes, same completion identity.
	// The cached answer is re-validated (schemas may have changed) and the
	// assistant message rebuilt deterministically with the stored IDs.
	if sess.LastReqHash == hash && sess.LastRespID != "" {
		calls, verr := bridgecore.ExtractExecutableCalls(sess.LastAnswer, schemas, choice)
		if verr != nil {
			diag := h.sentDiagnosis(sid, sess)
			h.mu.Unlock()
			h.writeError(w, http.StatusUnprocessableEntity, "invalid_request_error",
				"cached answer failed tool-protocol validation: "+verr.Error()+"; "+diag)
			return
		}
		resp := h.buildCompletion(&req, sess.LastAnswer, sess.LastImages, calls, sess.LastToolIDs, sess.LastCreated, sess.LastRespID)
		h.mu.Unlock()
		h.writeChatResponse(w, &req, resp, "")
		return
	}
	var reseedPlan *rewritePlan
	// dirtyUncertain marks a session that abandoned an uncertain turn
	// above: its old thread may hold an unverifiable submission, so it
	// can never be appended to — only reseeded or continued by adoption.
	dirtyUncertain := false
	// appending marks a compaction-append: the tail below the mismatch is
	// submitted onto the stored thread instead of replayed into a new one.
	appending := false
	appendStart := 0
	if sess.Pending {
		// A record with no binding at all cannot prove what was submitted, so
		// it is refused fast and names the operator exit.
		unbound := len(sess.PendingMessages) == 0 && sess.PendingPrompt == ""
		// Model/schema changes alone do not establish new input. Keep exact
		// uncertain retries fenced without invoking implicit browser recovery.
		same := len(sess.PendingMessages) == 0 || len(req.Messages) == len(sess.PendingMessages)
		if len(sess.PendingMessages) != 0 && same {
			for i := range req.Messages {
				if !canonicalEqual(req.Messages[i], sess.PendingMessages[i]) {
					same = false
					break
				}
			}
		}
		if same || !coherentTranscript(req.Messages) {
			msg := "previous submission is uncertain; use explicit recovery to read it back, or send a distinct request; no automatic resubmission"
			if unbound {
				msg = "pending uncertain turn; inspect owned chat, or clear an unresolvable record explicitly via POST /v1/sessions/abandon; no automatic resubmission"
			}
			// The refusal keeps the sent-log lineage line: without it the
			// operator cannot tell a lost submission from an overwritten
			// pending prompt before choosing recovery or abandon.
			h.mu.Unlock()
			h.writeError(w, http.StatusUnprocessableEntity, "pending_turn", msg+"; "+h.sentDiagnosis(sid, sess))
			return
		}
		// Best-effort fast adoption first: an interrupted turn often completed
		// (cancels routinely land after the answer exists). A bounded
		// read-only adopt commits it and the request below continues delta
		// on the same thread — no replay. Anything unadoptable falls
		// through to the reseed path unchanged. Nothing is submitted and
		// nothing executes here either way; the unlock brackets the browser
		// wait (recordCompletedTurn takes the lock itself).
		h.mu.Unlock()
		adopted, _ := h.tryAdoptInterruptedWait(r, sid, sess, h.dirtyAdoptWait())
		h.mu.Lock()
		if adopted {
			log.Printf("[openai] session=%s dirty-adopted interrupted turn; continuing without reseed", shortSID(sid))
		} else {
			// Context continuity is best effort: the client's history is
			// authoritative. Record what is abandoned (executed exchanges are
			// carried inertly; the undelivered turn is listed, never resent).
			// The adopt attempt above already proved nothing readable exists,
			// so no unverifiable answer gates the new request. Work on a copy
			// until prompt validation succeeds.
			reseedPlan = h.planDirtyReseed(sid, sess, req.Messages)
			dirtyUncertain = true
			next := *sess
			next.URL, next.Messages = "", nil
			next.LastReqHash, next.LastRespID = "", ""
			clearPendingBinding(&next)
			sess = &next
			log.Printf("[openai] continuation reason=uncertain_branch incoming_messages=%d planned=%v", len(req.Messages), reseedPlan != nil)
		}
	}
	// Delta vs fresh: incoming history extending the stored prefix sends
	// only the suffix; anything else starts a new chat and replays the
	// history as bounded turns. A reseed plan skips delta: the stored
	// prefix belongs to the abandoned thread.
	var seedMsgs []message
	var recordMsgs []message
	var sysHead string
	fresh := true
	// Delta when the incoming history extends the stored prefix EXACTLY,
	// plus two narrow exceptions: assistant echo bloat, and regenerated
	// system prompts. The client
	// re-echoes our just-committed answers with appended re-render notes
	// (live: ~637B pure appends); treating that as a rewrite reseeds a
	// new chat every turn. echoBloatSkipped absorbs assistant-only appends
	// up to 1024B — roles, names, call IDs and tool calls stay exact, and
	// user/system/tool divergences of any size stay significant, so a
	// genuine edit is never silently dropped. (Former broad tolerance for
	// all roles is removed for exactly that reason.)
	// Location proof is the per-transaction thread binding at submit time,
	// not the in-memory slot: hasActive/activeSID never proves which
	// conversation the tab shows, so it plays no role here. A prefix match
	// with a valid saved URL attempts the delta; if the tab cannot be
	// restored to that URL the submit fails closed — never a silent new
	// chat.
	deltaKind := ""
	if reseedPlan == nil && len(sess.Messages) > 0 && firstMismatch(req.Messages, sess.Messages) == nil {
		deltaKind = "exact"
	} else if reseedPlan == nil && len(sess.Messages) > 0 {
		if ok, skipped := echoBloatSkipped(sess.Messages, req.Messages, 1024); ok {
			if skipped > 0 {
				log.Printf("[openai] session=%s tolerant-prefix skipped=%dB echo-bloat absorbed", shortSID(sid), skipped)
			}
			deltaKind = "echo"
		} else if systemOnlyDivergence(sess.Messages, req.Messages) && sess.ToolsFP != "" && toolsFingerprint(req.Tools) != sess.ToolsFP {
			// Regenerated system prompt over untouched conversation AND a
			// changed tool set versus the RECORDED one: mechanical client
			// regen (the tool catalog regenerates the whole system
			// message), not an authored rewrite — the thread keeps
			// operating under its stored instructions while the
			// recomputed reminder below teaches the new tool names. An
			// unrecorded tool set (legacy journal) or identical tools
			// with a rewritten system stays a reseed: those combinations
			// are authored rewrites or unknowns, never assumed mechanical.
			log.Printf("[openai] session=%s system-prompt regenerated tools-changed; continuing delta under stored instructions", shortSID(sid))
			deltaKind = "system"
		}
	}
	if deltaKind != "" {
		if gemini.ValidThreadURL(sess.URL) {
			fresh = false
			if teach := h.teachReminder(req, sess); teach != "" {
				sysHead = teach + "\n\n"
			}
			seedMsgs = req.Messages[len(sess.Messages):]
		} else {
			log.Printf("[openai] session=%s delta-refused reason=missing-or-invalid-target incoming_messages=%d saved_messages=%d",
				shortSID(sid), len(req.Messages), len(sess.Messages))
		}
	}
	if fresh {
		sysHead = ""
		if sys := systemPrompt(req.Tools, choice); sys != "" {
			sysHead = sys + "\n\n"
		}
		// History-rewrite reseed: the incoming history is a coherent
		// rewrite (revert/fork/compaction) of committed state, so seed
		// a fresh chat with the rewritten timeline plus carried
		// executed results instead of replaying blind. Incoherent input
		// plans nothing and keeps the legacy replay below (clean
		// sessions hold no uncertainty to protect).
		// Audit (2026-10-16): no legacy rotation path exists on this
		// bridge, so the reseed plan can never be shadowed — this single
		// mismatch path always carries the record and evidence.
		if reseedPlan == nil && len(sess.Messages) > 0 && sess.URL != "" {
			if mm := firstMismatch(req.Messages, sess.Messages); mm != nil {
				log.Printf("[openai] session=%s prefix_mismatch index=%d field=%s confoff=%d incoming_messages=%d saved_messages=%d incoming_shape=%s saved_shape=%s",
					shortSID(sid), mm.Index, mm.Field, mm.ContentOff, len(req.Messages), len(sess.Messages), mm.IncomingShape, mm.SavedShape)
				reseedPlan = planCleanReseed(sess, req.Messages)
				if reseedPlan != nil {
					log.Printf("[openai] session=%s rewrite_reseed classification=%s carried=%d not_carried=%d old=%s",
						shortSID(sid), reseedPlan.classification, len(reseedPlan.carriedIDs), len(reseedPlan.notCarried), reseedPlan.oldURL)
				}
			}
		}
		if reseedPlan != nil {
			recordMsgs = reseedPlan.carried
		}
		// Compaction with a huge shared prefix: the stored thread already
		// holds ~everything before the summary, so appending the tail onto
		// it costs a few pastes instead of a full-thread replay into a new
		// thread. The summary is meant to supersede the old text; keeping
		// both costs one stale message of context against recency, which
		// wins. Never on an uncertain session, never for non-content
		// rewrites, never without sent-log proof — forks, truncations
		// and anything unproven keep the reseed path below. Carried
		// executed exchanges are NOT re-imported: the stored thread
		// already holds them; they are only listed in the evidence.
		if reseedPlan != nil && reseedPlan.classification == rewriteCompaction && !dirtyUncertain &&
			reseedPlan.mismatchField == "content" && len(sess.Messages) > 0 &&
			reseedPlan.mismatchIndex >= len(sess.Messages)*9/10 &&
			coherentTranscript(req.Messages) && h.sentEntryForThread(sid, sess.URL) != nil {
			appending = true
			appendStart = reseedPlan.mismatchIndex
			fresh = false
			seedMsgs = req.Messages[appendStart:]
			recordMsgs = nil
			if teach := h.teachReminder(req, sess); teach != "" {
				sysHead = teach + "\n\n"
			}
			log.Printf("[openai] session=%s compaction-append mismatch=%d prefix=%d/%d thread=%s",
				shortSID(sid), appendStart, appendStart, len(sess.Messages), sess.URL)
		} else {
			seedMsgs = req.Messages
		}
		// Every new thread is explicitly authorized: genuine fresh (no
		// committed history) or a classified history rewrite. Anything
		// else reaching here is the legacy incoherent-input replay.
		// Compaction-append mints nothing, so it logs nothing here.
		if !appending {
			reason := "legacy-replay"
			switch {
			case len(sess.Messages) == 0:
				reason = "genuine-fresh"
			case reseedPlan != nil:
				reason = "reseed-" + reseedPlan.classification
			}
			log.Printf("[openai] session=%s new-thread-authorized reason=%s incoming_messages=%d saved_messages=%d",
				shortSID(sid), reason, len(req.Messages), len(sess.Messages))
		}
	}
	// Chunk into bounded turns: carried record first (inert ACK imports),
	// then history imports plus one final answer-seeking turn. Single-turn
	// assembly is byte-identical to the legacy one-paste path.
	limit := h.pasteLimit()
	recordTurns := h.buildGeminiRecordTurns(recordMsgs, limit)
	turns, terr := h.buildGeminiTurns(sysHead, seedMsgs, limit)
	if terr != nil {
		h.mu.Unlock()
		h.writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", terr.Error())
		return
	}
	allTurns := append(recordTurns, turns...)
	seedBase := 0
	if !fresh {
		seedBase = len(sess.Messages)
		if appending {
			// The seed starts at the mismatch, below the stored prefix
			// end: synced offsets map into the incoming history from
			// there, not from the stored length.
			seedBase = appendStart
		}
	}
	// One choke point: every turn obeys the paste ceiling before anything
	// is journaled or submitted.
	sizes := make([]int, len(allTurns))
	replays := make([]bool, len(allTurns))
	for i, t := range allTurns {
		sizes[i], replays[i] = len(t.prompt), t.replay
		if len(t.prompt) > limit && limit > 0 {
			h.mu.Unlock()
			h.writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("turn %d/%d needs a %d-byte paste over the %d-byte single-paste limit. Nothing was sent and no tool was run.", i+1, len(allTurns), len(t.prompt), limit))
			return
		}
	}
	log.Printf("[openai] session=%s plan turns=%d sizes=%v replay=%v paste_limit=%d seed=%d roles=%s record=%d", shortSID(sid), len(allTurns), sizes, replays, limit, len(seedMsgs), roleInitials(seedMsgs), len(recordMsgs))
	if len(allTurns) == 0 {
		h.mu.Unlock()
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "no submission turns")
		return
	}
	canonical := append([]message{}, req.Messages...)
	finalPrompt := allTurns[len(allTurns)-1].prompt
	sess.Pending = true
	sess.PendingPrompt = finalPrompt
	sess.PendingMessages = canonical
	sess.PendingTools = req.Tools
	sess.PendingChoice = req.ToolChoice
	sess.PendingReqHash = hash
	h.sessions[sid] = sess
	h.saveLocked()
	h.mu.Unlock()

	// Per-chunk binding: the saved thread URL is the submit target for
	// every continuation chunk. The journal URL is never overwritten here
	// with a pre-submit probe (a rebuilt tab reports the homepage, which
	// must not become the session's thread). Fresh turns mint their thread
	// on chunk 1; later chunks bind to the minted URL captured below.
	bound := sess.URL

	started := time.Now()
	fid := gemini.NewFlowID()
	var answer string
	var images []gemini.ResponseImage
	for i, t := range allTurns {
		turnFresh := fresh && i == 0
		kind := "final"
		if t.replay {
			kind = "replay"
		}
		if !turnFresh && !gemini.ValidThreadURL(bound) {
			// Binding failed before anything was typed or sent: no new
			// chat, no replay, transcript and URL retained.
			h.mu.Lock()
			clearPendingBinding(sess)
			h.appendSent(sentEntry{Event: "failed", Session: sid, URL: sess.URL, PromptHash: promptHash(t.prompt), Bytes: len(t.prompt), Chunk: i + 1, Chunks: len(allTurns), Note: "binding missing-or-invalid-target kind=" + kind})
			h.saveLocked()
			h.mu.Unlock()
			log.Printf("[openai] session=%s thread-bind-refused reason=missing-or-invalid-target turn=%d/%d kind=%s", shortSID(sid), i+1, len(allTurns), kind)
			h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: thread binding missing-or-invalid-target; nothing was sent and no new chat was started.")
			return
		}
		log.Printf("[gemini-flow] id=%s session=%s stage=request turn=%d/%d replay=%v fresh=%v prompt_bytes=%d tools=%d stream=%v", fid, shortSID(sid), i+1, len(allTurns), t.replay, turnFresh, len(t.prompt), len(req.Tools), req.Stream)
		log.Printf("[openai] session=%s stage=browser-start turn=%d/%d fresh=%v prompt_bytes=%d tools=%d", shortSID(sid), i+1, len(allTurns), turnFresh, len(t.prompt), len(req.Tools))
		// Intent is journaled per imminent chunk — replay vs final
		// distinguished — with a durable save BEFORE submission, so the
		// log never claims the final turn submitted while only history
		// imports are in flight.
		h.mu.Lock()
		h.appendSent(sentEntry{Event: "submitted", Session: sid, URL: bound, PromptHash: promptHash(t.prompt), Bytes: len(t.prompt), Chunk: i + 1, Chunks: len(allTurns), Note: "kind=" + kind})
		h.saveLocked()
		h.mu.Unlock()
		var turnImages []gemini.ResponseImage
		var terr error
		answer, turnImages, terr = h.askTurnOnThread(gemini.WithFlowID(gemini.WithTurnIdentity(r.Context(), sid+"\x00"+hash), fid), t.prompt, bound, turnFresh)
		if terr != nil {
			log.Printf("[gemini-flow] id=%s session=%s stage=browser_result turn=%d/%d result=error duration=%s not_submitted=%v err=%v", fid, shortSID(sid), i+1, len(allTurns), time.Since(started).Round(time.Millisecond), errors.Is(terr, gemini.ErrNotSubmitted), terr)
			log.Printf("[openai] session=%s stage=browser-failed turn=%d/%d duration=%s not_submitted=%v", shortSID(sid), i+1, len(allTurns), time.Since(started).Round(time.Millisecond), errors.Is(terr, gemini.ErrNotSubmitted))
			err = terr
			if errors.Is(err, gemini.ErrNotLoggedIn) || gemini.IsPreSubmit(err) || errors.Is(err, gemini.ErrNoComposer) || gemini.IsNavigateError(err) {
				h.mu.Lock()
				clearPendingBinding(sess)
				h.appendSent(sentEntry{Event: "failed", Session: sid, URL: sess.URL, PromptHash: promptHash(t.prompt), Bytes: len(t.prompt), Chunk: i + 1, Chunks: len(allTurns)})
				h.saveLocked()
				h.mu.Unlock()
			} else {
				// Uncertain outcome: the submission may have reached the
				// thread. PendingPrompt is kept for hash-lineage diagnosis.
				h.mu.Lock()
				h.appendSent(sentEntry{Event: "interrupted", Session: sid, URL: sess.URL, PromptHash: promptHash(t.prompt), Bytes: len(t.prompt), Chunk: i + 1, Chunks: len(allTurns)})
				h.saveLocked()
				h.mu.Unlock()
			}
			if errors.Is(err, gemini.ErrNotLoggedIn) {
				h.writeError(w, http.StatusUnauthorized, "authentication_error", "not logged in to gemini.google.com — run gemini-proxy -wait-login")
				return
			}
			if errors.Is(err, gemini.ErrForeignDraft) || errors.Is(err, gemini.ErrDraftOwned) {
				// Nothing was typed over and nothing was sent, so retrying the same
				// request would only refuse again. The person gets the outcome and
				// the one thing they can do instead of a selector diagnostic.
				unknown := "This chat box holds text this bridge cannot verify it wrote, so it was left untouched."
				next := "Nothing from your previous message was resent. Next: open the dedicated Gemini Web Chrome window and keep or clear that text yourself before sending your message again."
				if errors.Is(err, gemini.ErrDraftOwned) {
					unknown = "This chat box still holds a message from an attempt that may already have been sent, so it was left untouched."
					next = "Do not resend it. Nothing from your previous message was resent and no tools were re-run. Next: open the dedicated Gemini Web Chrome window and read that conversation for the answer; clearing the chat box is a separate manual step there."
				}
				h.writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", unknown+" "+next)
				return
			}
			if errors.Is(err, gemini.ErrThreadBinding) {
				// Binding restore failed (or the tab sat on the wrong
				// conversation): provably nothing was typed or sent. No
				// fallback new chat, no replay — transcript and saved URL
				// are retained, pending is cleared, the person retries.
				log.Printf("[openai] session=%s thread-bind-refused turn=%d/%d kind=%s not_submitted=%v", shortSID(sid), i+1, len(allTurns), kind, gemini.IsPreSubmit(err))
				h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: thread binding failed ("+err.Error()+"); nothing was sent and no new chat was started. The saved thread and transcript are retained; retry the same request.")
				return
			}
			h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: "+err.Error())
			return
		}
		// Chunk succeeded: capture the thread URL for binding the next
		// chunk. Only a valid conversation URL is ever adopted, and on an
		// established delta the saved URL wins over any drift.
		if cur := h.currentThreadURL(r.Context()); gemini.ValidThreadURL(cur) {
			if bound == "" || fresh || (reseedPlan != nil && !appending) {
				bound = cur
			} else if cur != bound {
				log.Printf("[openai] session=%s thread-url-drift-ignored turn=%d/%d", shortSID(sid), i+1, len(allTurns))
			}
		}
		if t.replay {
			// History imports are inert: the model must acknowledge without
			// tool calls. An empty acknowledgement is accepted (the import
			// is the user message, which reached the thread before
			// generation); anything else fails closed without executing.
			if got := strings.TrimSpace(answer); got != "ACK" && got != "" {
				log.Printf("[gemini-flow] id=%s session=%s stage=replay turn=%d/%d result=unexpected-answer", fid, shortSID(sid), i+1, len(allTurns))
				h.mu.Lock()
				h.appendSent(sentEntry{Event: "interrupted", Session: sid, URL: sess.URL, PromptHash: promptHash(t.prompt), Bytes: len(t.prompt), Chunk: i + 1, Chunks: len(allTurns), Note: "history import did not acknowledge with ACK; session remains uncertain"})
				h.saveLocked()
				h.mu.Unlock()
				h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: history import did not acknowledge with ACK; session remains uncertain")
				return
			}
			if got := strings.TrimSpace(answer); got == "" {
				log.Printf("[gemini-flow] id=%s session=%s stage=replay turn=%d/%d result=imported-without-answer", fid, shortSID(sid), i+1, len(allTurns))
			} else {
				log.Printf("[gemini-flow] id=%s session=%s stage=replay turn=%d/%d result=acked", fid, shortSID(sid), i+1, len(allTurns))
			}
			if t.synced > 0 {
				h.mu.Lock()
				sess.Messages = append([]message{}, req.Messages[:seedBase+t.synced]...)
				h.saveLocked()
				h.mu.Unlock()
			}
			continue
		}
		images = turnImages
	}

	// Validate the protocol BEFORE emitting anything (buffered SSE).
	log.Printf("[gemini-flow] id=%s session=%s stage=answer bytes=%d duration=%s", fid, shortSID(sid), len(answer), time.Since(started).Round(time.Millisecond))
	calls, err := bridgecore.ExtractExecutableCalls(answer, schemas, choice)
	if err != nil {
		log.Printf("[gemini-flow] id=%s session=%s stage=protocol_validation result=rejected err=%v", fid, shortSID(sid), err)
		h.mu.Lock()
		// The submission completed (an answer arrived) but failed strict
		// protocol validation: no calls are delivered. The outcome is
		// recorded; the Dirty lifecycle is unchanged (a later recovery
		// batch owns it).
		h.appendSent(sentEntry{Event: "completed", Session: sid, URL: sess.URL, PromptHash: promptHash(finalPrompt), Bytes: len(finalPrompt), Chunk: 1, Chunks: 1, AnswerHash: gemini.AnswerHash(answer), Note: "answer rejected by tool-protocol validation; no calls delivered"})
		diag := h.sentDiagnosis(sid, sess)
		h.saveLocked()
		h.mu.Unlock()
		h.writeError(w, http.StatusUnprocessableEntity, "invalid_request_error",
			"model output failed tool-protocol validation: "+err.Error()+"; "+diag)
		return
	}

	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-gemini-%d", created)
	var toolIDs []string
	for i := range calls {
		toolIDs = append(toolIDs, fmt.Sprintf("call_gemini_%d_%d", created, i))
	}

	h.mu.Lock()
	// Absorb our own answer into the prefix baseline (same shape as the
	// read-back path): otherwise every next turn re-imports the previous
	// answer as history on the thread that already holds it.
	sess.Messages = append(canonical, message{Role: "assistant", Content: assistantContent(answer, images), ToolCalls: h.rebuildToolCalls(calls, toolIDs)})
	sess.ToolsFP = toolsFingerprint(req.Tools)
	sess.LastAnswer = answer
	sess.LastImages = images
	sess.LastReqHash = hash
	sess.LastRespID = respID
	sess.LastCreated = created
	sess.LastToolIDs = toolIDs
	clearPendingBinding(sess)
	// Commit the bound thread URL only: a valid bound URL is adopted for
	// fresh/reseed turns that minted it; an established delta keeps its
	// saved URL. A probe reporting the homepage or anything else never
	// overwrites the journal.
	if gemini.ValidThreadURL(bound) && (sess.URL == "" || fresh || reseedPlan != nil || bound == sess.URL) {
		sess.URL = bound
	} else if bound != sess.URL {
		log.Printf("[openai] session=%s thread-url-retained", shortSID(sid))
	}
	h.appendSent(sentEntry{Event: "completed", Session: sid, URL: sess.URL, PromptHash: promptHash(finalPrompt), Bytes: len(finalPrompt), Chunk: 1, Chunks: 1, AnswerHash: gemini.AnswerHash(answer)})
	if reseedPlan != nil {
		// The commit records the linkage: abandoned thread, rewrite
		// classification, and carried/dropped lists — in the journal
		// and the sent-log, so both survive adapter restart. A
		// compaction-append continued the stored thread instead of
		// abandoning it, so it records same-thread evidence.
		if appending {
			sess.ReseedFrom = reseedPlan.oldURL
			sess.ReseedEvidence = reseedPlan.appendEvidence(sess.URL, len(finalPrompt))
			h.appendSent(sentEntry{Event: "append", Session: sid, URL: sess.URL, PromptHash: promptHash(finalPrompt), Bytes: len(finalPrompt), Note: reseedPlan.note(sess.URL)})
			log.Printf("[openai] session=%s compaction-append committed %s", shortSID(sid), sess.ReseedEvidence)
		} else {
			sess.ReseedFrom = reseedPlan.oldURL
			sess.ReseedEvidence = reseedPlan.evidence(sess.URL, len(finalPrompt))
			h.appendSent(sentEntry{Event: "reseed", Session: sid, URL: sess.URL, PromptHash: promptHash(finalPrompt), Bytes: len(finalPrompt), Note: reseedPlan.note(sess.URL)})
			log.Printf("[openai] session=%s rewrite-reseed committed %s", shortSID(sid), sess.ReseedEvidence)
		}
	}
	h.activeSID, h.hasActive = sid, true
	h.saveLocked()
	h.mu.Unlock()

	log.Printf("[openai] session=%s fresh=%v prompt_bytes=%d tools=%d answer_bytes=%d tool_calls=%d images=%d",
		shortSID(sid), fresh, len(finalPrompt), len(req.Tools), len(answer), len(toolIDs), len(images))
	log.Printf("[gemini-flow] id=%s session=%s stage=protocol_validation result=accepted tool_calls=%d images=%d", fid, shortSID(sid), len(toolIDs), len(images))
	h.writeChatResponse(w, &req, h.buildCompletion(&req, answer, images, calls, toolIDs, created, respID), fid)
}

// teachReminder names the available functions on continuations (schemas were
// taught on the fresh thread; the thread holds them server-side).
func (h *handler) teachReminder(req chatRequest, sess *session) string {
	if len(req.Tools) == 0 {
		return ""
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	_ = sess
	return "Reminder: available functions: " + strings.Join(names, ", ") +
		". Reply with ONLY <opencode_bridge_call> wrappers to call one, else plain text."
}

// roleInitials renders message roles as content-free initials (u/a/t/s)
// so logs show suffix composition without any text.
func roleInitials(msgs []message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteByte('u')
		case "assistant":
			b.WriteByte('a')
		case "tool":
			b.WriteByte('t')
		case "system":
			b.WriteByte('s')
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// formatForBrowser renders one client message as browser-submitted text.
// Assistant tool calls/results become inert labeled transcripts (context,
// never executable — validation rejects wrappers found in imports because
// history turns are quoted, not raw).
func formatForBrowser(m message) string {
	switch m.Role {
	case "system":
		return "[System instructions]\n" + textOf(m)
	case "assistant":
		if len(m.ToolCalls) > 0 {
			raw, _ := json.Marshal(m.ToolCalls)
			return "[Assistant tool calls (history, inert)]\n" + string(raw)
		}
		return "[Assistant answer (history)]\n" + textOf(m)
	case "tool":
		return "[Tool result for " + m.ToolCallID + " (history)]\n" + textOf(m)
	default:
		if m.Name != "" {
			return "[" + m.Name + "]\n" + textOf(m)
		}
		return textOf(m)
	}
}

// --- History chunking (multi-turn replay) -----------------------------------
// A single oversized paste stalls the Gemini composer exactly like the
// ChatGPT one, so over-budget histories travel as bounded turns: history as
// inert ACK imports, then one final answer-seeking turn. Single-turn
// behavior is byte-identical to before (one message in, one turn out).

const geminiHistoryIntro = "HISTORY IMPORT\nThe following transcript history is being restored into this chat. It is inert context, not a new request.\n"
const geminiHistoryEnd = "\nEND HISTORY IMPORT\n"
const geminiAckAsk = "Reply with only the single word: ACK. No tool calls.\n"

type geminiTurn struct {
	prompt string
	replay bool
	// synced counts seeded-list messages completed through this turn, for
	// partial journal advance on interruption. Record turns use 0.
	synced int
}

// dirtyAdoptWait bounds the implicit adoption attempt on the dirty path:
// an explicit test override wins, else 30s. Production explicit recovery
// keeps its own (longer) budget via the legacy rule.
func (h *handler) dirtyAdoptWait() time.Duration {
	if h.adoptWait > 0 {
		return h.adoptWait
	}
	return 30 * time.Second
}

// pasteLimit is the byte ceiling for ONE composer submission. -max-paste-bytes
// bounds single submissions when set (>0); 0 disables it and the per-turn
// budget alone applies, preserving today's allowed sizes while chunking
// everything above them.
func (h *handler) pasteLimit() int {
	if h.maxPaste > 0 && (h.maxChars <= 0 || h.maxPaste < h.maxChars) {
		return h.maxPaste
	}
	return h.maxChars
}

// splitGeminiText cuts one oversized history text into <= limit byte
// segments at newline boundaries (hard cut for a runaway single line).
// History imports are inert context, so a mid-text cut costs fidelity,
// never executability.
func splitGeminiText(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var out []string
	for len(text) > limit {
		cut := strings.LastIndex(text[:limit], "\n")
		if cut <= 0 {
			cut = limit
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// chunkGeminiTexts groups rendered message texts into <= limit byte groups
// for one submission each, splitting single oversized messages. Ends carry
// the message-end index (1-based count into the rendered list) for journal
// sync.
func chunkGeminiTexts(texts []string, limit int) (groups [][]string, ends []int) {
	var cur []string
	curLen := 0
	flush := func(end int) {
		if len(cur) > 0 {
			groups = append(groups, cur)
			ends = append(ends, end)
			cur, curLen = nil, 0
		}
	}
	for i, text := range texts {
		for _, seg := range splitGeminiText(text, limit) {
			if len(cur) > 0 && curLen+len(seg)+1 > limit {
				flush(i)
			}
			cur = append(cur, seg)
			curLen += len(seg) + 1
		}
	}
	flush(len(texts))
	return groups, ends
}

// buildGeminiTurns splits a message list into bounded submissions: history
// (through the last completed assistant exchange) travels as inert ACK
// imports, the remainder asks once as the final turn carrying sysHead. A
// single over-budget question is refused, never split: an interruption
// would strand half a request with no answer that belongs to it.
func (h *handler) buildGeminiTurns(sysHead string, msgs []message, limit int) ([]geminiTurn, error) {
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, formatForBrowser(m)+"\n")
	}
	// Everything through the last completed assistant exchange is history.
	// The remaining user/tool messages are the currently unanswered request.
	cut := 0
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			cut = i + 1
		}
	}
	// Also support supplied transcripts with consecutive user messages.
	for i, m := range msgs {
		if m.Role == "user" && i > cut {
			for _, earlier := range msgs[:i] {
				if earlier.Role == "user" {
					cut = i
					break
				}
			}
		}
	}
	overhead := len(geminiHistoryIntro) + len(geminiHistoryEnd) + len(geminiAckAsk)
	pack := limit
	if pack > 0 {
		pack = limit - overhead
		if pack < 1 {
			pack = 1
		}
	}
	var turns []geminiTurn
	if cut > 0 {
		groups, ends := chunkGeminiTexts(texts[:cut], pack)
		for gi, g := range groups {
			var b strings.Builder
			b.WriteString(geminiHistoryIntro)
			for _, seg := range g {
				b.WriteString(seg + "\n")
			}
			b.WriteString(geminiHistoryEnd + geminiAckAsk)
			turns = append(turns, geminiTurn{prompt: b.String(), replay: true, synced: ends[gi]})
		}
	}
	current := texts[cut:]
	if limit > 0 {
		for _, text := range current {
			if len(text)+1 > limit {
				return nil, fmt.Errorf("one message is %d bytes, over the %d-byte per-turn budget. The bridge will not split a single message across submissions, because an interruption cannot be recovered. Nothing was sent and no tool was run; shorten the message or raise -max-chars", len(text), limit)
			}
		}
	}
	groups, ends := chunkGeminiTexts(current, pack)
	for i, g := range groups {
		last := i == len(groups)-1
		var b strings.Builder
		if last {
			b.WriteString(sysHead)
		} else {
			b.WriteString(geminiHistoryIntro)
		}
		for _, seg := range g {
			b.WriteString(seg + "\n")
		}
		if last {
			turns = append(turns, geminiTurn{prompt: b.String()})
		} else {
			b.WriteString(geminiHistoryEnd + geminiAckAsk)
			turns = append(turns, geminiTurn{prompt: b.String(), replay: true, synced: cut + ends[i]})
		}
	}
	return turns, nil
}

// buildGeminiRecordTurns chunks carried prior-execution exchanges as inert
// ACK imports ahead of the rewritten transcript, mirroring the ChatGPT
// record-first path. Never pending calls, never executed by recovery.
func (h *handler) buildGeminiRecordTurns(carried []message, limit int) []geminiTurn {
	if len(carried) == 0 {
		return nil
	}
	texts := make([]string, 0, len(carried))
	for _, m := range carried {
		texts = append(texts, formatForBrowser(m)+"\n")
	}
	overhead := len(reseedRecordIntro) + len(reseedRecordEnd) + len(geminiAckAsk)
	pack := limit
	if pack > 0 {
		pack = limit - overhead
		if pack < 1 {
			pack = 1
		}
	}
	groups, _ := chunkGeminiTexts(texts, pack)
	var turns []geminiTurn
	for _, g := range groups {
		var b strings.Builder
		b.WriteString(reseedRecordIntro)
		for _, seg := range g {
			b.WriteString(seg + "\n")
		}
		b.WriteString(reseedRecordEnd + geminiAckAsk)
		turns = append(turns, geminiTurn{prompt: b.String(), replay: true})
	}
	return turns
}

type completionChoice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}
type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   map[string]int     `json:"usage"`
}

func (h *handler) buildCompletion(req *chatRequest, answer string, images []gemini.ResponseImage, calls []*bridgecore.BridgeCall, toolIDs []string, created int64, id string) *completion {
	msg := message{Role: "assistant"}
	finish := "stop"
	if len(calls) > 0 {
		for i, c := range calls {
			tcID := fmt.Sprintf("call_rebuilt_%d", i)
			rawArgs := c.RawArguments
			if i < len(toolIDs) {
				tcID = toolIDs[i]
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCall{
				ID: tcID, Type: "function",
				Function: callFunction{Name: c.Name, Arguments: rawArgs},
			})
		}
		finish = "tool_calls"
	} else {
		msg.Content = assistantContent(answer, images)
	}
	return &completion{ID: id, Object: "chat.completion", Created: created, Model: req.Model,
		Choices: []completionChoice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
}

func (h *handler) writeChatResponse(w http.ResponseWriter, req *chatRequest, resp *completion, fid string) {
	log.Printf("[gemini-flow] id=%s stage=response stream=%v answer_bytes=%d tool_calls=%d", fid, req.Stream, len(streamTextOf(resp.Choices[0].Message.Content)), len(resp.Choices[0].Message.ToolCalls))
	if req.Stream {
		// Buffered SSE: emit the validated completion as deltas, then DONE.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		choice := resp.Choices[0]
		meta := map[string]any{
			"id": resp.ID, "object": "chat.completion.chunk", "created": resp.Created, "model": resp.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
		}
		writeSSE(w, meta)
		if len(choice.Message.ToolCalls) > 0 {
			for i, tc := range choice.Message.ToolCalls {
				delta := map[string]any{
					"id": resp.ID, "object": "chat.completion.chunk", "created": resp.Created, "model": resp.Model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{
						"tool_calls": []map[string]any{{"index": i, "id": tc.ID, "type": "function",
							"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments}}},
					}, "finish_reason": nil}},
				}
				writeSSE(w, delta)
			}
		} else if texts := streamTextOf(choice.Message.Content); len(texts) > 0 {
			for _, s := range texts {
				delta := map[string]any{
					"id": resp.ID, "object": "chat.completion.chunk", "created": resp.Created, "model": resp.Model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": s}, "finish_reason": nil}},
				}
				writeSSE(w, delta)
			}
		}
		done := map[string]any{
			"id": resp.ID, "object": "chat.completion.chunk", "created": resp.Created, "model": resp.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": choice.FinishReason}},
		}
		writeSSE(w, done)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeSSE(w http.ResponseWriter, v any) {
	raw, _ := json.Marshal(v)
	_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func shortSID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
