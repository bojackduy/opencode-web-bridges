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

	"github.com/bojackduy/bridgecore"
	"github.com/bojackduy/gemini-proxy/internal/gemini"
)

// Interrupt adoption + hash-lineage auto-resync for the Gemini bridge,
// ported from the ChatGPT bridge and adapted to the Gemini thread model.
//
// Differences from chatgpt-proxy (deliberate, all fail-closed):
//   - No background target: the adapter owns ONE tab and never navigates it
//     for recovery. The owned tab IS the thread; a tab that moved elsewhere
//     refuses recovery instead of following it.
//   - No bounded-transform class: Gemini adoption requires FULL normalized
//     equality (self-validating). The enumerated transform set is empty
//     until a Gemini renderer transform is observed.
//   - Stable completion IDs reuse the adapter's own call_gemini_<ts>_<i>
//     scheme, so transplanted turns are indistinguishable downstream.
//
// Recovery never submits, never executes tools, and never clears an
// ambiguous journal. Transplanted tool calls are RECORDED for the external
// client to execute.

// spansStructural reports whether span evidence describes structural desync
// (multi-KB interior regions or scattered spans with no re-convergence)
// rather than a small bounded difference (one small span, then
// re-convergence). Unparseable evidence fails closed: not structural, no
// auto-resync. Span grammar comes from gemini.DiffSpans:
// "aOff-aEnd/bOff-bEnd,..." or "aOff+rest/bOff+rest" when anchors never
// re-converge.
func spansStructural(spans string) bool {
	if spans == "" {
		return false
	}
	if strings.Contains(spans, "+rest") {
		return true
	}
	segs := strings.Split(spans, ",")
	if len(segs) > 1 {
		return true // scattered spans: compaction or a rebuilt request
	}
	var a0, a1, b0, b1 int
	if _, err := fmt.Sscanf(segs[0], "%d-%d/%d-%d", &a0, &a1, &b0, &b1); err != nil {
		return false
	}
	if a1 < a0 || b1 < b0 {
		return false
	}
	// Multi-KB interior divergence is not a bounded difference.
	return (a1-a0)+(b1-b0) >= 2048
}

// readPendingTurn waits for the owned thread's answer to complete and reads
// it against the pending prompt. It returns adopted=true with the thread
// result on full normalized equality. A typed structural mismatch is
// returned separately so the caller can attempt hash-lineage auto-resync;
// every other failure returns adopted=false with a nil mismatch and the
// caller keeps terminal uncertainty. wait>0 caps the polling budget;
// wait<=0 keeps the legacy rule (h.adoptWait override else the 90s default).
func (h *handler) readPendingTurn(r *http.Request, id string, s *session, wait time.Duration) (gemini.TurnResult, *gemini.UserBubbleMismatchError, bool) {
	if s.PendingPrompt == "" || s.URL == "" {
		return gemini.TurnResult{}, nil, false
	}
	if h.client == nil {
		return gemini.TurnResult{}, nil, false
	}
	// Errors that waiting cannot fix: the stored prompt genuinely does not
	// match, or the owned tab is no longer on the stored thread.
	deterministic := func(err error) bool {
		if err == nil {
			return false
		}
		msg := err.Error()
		return strings.Contains(msg, "does not exactly match") ||
			strings.Contains(msg, "not on the stored thread")
	}
	deadline := time.Now().Add(90 * time.Second)
	if wait > 0 {
		deadline = time.Now().Add(wait)
	} else if h.adoptWait > 0 {
		deadline = time.Now().Add(h.adoptWait)
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	var res gemini.TurnResult
	for {
		var err error
		res, err = h.client.ReadThreadTurn(ctx, s.URL, s.PendingPrompt)
		if err == nil {
			return res, nil, true
		}
		var mismatch *gemini.UserBubbleMismatchError
		if errors.As(err, &mismatch) {
			log.Printf("[openai] session=%s adoption impossible: %v", id, err)
			return gemini.TurnResult{}, mismatch, false
		}
		if deterministic(err) {
			log.Printf("[openai] session=%s adoption impossible: %v", id, err)
			return gemini.TurnResult{}, nil, false
		}
		if time.Now().After(deadline) {
			log.Printf("[openai] session=%s adoption timed out waiting for stored answer: %v", id, err)
			return gemini.TurnResult{}, nil, false
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return gemini.TurnResult{}, nil, false
		case <-timer.C:
		}
	}
}

// recordCompletedTurn validates a thread answer against the PENDING request's
// schemas and records it as the completed pending turn: transcript, stable
// IDs, and duplicate-cache identity, exactly like a normal completion.
// resyncEvidence ("" for plain adoption) is journaled; sentNote annotates
// the sent-log completion. On any failure it returns false with no state
// change. Recovery never executes anything itself.
func (h *handler) recordCompletedTurn(id string, s *session, answer string, images []gemini.ResponseImage, resyncEvidence, sentNote string) bool {
	if len(s.PendingMessages) == 0 {
		return false
	}
	schemas, err := toolSchemas(s.PendingTools)
	if err != nil {
		log.Printf("[openai] session=%s recovery refused: pending schemas invalid: %v", id, err)
		return false
	}
	choice := effectiveToolChoice(s.PendingChoice, len(s.PendingTools))
	calls, err := bridgecore.ExtractExecutableCalls(answer, schemas, choice)
	if err != nil {
		log.Printf("[openai] session=%s recovery refused: thread answer fails strict protocol validation: %v", id, err)
		return false
	}
	if strings.TrimSpace(answer) == "" && len(calls) == 0 {
		log.Printf("[openai] session=%s recovery refused: thread answer is empty", id)
		return false
	}
	created := time.Now().Unix()
	respID := fmt.Sprintf("chatcmpl-gemini-%d", created)
	var toolIDs []string
	for i := range calls {
		toolIDs = append(toolIDs, fmt.Sprintf("call_gemini_%d_%d", created, i))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	old := *s
	s.Messages = append(append([]message{}, s.PendingMessages...),
		message{Role: "assistant", Content: assistantContent(answer, images), ToolCalls: h.rebuildToolCalls(calls, toolIDs)})
	s.LastAnswer = answer
	s.LastImages = images
	s.LastReqHash = s.PendingReqHash
	s.LastRespID = respID
	s.LastCreated = created
	s.LastToolIDs = toolIDs
	clearPendingBinding(s)
	if resyncEvidence != "" {
		s.AutoResyncEvidence = resyncEvidence
	}
	h.appendSent(sentEntry{Event: "completed", Session: id, URL: s.URL, PromptHash: promptHash(old.PendingPrompt), Bytes: len(old.PendingPrompt), Chunk: 1, Chunks: 1, AnswerHash: gemini.AnswerHash(answer), Note: sentNote})
	h.activeSID, h.hasActive = id, true
	// saveLocked persists synchronously (atomic write + rename); the
	// journal and the sent-log file are the restart-durable record.
	h.saveLocked()
	return true
}

// rebuildToolCalls maps validated bridge calls onto wire tool calls with the
// stored stable IDs (same shape as the normal completion path).
func (h *handler) rebuildToolCalls(calls []*bridgecore.BridgeCall, toolIDs []string) []toolCall {
	var out []toolCall
	for i, c := range calls {
		tcID := fmt.Sprintf("call_rebuilt_%d", i)
		if i < len(toolIDs) {
			tcID = toolIDs[i]
		}
		out = append(out, toolCall{
			ID: tcID, Type: "function",
			Function: callFunction{Name: c.Name, Arguments: c.RawArguments},
		})
	}
	return out
}

// mustMarshalArgs renders validated call arguments for the wire. The calls
// were already schema-validated, so encoding cannot fail; "{}" is the
// closed fallback, never a bare empty string.
func mustMarshalArgs(args map[string]any) string {
	if args == nil {
		return "{}"
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// tryAdoptInterrupted attempts read-only recovery of an uncertain turn: the
// owned thread's answer must fully equal the pending submission's context.
// On success the completed turn is recorded exactly like a normal
// completion and the caller continues as duplicate-cache/delta. On any
// failure it returns false (with the structural mismatch, if any) and the
// caller keeps terminal uncertainty.
func (h *handler) tryAdoptInterrupted(r *http.Request, id string, s *session) (bool, *gemini.UserBubbleMismatchError) {
	return h.tryAdoptInterruptedWait(r, id, s, 0)
}

// tryAdoptInterruptedWait is tryAdoptInterrupted with an explicit polling
// budget (wait<=0 keeps the legacy h.adoptWait/90s rule), so the dirty path
// can attempt a fast adopt without mutating shared handler state.
func (h *handler) tryAdoptInterruptedWait(r *http.Request, id string, s *session, wait time.Duration) (bool, *gemini.UserBubbleMismatchError) {
	res, mismatch, adopted := h.readPendingTurn(r, id, s, wait)
	if mismatch != nil || !adopted {
		return false, mismatch
	}
	if !h.recordCompletedTurn(id, s, res.Answer, res.Images, "", "adopted interrupted turn; recorded, never executed by recovery") {
		return false, nil
	}
	log.Printf("[openai] session=%s adopted interrupted turn answer_bytes=%d", id, len(res.Answer))
	return true, nil
}

// tryAutoResync attempts the recognized clear-cut shape and returns true on
// a durable transplant (the caller then continues as delta / duplicate
// cache, exactly like adoption). All three gates must hold:
//
// (a) adoption failed structurally: a typed mismatch whose spans show
// multi-KB interior divergence, not a small bounded difference;
// (b) the thread holds a verifiably complete assistant answer following the
// submitted user turn (ReadLatestThreadTurn's finishedness proof);
// (c) the sent-log ties that thread submission to this session: the thread's
// latest user turn hashes (under adoption normalization) to this session's
// recorded submission for the same thread URL.
//
// Any gate failure returns false with no state change: the session stays
// Dirty for an explicit 409/manual resync.
func (h *handler) tryAutoResync(r *http.Request, id string, s *session, mismatch *gemini.UserBubbleMismatchError) bool {
	if mismatch == nil || s == nil {
		return false
	}
	if s.PendingPrompt == "" || s.URL == "" || len(s.PendingMessages) == 0 {
		return false
	}
	if !spansStructural(mismatch.Spans) {
		log.Printf("[openai] session=%s auto-resync skipped: bounded difference spans=%s", id, mismatch.Spans)
		return false
	}
	if h.client == nil {
		return false
	}
	latestUser, res, err := h.client.ReadLatestThreadTurn(r.Context(), s.URL)
	if err != nil {
		log.Printf("[openai] session=%s auto-resync skipped: no transplantable thread answer: %v", id, err)
		return false
	}
	entry := h.sentEntryForThread(id, s.URL)
	if entry == nil {
		log.Printf("[openai] session=%s auto-resync refused: sent-log shows no submission by this session to thread %s", id, s.URL)
		return false
	}
	if gemini.PromptHash(latestUser) != entry.PromptHash {
		log.Printf("[openai] session=%s auto-resync refused: thread latest turn hash %s != sent-log submission %s for thread %s", id, shortHash12(gemini.PromptHash(latestUser)), shortHash12(entry.PromptHash), s.URL)
		return false
	}
	pendingHash := gemini.PromptHash(s.PendingPrompt)
	evidence := fmt.Sprintf("auto-resync %s: transplanted %dB thread answer (hash %s) from thread %s; thread submission hash %s == sent-log %s entry (%s); pending hash %s differed by structural spans %s, proving pending-overwritten-by-retry desync; deemed safe: thread holds this session's verifiably complete answer, validated against the pending schemas and recorded (never executed by recovery)",
		time.Now().UTC().Format(time.RFC3339), len(res.Answer), shortHash12(gemini.AnswerHash(res.Answer)), s.URL, shortHash12(entry.PromptHash), entry.Event, entry.At, shortHash12(pendingHash), mismatch.Spans)
	if !h.recordCompletedTurn(id, s, res.Answer, res.Images, evidence, "auto-resync transplant; recorded, never executed by recovery") {
		return false
	}
	log.Printf("[openai] session=%s auto-resynced recognized desync shape answer_bytes=%d evidence=%q", id, len(res.Answer), evidence)
	return true
}

// recoverSession runs explicit read-only recovery for one pending session:
// adoption of the stored answer (exact, key or window identity), then
// hash-lineage auto-resync — the same recoverPending the dirty path runs.
// It never submits and never executes tools. POST only, one session named
// by the session headers (wildcards refused), never concurrent with a
// browser turn: like abandon it takes the whole-turn gate without queuing,
// and a busy adapter refuses. The 200 body matches the shared recovery
// report shape (cached_tool_calls, response_bytes).
func (h *handler) recoverSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "recover requires POST")
		return
	}
	// Operator-only read of the owned tab, not a browser API. Reject
	// browser-originated requests, including cross-site POSTs to loopback.
	if r.Header.Get("Origin") != "" {
		h.writeError(w, http.StatusForbidden, "invalid_request_error", "browser-originated recovery is forbidden")
		return
	}
	id := sessionID(r)
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "session header required")
		return
	}
	if why := unresolvableList(id); why != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", why)
		return
	}
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
	pending := h.sessions[id] != nil && h.sessions[id].Pending
	h.mu.Unlock()
	if !pending {
		h.mu.Lock()
		diag := h.sentDiagnosis(id, h.sessions[id])
		h.mu.Unlock()
		h.writeError(w, http.StatusConflict, "invalid_request_error", "no uncertain pending turn for this session; "+diag)
		return
	}
	if !h.recoverPending(r, id) {
		h.mu.Lock()
		diag := h.sentDiagnosis(id, h.sessions[id])
		h.mu.Unlock()
		h.writeError(w, http.StatusConflict, "invalid_request_error", "no completed answer to adopt yet; "+diag)
		return
	}
	h.mu.Lock()
	calls, bytes := 0, 0
	if s := h.sessions[id]; s != nil {
		calls, bytes = len(s.LastToolIDs), len(s.LastAnswer)
	}
	h.mu.Unlock()
	log.Printf("[openai] session=%s explicit recover adopted cached_calls=%d response_bytes=%d", shortSID(id), calls, bytes)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"recovered": true, "session": id,
		"cached_tool_calls": calls, "response_bytes": bytes,
		"next": "send the same message again, unchanged — the bridge returns the stored reply instead of asking Gemini",
	})
}

// recoverPending runs the full Dirty-path recovery for one session: exact
// adoption first, then hash-lineage auto-resync on structural mismatch.
// True means the journal now holds the completed turn and the caller should
// continue as duplicate-cache/delta. False means terminal uncertainty:
// keep 409 + diagnosis, Dirty untouched. It never submits and never
// executes tools.
func (h *handler) recoverPending(r *http.Request, sid string) bool {
	if h.client == nil {
		return false
	}
	h.mu.Lock()
	s := h.sessions[sid]
	h.mu.Unlock()
	if s == nil || !s.Pending || s.PendingPrompt == "" || s.URL == "" || len(s.PendingMessages) == 0 {
		return false
	}
	if ok, mismatch := h.tryAdoptInterrupted(r, sid, s); ok {
		return true
	} else if mismatch != nil && h.tryAutoResync(r, sid, s, mismatch) {
		return true
	}
	return false
}
