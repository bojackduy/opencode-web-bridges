package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	Model      string    `json:"model"`
	Messages   []message `json:"messages"`
	Tools      []toolDef `json:"tools,omitempty"`
	ToolChoice any       `json:"tool_choice,omitempty"`
	Stream     bool      `json:"stream,omitempty"`
}

// session mirrors gemini's session with full recovery fields.
type session struct {
	Pending         bool      `json:"pending,omitempty"`
	PendingPrompt   string    `json:"pending_prompt,omitempty"`
	PendingMessages []message `json:"pending_messages,omitempty"`
	PendingTools    []toolDef `json:"pending_tools,omitempty"`
	PendingChoice   any       `json:"pending_choice,omitempty"`
	PendingReqHash  string    `json:"pending_req_hash,omitempty"`

	Messages       []message `json:"messages"`
	LastAnswer     string    `json:"last_answer,omitempty"`
	LastReqHash    string    `json:"last_req_hash,omitempty"`
	LastRespID     string    `json:"last_resp_id,omitempty"`
	LastCreated    int64     `json:"last_created,omitempty"`
	LastToolIDs    []string  `json:"last_tool_ids,omitempty"`
	URL            string    `json:"url,omitempty"`
	ToolsFP        string    `json:"tools_fp,omitempty"`
	ReseedFrom     string    `json:"reseed_from,omitempty"`
	ReseedEvidence string    `json:"reseed_evidence,omitempty"`
}

type browserClient interface {
	Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error)
	CurrentURL(ctx context.Context) string
	LoginState(ctx context.Context) (*meta.Status, error)
	ReadThreadTurn(ctx context.Context, url, prompt string) (meta.TurnResult, error)
	ReadLatestThreadTurn(ctx context.Context, url string) (string, meta.TurnResult, error)
}

type handler struct {
	client     *meta.Client
	model      string
	maxChars   int
	maxPaste   int
	statePath  string
	profileDir string

	mu       sync.Mutex
	sessions map[string]*session

	gate      chan struct{}
	activeSID string
	hasActive bool

	adoptWait time.Duration
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
	if h.statePath == "" {
		return nil
	}
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

func shortSID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func reqHash(req *chatRequest) string {
	raw, _ := json.Marshal(req)
	sum := sha256Sum(string(raw))
	return sum
}

func toolsFingerprint(tools []toolDef) string {
	raw, _ := json.Marshal(tools)
	return sha256Sum(string(raw))
}

func textOf(m message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
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

// browserClient assertion: *meta.Client satisfies browserClient.
var _ browserClient = (*meta.Client)(nil)

func (h *handler) writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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
	schemas, err := toolSchemas(req.Tools)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	choice := effectiveToolChoice(req.ToolChoice, len(req.Tools))

	sid := h.sessionID(r)
	hash := reqHash(&req)

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
	log.Printf("[openai] session=%s request in=%d saved=%d thread=%s pending=%v", shortSID(sid), len(req.Messages), len(sess.Messages), threadLabel(sess.URL), sess.Pending)

	// Exact last-request retry cache.
	if sess.LastReqHash == hash && sess.LastRespID != "" {
		if sess.LastAnswer != "" {
			calls, verr := metaExtractCalls(sess.LastAnswer, schemas, choice)
			if verr == nil {
				resp := h.buildCompletion(&req, sess.LastAnswer, nil, calls, sess.LastToolIDs, sess.LastCreated, sess.LastRespID)
				h.mu.Unlock()
				h.writeMetaChatResponse(w, &req, resp, "")
				return
			}
		}
	}

	// Pending-turn fencing.
	dirtyUncertain := false
	var reseedPlan *rewritePlan
	if sess.Pending {
		unbound := len(sess.PendingMessages) == 0 && sess.PendingPrompt == ""
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
			h.mu.Unlock()
			h.writeError(w, http.StatusUnprocessableEntity, "pending_turn", msg+"; "+h.sentDiagnosis(sid, sess))
			return
		}
		// Dirty adopt-first (bounded, read-only).
		h.mu.Unlock()
		adopted := h.tryAdoptInterrupted(sid, sess)
		h.mu.Lock()
		if adopted {
			log.Printf("[openai] session=%s dirty-adopted interrupted turn; continuing without reseed", shortSID(sid))
		} else {
			reseedPlan = h.planDirtyReseed(sid, sess, req.Messages)
			dirtyUncertain = true
			next := *sess
			next.URL, next.Messages = "", nil
			next.LastReqHash, next.LastRespID = "", ""
			clearPendingBinding(&next)
			sess = &next
			h.sessions[sid] = sess
			log.Printf("[openai] continuation reason=uncertain_branch incoming_messages=%d planned=%v", len(req.Messages), reseedPlan != nil)
		}
	}

	// Delta vs fresh.
	var seedMsgs []message
	var recordMsgs []message
	var teach []string
	teachBytes := 0
	// setTeach arms the tool teaching for this turn: instruction first,
	// then one blob per tool (chunking never splits mid-tool).
	setTeach := func() {
		if instr, parts := systemPromptParts(req.Tools, choice); instr != "" {
			teach = append([]string{instr}, parts...)
			for _, t := range teach {
				teachBytes += len(t) + 1
			}
		}
	}
	fresh := true
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
			log.Printf("[openai] session=%s system-prompt regenerated tools-changed; continuing delta under stored instructions", shortSID(sid))
			deltaKind = "system"
		}
	}
	if deltaKind != "" {
		if meta.ValidThreadURL(sess.URL) {
			fresh = false
			seedMsgs = req.Messages[len(sess.Messages):]
			// Teaching debt: the thread may have been minted by a tool-less
			// turn (titling carries no tools), in which case the model never
			// learned the wrapper protocol and every later turn runs
			// untaught. Any tool-set change — including none-to-some —
			// repays the full teaching once on this turn.
			if len(req.Tools) > 0 && toolsFingerprint(req.Tools) != sess.ToolsFP {
				setTeach()
				if len(teach) > 0 {
					log.Printf("[openai] session=%s teaching-debt repaid tools=%d", shortSID(sid), len(req.Tools))
				}
			}
		} else {
			log.Printf("[openai] session=%s delta-refused reason=missing-or-invalid-target incoming_messages=%d saved_messages=%d",
				shortSID(sid), len(req.Messages), len(sess.Messages))
		}
	}
	if fresh {
		setTeach()
		if reseedPlan == nil && len(sess.Messages) > 0 && sess.URL != "" {
			if mm := firstMismatch(req.Messages, sess.Messages); mm != nil {
				log.Printf("[openai] session=%s prefix_mismatch index=%d field=%s confoff=%d incoming_messages=%d saved_messages=%d",
					shortSID(sid), mm.Index, mm.Field, mm.ContentOff, len(req.Messages), len(sess.Messages))
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
		// Compaction-append.
		appending := false
		appendStart := 0
		if reseedPlan != nil && reseedPlan.classification == rewriteCompaction && !dirtyUncertain &&
			reseedPlan.mismatchField == "content" && len(sess.Messages) > 0 &&
			reseedPlan.mismatchIndex >= len(sess.Messages)*9/10 &&
			coherentTranscript(req.Messages) {
			appending = true
			appendStart = reseedPlan.mismatchIndex
			fresh = false
			seedMsgs = req.Messages[appendStart:]
			recordMsgs = nil
			log.Printf("[openai] session=%s compaction-append mismatch=%d prefix=%d/%d thread=%s",
				shortSID(sid), appendStart, appendStart, len(sess.Messages), sess.URL)
		}
		if !appending {
			seedMsgs = req.Messages
		}
		_ = appending
		_ = appendStart
	}

	// Chunk into bounded turns.
	limit := h.pasteLimit()
	recordTurns := buildRecordTurns(recordMsgs, limit)
	turns, terr := buildTurns(teach, seedMsgs, limit)
	if terr != nil {
		h.mu.Unlock()
		h.writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", terr.Error())
		return
	}
	allTurns := append(recordTurns, turns...)
	seedBase := 0
	if !fresh {
		seedBase = len(sess.Messages)
	}

	// Paste ceiling check before journaling. The fixed teaching head is not
	// changeable content: the budget binds what we choose to paste (history
	// slices, record), never the tool catalog we must teach on a fresh
	// thread. A huge catalog therefore chunks around the teaching instead
	// of refusing it; the insert verifier still fails closed on short
	// writes, so an oversized paste errors honestly instead of corrupting.
	sizes := make([]int, len(allTurns))
	replays := make([]bool, len(allTurns))
	for i, t := range allTurns {
		sizes[i], replays[i] = len(t.prompt), t.replay
		if len(t.prompt) > pasteCap(limit, teachBytes, t.replay) && limit > 0 {
			h.mu.Unlock()
			h.writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("turn %d/%d needs a %d-byte paste over the %d-byte single-paste limit. Nothing was sent and no tool was run.", i+1, len(allTurns), len(t.prompt), limit))
			return
		}
	}
	log.Printf("[openai] session=%s plan turns=%d sizes=%v replay=%v paste_limit=%d seed=%d", shortSID(sid), len(allTurns), sizes, replays, limit, len(seedMsgs))
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

	bound := sess.URL

	started := time.Now()
	var answer string
	var err2 error
	for i, t := range allTurns {
		turnFresh := fresh && i == 0
		if !turnFresh && !meta.ValidThreadURL(bound) {
			h.mu.Lock()
			clearPendingBinding(sess)
			h.saveLocked()
			h.mu.Unlock()
			h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: thread binding missing-or-invalid-target; nothing was sent and no new chat was started.")
			return
		}
		log.Printf("[openai] session=%s stage=browser-start turn=%d/%d fresh=%v prompt_bytes=%d", shortSID(sid), i+1, len(allTurns), turnFresh, len(t.prompt))
		h.mu.Lock()
		h.saveLocked()
		h.mu.Unlock()
		answer, err2 = h.client.Ask(r.Context(), t.prompt, turnFresh, 10*time.Minute)
		if err2 != nil {
			log.Printf("[openai] session=%s stage=browser-failed turn=%d/%d err=%v", shortSID(sid), i+1, len(allTurns), err2)
			h.mu.Lock()
			if meta.IsPreSubmit(err2) || errors.Is(err2, meta.ErrNoComposer) || errors.Is(err2, meta.ErrNotLoggedIn) {
				clearPendingBinding(sess)
			}
			h.saveLocked()
			h.mu.Unlock()
			if errors.Is(err2, meta.ErrNotLoggedIn) {
				h.writeError(w, http.StatusUnauthorized, "authentication_error", "not logged in to meta.ai")
				return
			}
			if errors.Is(err2, meta.ErrThreadBinding) {
				h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: thread binding failed ("+err2.Error()+"); nothing was sent and no new chat was started. The saved thread and transcript are retained; retry the same request.")
				return
			}
			h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: "+err2.Error())
			return
		}
		// Capture thread URL for binding next chunk.
		if cur := h.client.CurrentURL(r.Context()); meta.ValidThreadURL(cur) {
			if bound == "" || fresh {
				bound = cur
			}
		}
		if t.replay {
			// Meta answers imports conversationally instead of ACKing
			// (observed repeatedly: short prose referencing the import,
			// e.g. "Understood — treating the next messages as restored
			// transcript history"). A short wrapper-free answer proves the
			// import was read and is accepted as absorbed. Anything
			// executable, malformed, or long fails closed: a wrapper in an
			// import answer is an execution attempt, and a long answer
			// means the model went off-script.
			absorbed, prose := importAbsorbed(answer, schemas)
			if !absorbed {
				log.Printf("[meta] stage=replay turn=%d/%d result=rejected-answer session=%s", i+1, len(allTurns), shortSID(sid))
				h.mu.Lock()
				h.saveLocked()
				h.mu.Unlock()
				h.writeError(w, http.StatusBadGateway, "server_error", "BACKEND FAILURE: history import answer was not absorbable (executable, malformed, or overlong); session remains uncertain")
				return
			}
			if prose {
				log.Printf("[meta] stage=replay turn=%d/%d result=imported-with-prose bytes=%d session=%s", i+1, len(allTurns), len(strings.TrimSpace(answer)), shortSID(sid))
			} else {
				log.Printf("[meta] stage=replay turn=%d/%d result=acked session=%s", i+1, len(allTurns), shortSID(sid))
			}
			if t.synced > 0 {
				h.mu.Lock()
				sess.Messages = append([]message{}, req.Messages[:seedBase+t.synced]...)
				h.saveLocked()
				h.mu.Unlock()
			}
			continue
		}
	}

	// Protocol validation (strict, never weaken).
	calls, verr := meta.ExtractExecutableCalls(answer, schemas, choice)
	if verr != nil {
		h.mu.Lock()
		h.saveLocked()
		h.mu.Unlock()
		h.writeError(w, http.StatusUnprocessableEntity, "invalid_request_error",
			"model output failed tool-protocol validation: "+verr.Error())
		return
	}

	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-meta-%d", created)
	var toolIDs []string
	for i := range calls {
		toolIDs = append(toolIDs, fmt.Sprintf("call_meta_%d_%d", created, i))
	}

	h.mu.Lock()
	sess.Messages = append(canonical, message{Role: "assistant", Content: answer, ToolCalls: rebuildToolCalls(toolIDs, calls)})
	sess.ToolsFP = toolsFingerprint(req.Tools)
	sess.LastAnswer = answer
	sess.LastReqHash = hash
	sess.LastRespID = respID
	sess.LastCreated = created
	sess.LastToolIDs = toolIDs
	clearPendingBinding(sess)
	if meta.ValidThreadURL(bound) && (sess.URL == "" || fresh || reseedPlan != nil || bound == sess.URL) {
		sess.URL = bound
	}
	h.saveLocked()
	h.mu.Unlock()

	resp := h.buildCompletion(&req, answer, nil, calls, toolIDs, created, respID)
	h.writeMetaChatResponse(w, &req, resp, "")
	log.Printf("[openai] session=%s completed answer_bytes=%d duration=%s", shortSID(sid), len(answer), time.Since(started).Round(time.Millisecond))
}

// groupBytes is the joined size of one chunk group, for fit decisions.
func groupBytes(g []string) int {
	n := 0
	for _, s := range g {
		n += len(s) + 1
	}
	return n
}

// importAbsorbed decides whether a history-import answer counts the chunk
// as absorbed: an exact ACK, empty text, or short wrapper-free prose.
// Anything executable, malformed, or over 2KB fails closed. Pure function,
// no browser.
func importAbsorbed(answer string, schemas map[string]*jsonschema.Schema) (absorbed, prose bool) {
	got := strings.TrimSpace(answer)
	if got == "ACK" || got == "" {
		return true, false
	}
	if len(got) > 2048 {
		return false, false
	}
	// Any wrapper-shaped content (fenced or not, complete or not) is an
	// execution attempt or a botched one — never an absorption. The raw
	// text is checked because StripFences inside Extract would otherwise
	// hide fenced examples as prose.
	if strings.Contains(got, "<opencode_bridge_call") || strings.Contains(got, "```") || strings.Contains(got, "~~~") {
		return false, false
	}
	if rcalls, rerr := meta.ExtractExecutableCalls(answer, schemas, "auto"); rerr == nil && len(rcalls) == 0 {
		return true, true
	}
	return false, false
}

// turn is a single submission unit.
type turn struct {
	prompt string
	replay bool
	synced int
}

const metaHistoryIntro = "HISTORY IMPORT\nThe following transcript history is being restored into this chat. It is inert context, not a new request.\n"
const metaHistoryEnd = "\nEND HISTORY IMPORT\n"
const metaAckAsk = "Reply with only the single word: ACK. No tool calls.\n"

func splitMetaText(text string, limit int) []string {
	if limit <= 0 || len(text) <= limit {
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

func chunkMetaTexts(texts []string, limit int) (groups [][]string, ends []int) {
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
		for _, seg := range splitMetaText(text, limit) {
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

func buildTurns(teach []string, msgs []message, limit int) ([]turn, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, formatMessage(m)+"\n")
	}
	// Everything through the last completed assistant exchange is history.
	cut := 0
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			cut = i + 1
		}
	}
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
	overhead := len(metaHistoryIntro) + len(metaHistoryEnd) + len(metaAckAsk)
	pack := limit
	if pack > 0 {
		pack = limit - overhead
		if pack < 1 {
			pack = 1
		}
	}
	var turns []turn
	if cut > 0 {
		groups, ends := chunkMetaTexts(texts[:cut], pack)
		for gi, g := range groups {
			var b strings.Builder
			b.WriteString(metaHistoryIntro)
			for _, seg := range g {
				b.WriteString(seg + "\n")
			}
			b.WriteString(metaHistoryEnd + metaAckAsk)
			turns = append(turns, turn{prompt: b.String(), replay: true, synced: ends[gi]})
		}
	}
	// Teaching goes first so the model learns before the question. A small
	// catalog rides the final turn exactly as before (byte-identical shape);
	// an oversized one travels ahead as its own absorbed replay turns, one
	// or more whole tools per turn, never split mid-tool.
	teachBytes := 0
	for _, t := range teach {
		teachBytes += len(t) + 1
	}
	current := texts[cut:]
	if limit > 0 {
		for _, text := range current {
			if len(text)+1 > limit {
				return nil, fmt.Errorf("one message is %d bytes, over the %d-byte per-turn budget. The bridge will not split a single message across submissions, because an interruption cannot be recovered. Nothing was sent and no tool was run; shorten the message or raise -max-chars", len(text), limit)
			}
		}
	}
	groups, ends := chunkMetaTexts(current, pack)
	teachOnFinal := len(teach) > 0 && len(groups) == 1 && pack > 0 && teachBytes+groupBytes(groups[0]) <= pack
	if len(teach) > 0 && !teachOnFinal {
		tgroups, _ := chunkMetaTexts(teach, pack)
		for _, g := range tgroups {
			var b strings.Builder
			for _, seg := range g {
				b.WriteString(seg + "\n")
			}
			b.WriteString(metaAckAsk)
			turns = append(turns, turn{prompt: b.String(), replay: true})
		}
	}
	var sysHead string
	if teachOnFinal {
		sysHead = strings.Join(teach, "\n") + "\n\n"
	}
	for i, g := range groups {
		last := i == len(groups)-1
		var b strings.Builder
		if last {
			b.WriteString(sysHead)
		} else {
			b.WriteString(metaHistoryIntro)
		}
		for _, seg := range g {
			b.WriteString(seg + "\n")
		}
		if last {
			turns = append(turns, turn{prompt: b.String()})
		} else {
			b.WriteString(metaHistoryEnd + metaAckAsk)
			turns = append(turns, turn{prompt: b.String(), replay: true, synced: cut + ends[i]})
		}
	}
	return turns, nil
}

func buildRecordTurns(msgs []message, limit int) []turn {
	if len(msgs) == 0 {
		return nil
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, formatMessage(m)+"\n")
	}
	overhead := len(reseedRecordIntro) + len(reseedRecordEnd) + len(metaAckAsk)
	pack := limit
	if pack > 0 {
		pack = limit - overhead
		if pack < 1 {
			pack = 1
		}
	}
	groups, _ := chunkMetaTexts(texts, pack)
	var turns []turn
	for _, g := range groups {
		var b strings.Builder
		b.WriteString(reseedRecordIntro)
		for _, seg := range g {
			b.WriteString(seg + "\n")
		}
		b.WriteString(reseedRecordEnd + metaAckAsk)
		turns = append(turns, turn{prompt: b.String(), replay: true})
	}
	return turns
}

func formatMessage(m message) string {
	t := textOf(m)
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		raw, _ := json.Marshal(m.ToolCalls)
		return fmt.Sprintf("[%s] %s (calls: %s)", m.Role, t, string(raw))
	}
	if m.Role == "tool" {
		return fmt.Sprintf("[tool:%s] %s", m.ToolCallID, t)
	}
	return fmt.Sprintf("[%s] %s", m.Role, t)
}

func systemPrompt(tools []toolDef, choice string) string {
	instr, parts := systemPromptParts(tools, choice)
	if instr == "" {
		return ""
	}
	return instr + "\n" + strings.Join(parts, "\n")
}

// systemPromptParts splits the teaching into standalone texts: the
// instruction first, then one JSON blob per tool. Chunking never splits
// mid-tool — a cut JSON half is unusable context, while whole-tool pieces
// stay independently absorbable.
func systemPromptParts(tools []toolDef, choice string) (string, []string) {
	if len(tools) == 0 {
		return "", nil
	}
	instr := "You are a tool-using assistant. Available functions (JSON schemas):\n" +
		"\ntool_choice: " + choice + "\n" +
		"To call a function, reply with ONLY one or more <opencode_bridge_call>{\"name\":\"...\",\"arguments\":{...}}</opencode_bridge_call> wrappers and no other text. " +
		"Otherwise reply in plain text."
	var parts []string
	for _, t := range tools {
		raw, _ := json.Marshal(t)
		parts = append(parts, string(raw))
	}
	return instr, parts
}

func (h *handler) pasteLimit() int {
	if h.maxPaste > 0 {
		return h.maxPaste
	}
	if h.maxChars > 0 {
		return h.maxChars
	}
	return 200000
}

// pasteCap is the effective ceiling for one turn: replay/import turns obey
// the raw budget, while the answer-seeking turn additionally carries the
// fixed teaching head (tool catalog), which is not changeable content and
// must never count against the budget.
func pasteCap(limit, headBytes int, replay bool) int {
	if !replay {
		return limit + headBytes
	}
	return limit
}

func (h *handler) tryAdoptInterrupted(sid string, sess *session) bool {
	if h.client == nil {
		return false
	}
	if sess == nil || sess.URL == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tr, err := h.client.ReadThreadTurn(ctx, sess.URL, sess.PendingPrompt)
	if err != nil {
		return false
	}
	if strings.TrimSpace(tr.Answer) == "" {
		return false
	}
	// Adopt: record answer, clear pending.
	h.mu.Lock()
	sess.LastAnswer = tr.Answer
	clearPendingBinding(sess)
	h.saveLocked()
	h.mu.Unlock()
	log.Printf("[openai] session=%s adopted interrupted answer bytes=%d", shortSID(sid), len(tr.Answer))
	return true
}

func (h *handler) sentDiagnosis(sid string, sess *session) string {
	if sess == nil {
		return "no session"
	}
	return fmt.Sprintf("pending=%v url=%v messages=%d", sess.Pending, sess.URL != "", len(sess.Messages))
}

func threadLabel(u string) string {
	if u == "" {
		return "none"
	}
	return "stored"
}

func (h *handler) abandonSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "abandon requires POST")
		return
	}
	// Operator-only state mutation: reject browser-originated requests.
	if r.Header.Get("Origin") != "" {
		h.writeError(w, http.StatusForbidden, "invalid_request_error", "browser-originated abandonment is forbidden")
		return
	}
	id := h.sessionID(r)
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "session header required")
		return
	}
	// Never run next to a browser turn.
	select {
	case <-h.gate:
		defer func() { h.gate <- struct{}{} }()
	case <-r.Context().Done():
		h.writeError(w, http.StatusRequestTimeout, "timeout", "request canceled while queued")
		return
	default:
		h.writeError(w, http.StatusServiceUnavailable, "server_error", "browser turn busy; retry when the adapter is idle")
		return
	}
	h.mu.Lock()
	s, ok := h.sessions[id]
	outcome := "unknown"
	cleared := false
	if ok && s != nil {
		if s.Pending {
			clearPendingBinding(s)
			cleared = true
			outcome = "cleared"
		} else {
			outcome = "not_pending"
		}
	}
	if cleared {
		if err := h.saveLocked(); err != nil {
			h.mu.Unlock()
			h.writeError(w, http.StatusInternalServerError, "server_error", "session abandonment could not be persisted")
			return
		}
	}
	h.mu.Unlock()
	log.Printf("[openai] abandon session=%s outcome=%s", shortSID(id), outcome)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session": id,
		"outcome": outcome,
		"cleared": cleared,
	})
}

func (h *handler) recoverSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "recover requires POST")
		return
	}
	id := h.sessionID(r)
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "session header required")
		return
	}
	h.mu.Lock()
	sess, ok := h.sessions[id]
	pending := ok && sess != nil && sess.Pending
	h.mu.Unlock()
	if !pending {
		h.writeError(w, http.StatusConflict, "invalid_request_error", "no uncertain pending turn for this session")
		return
	}
	// Honest recovery: attempt bounded read-only adoption of the interrupted
	// turn. Only report recovered when an answer was actually adopted.
	adopted := h.tryAdoptInterrupted(id, sess)
	if !adopted {
		h.writeError(w, http.StatusConflict, "invalid_request_error", "recovery could not adopt a finished answer; pending turn remains uncertain")
		return
	}
	h.mu.Lock()
	bytes := 0
	if s := h.sessions[id]; s != nil {
		bytes = len(s.LastAnswer)
	}
	h.mu.Unlock()
	log.Printf("[openai] session=%s recover adopted answer_bytes=%d", shortSID(id), bytes)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"recovered":      true,
		"session":        id,
		"response_bytes": bytes,
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

// metaExtractCalls delegates to meta.ExtractExecutableCalls.
func metaExtractCalls(answer string, schemas map[string]*jsonschema.Schema, choice string) ([]*meta.BridgeCall, error) {
	return meta.ExtractExecutableCalls(answer, schemas, choice)
}

// completion wire types.
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

func (h *handler) buildCompletion(req *chatRequest, answer string, images []meta.ResponseImage, calls []*meta.BridgeCall, toolIDs []string, created int64, id string) *completion {
	msg := message{Role: "assistant"}
	finish := "stop"
	if len(calls) > 0 {
		for i, c := range calls {
			rawArgs, _ := json.Marshal(c.Arguments)
			tcID := fmt.Sprintf("call_meta_%d", i)
			if i < len(toolIDs) && toolIDs[i] != "" {
				tcID = toolIDs[i]
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCall{
				ID: tcID, Type: "function",
				Function: callFunction{Name: c.Name, Arguments: string(rawArgs)},
			})
		}
		finish = "tool_calls"
	} else {
		msg.Content = answer
		if len(images) > 0 {
			// keep simple: text only for meta in this batch
			msg.Content = answer
		}
	}
	if req != nil && req.Model != "" {
		// keep model from handler
	}
	return &completion{ID: id, Object: "chat.completion", Created: created, Model: h.model,
		Choices: []completionChoice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
}

func metaWireMessage(m message) map[string]any {
	out := map[string]any{"role": m.Role}
	if m.Content != nil {
		out["content"] = m.Content
	}
	if len(m.ToolCalls) > 0 {
		out["tool_calls"] = m.ToolCalls
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if m.Name != "" {
		out["name"] = m.Name
	}
	return out
}

func (h *handler) writeMetaChatResponse(w http.ResponseWriter, req *chatRequest, resp *completion, fid string) {
	if req != nil && req.Stream {
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
		} else {
			texts := []string{}
			if s, ok := choice.Message.Content.(string); ok && s != "" {
				texts = append(texts, s)
			}
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

func rebuildToolCalls(ids []string, calls []*meta.BridgeCall) []toolCall {
	out := make([]toolCall, 0, len(calls))
	for i, c := range calls {
		id := ""
		if i < len(ids) {
			id = ids[i]
		}
		args, _ := json.Marshal(c.Arguments)
		out = append(out, toolCall{
			ID:   id,
			Type: "function",
			Function: callFunction{
				Name:      c.Name,
				Arguments: string(args),
			},
		})
	}
	return out
}
