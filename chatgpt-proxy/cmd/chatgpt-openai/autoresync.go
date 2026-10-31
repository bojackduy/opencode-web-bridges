package main

import (
	"chatgpt-proxy/internal/chatgpt"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Hash-lineage auto-resync: the programmatic form of the manual ses_ee37
// playbook. When a retry overwrote the journal pending prompt (H3) after the
// thread had already completed the earlier submission (H1), exact adoption
// against H3 fails structurally — but the thread still holds this session's
// verifiably complete answer, and the sent-log proves the thread submission
// belongs to this session. Transplanting that real answer (with its tool
// calls recorded, never executed by recovery) is safe; anything less certain
// stays Dirty with a 409 plus the sent-log diagnosis line.

type autoResyncBackend interface {
	ReadLatestThreadTurn(context.Context, string) (string, chatgpt.TurnResult, error)
}

// spansStructural reports whether span evidence describes structural desync
// (multi-KB interior regions or scattered spans with no re-convergence)
// rather than a bounded renderer-transform class (one small span, then
// re-convergence). Unparseable evidence fails closed: not structural, no
// auto-resync. Span grammar comes from internal/chatgpt diffSpans:
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
	// Multi-KB interior divergence is not a bounded transform class.
	return (a1-a0)+(b1-b0) >= 2048
}

// tryAutoResync attempts the recognized clear-cut shape and returns true on
// a durable transplant (the caller then continues as delta / duplicate
// cache, exactly like adoption). All three gates must hold:
//
// (a) adoption failed structurally: a typed mismatch whose spans show
// multi-KB interior divergence, not a bounded transform;
// (b) the thread holds a verifiably complete assistant answer following the
// submitted user turn (ReadLatestThreadTurn's finishedness proof);
// (c) the sent-log ties that thread submission to this session: the thread's
// latest user turn hashes (under adoption normalization) to this session's
// recorded submission for the same thread URL.
//
// On success the real answer is transplanted onto the pending transcript
// with stable call IDs, journal evidence is appended (AutoResyncEvidence),
// Dirty is cleared, and the request continues. Tool calls found in the
// answer are RECORDED for the external client to execute — recovery never
// executes anything itself — and each transplanted call ID is marked
// executed-by-resync in the evidence. Any gate failure returns false with no
// state change: the session stays Dirty for an explicit 409/manual resync.
func (h *handler) tryAutoResync(r *http.Request, id string, s *session, mismatch *chatgpt.UserBubbleMismatchError) bool {
	if mismatch == nil || s == nil {
		return false
	}
	pending := s.PendingRequest
	if pending == nil || s.PendingPrompt == "" || s.URL == "" || s.Phase != "final" {
		return false
	}
	if !spansStructural(mismatch.Spans) {
		log.Printf("[openai] session=%s auto-resync skipped: bounded transform class spans=%s", id, mismatch.Spans)
		return false
	}
	backend, ok := h.client.(autoResyncBackend)
	if !ok {
		return false
	}
	latestUser, res, err := backend.ReadLatestThreadTurn(r.Context(), s.URL)
	if err != nil {
		log.Printf("[openai] session=%s auto-resync skipped: no transplantable thread answer: %v", id, err)
		return false
	}
	entry := h.sentEntryForThread(id, s.URL)
	if entry == nil {
		log.Printf("[openai] session=%s auto-resync refused: sent-log shows no submission by this session to thread %s", id, s.URL)
		return false
	}
	if chatgpt.PromptHash(latestUser) != entry.PromptHash {
		log.Printf("[openai] session=%s auto-resync refused: thread latest turn hash %s != sent-log submission %s for thread %s", id, shortHash12(chatgpt.PromptHash(latestUser)), shortHash12(entry.PromptHash), s.URL)
		return false
	}
	out, err := parseAnswer(res.Answer, *pending)
	if err != nil {
		log.Printf("[openai] session=%s auto-resync refused: thread answer fails strict protocol validation: %v", id, err)
		return false
	}
	if out.Text == "" && len(out.Calls) == 0 {
		log.Printf("[openai] session=%s auto-resync refused: thread answer is empty", id)
		return false
	}
	out.Reasoning = res.Reasoning
	out.Images = res.Images
	fp := requestFingerprint(*pending)
	// Stable IDs derive exactly as adoption derives them, so a transplanted
	// result is indistinguishable from an adopted one downstream.
	idSum := sha256.Sum256([]byte(id + ":" + fp))
	out.ID, out.Created = fmt.Sprintf("chatcmpl-%x", idSum), time.Now().Unix()
	for i := range out.Calls {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", id, fp, i)))
		out.Calls[i].ID = fmt.Sprintf("call_%x", sum[:12])
	}
	callIDs := make([]string, 0, len(out.Calls))
	for _, c := range out.Calls {
		callIDs = append(callIDs, c.ID+"(executed-by-resync)")
	}
	old := *s
	pendingHash := chatgpt.PromptHash(s.PendingPrompt)
	s.Messages = append(append([]message{}, pending.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	s.Fingerprint, s.Result, s.Dirty, s.Phase = fp, out, false, ""
	s.ToolsFP = toolsFingerprint(pending.Tools)
	s.PendingRequest, s.PendingPrompt, s.RepairPendingPrompt, s.PendingBubbleID = nil, "", "", ""
	s.AutoResyncEvidence = fmt.Sprintf("auto-resync %s: transplanted %dB thread answer (hash %s) with %d tool call(s) [%s] from thread %s; thread submission hash %s == sent-log %s entry (%s); pending hash %s differed by structural spans %s, proving pending-overwritten-by-retry desync; deemed safe: thread holds this session's verifiably complete answer and every call validated against the pending schemas and recorded (never executed by recovery)",
		time.Now().UTC().Format(time.RFC3339), len(res.Answer), shortHash12(chatgpt.PromptHash(res.Answer)), len(out.Calls), strings.Join(callIDs, ","), s.URL, shortHash12(entry.PromptHash), entry.Event, entry.At, shortHash12(pendingHash), mismatch.Spans)
	if err := h.save(); err != nil {
		*s = old // retain the original pending binding if the transplant was not durable
		return false
	}
	log.Printf("[openai] session=%s auto-resynced recognized desync shape tool_calls=%d answer_bytes=%d evidence=%q", id, len(out.Calls), len(res.Answer), s.AutoResyncEvidence)
	return true
}
