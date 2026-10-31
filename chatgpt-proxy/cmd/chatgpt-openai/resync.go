package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// Explicit resync authorizes a NEW ordinary user boundary, not a retry of an
// old task whose output was never verified. Check before any loose re-arming
// for compaction: a changed prefix or system-update alone is not new input.
func freshResyncUser(s *session, req completionsRequest) bool {
	if len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || last.ToolCallID != "" || len(last.ToolCalls) != 0 {
		return false
	}
	updates := regexp.MustCompile(`(?s)<system-update>.*?</system-update>`)
	text := strings.TrimSpace(updates.ReplaceAllString(contentText(last), ""))
	if text == "" {
		return false
	}
	for _, old := range s.ResyncMessages {
		if old.Role == "user" && strings.TrimSpace(updates.ReplaceAllString(contentText(old), "")) == text {
			return false
		}
	}
	return true
}

// An explicit resync keeps uncertainty until a NEW user request arrives.
// All completed exchanges are imported as inert history, never as new calls.
func (h *handler) resyncSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, errors.New("POST required"))
		return
	}
	id, _, err := completionSessionID(r)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	var input struct {
		Messages []message `json:"messages"`
	}
	if id == "" {
		apiError(w, 400, errors.New("session header required"))
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&input); err != nil {
		apiError(w, 400, err)
		return
	}
	if !h.acquire(r.Context()) {
		return
	}
	defer h.release()
	unlock, err := h.lockTurn(r.Context())
	if err != nil {
		log.Printf("[openai] turn lock: %v", err)
		apiError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer unlock()
	s := h.sessions[id]
	// resyncable() is the single predicate behind this endpoint's acceptance —
	// the same one /v1/sessions/state reports as can_resync.
	if !s.resyncable() {
		apiError(w, 409, errors.New("resync requires an uncertain session with a recorded transcript and thread; nothing to reconcile"))
		return
	}
	if !prefix(input.Messages, s.Messages) {
		apiError(w, 409, errors.New("resync must extend the recorded uncertain session transcript"))
		return
	}
	last := s.Messages[len(s.Messages)-1]
	delta := input.Messages[len(s.Messages):]
	if last.Role == "assistant" && len(last.ToolCalls) > 0 {
		if len(delta) < len(last.ToolCalls) {
			apiError(w, 409, errors.New("supply exactly the recorded completed results for outstanding calls"))
			return
		}
		outstanding := map[string]bool{}
		for _, call := range last.ToolCalls {
			outstanding[call.ID] = true
		}
		for _, m := range delta[:len(last.ToolCalls)] {
			if m.Role != "tool" || !outstanding[m.ToolCallID] || contentText(m) == "" {
				apiError(w, 409, errors.New("completed tool result IDs/content must account for every outstanding call exactly once"))
				return
			}
			delete(outstanding, m.ToolCallID)
		}
	}
	{
		if len(input.Messages) < len(s.Messages) {
			apiError(w, 409, errors.New("authoritative transcript must include recorded prefix"))
			return
		}
		pendingCalls := map[string]bool{}
		seenCalls := map[string]bool{}
		for _, m := range input.Messages {
			if m.Role == "assistant" {
				for _, call := range m.ToolCalls {
					if call.ID == "" || seenCalls[call.ID] {
						apiError(w, 409, errors.New("authoritative transcript tool call IDs must be nonempty and unique"))
						return
					}
					seenCalls[call.ID] = true
					pendingCalls[call.ID] = true
				}
			} else if m.Role == "tool" {
				if !pendingCalls[m.ToolCallID] || contentText(m) == "" {
					apiError(w, 409, errors.New("tool results in authoritative transcript must match prior assistant calls"))
					return
				}
				delete(pendingCalls, m.ToolCallID)
			}
		}
		if len(pendingCalls) > 0 {
			apiError(w, 409, errors.New("authoritative transcript contains uncompleted tool calls"))
			return
		}
	}
	armed := *s
	armed.ResyncMessages = append([]message{}, input.Messages...)
	armed.ResyncFrom = s.URL
	h.sessions[id] = &armed
	if err := h.save(); err != nil {
		h.sessions[id] = s
		apiError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"resync_armed": true, "session": id, "history_messages": len(input.Messages), "dirty": true, "next": "send a NEW user message in this same session; recorded exchanges will be imported inertly into a new Project thread, with no historical tools rerun"})
}

// rearmResyncFromLive replaces a stale resync snapshot with the live request
// transcript (minus trailing new user messages) and returns true. It runs
// strictly pre-submission: Dirty, URL, and thread state are untouched, and
// every armed byte comes from the client's own authoritative transcript, so
// nothing can be duplicated or executed. The newest user question stays out of
// the snapshot and is answered fresh in the new thread.
//
// Anchoring is structural, not genealogical: the recorded synced prefix is
// preferred when it matches, but reverts, compactions, and prunes rewrite
// history by design, so a system-led transcript arriving under the session's
// own key is sufficient. Imports are inert ACK text either way, and Dirty is
// kept, so a wrong guess degrades to a confusing (not unsafe) import.
func (h *handler) rearmResyncFromLive(id string, s *session, req completionsRequest) bool {
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		return false
	}
	end := len(req.Messages)
	for end > 0 && req.Messages[end-1].Role == "user" {
		end--
	}
	if end == 0 {
		return false
	}
	candidate := append([]message{}, req.Messages[:end]...)
	// No-op guard: never replace the snapshot with an equivalent one (the
	// retry would 409 identically forever).
	if len(candidate) == len(s.ResyncMessages) && prefix(candidate, s.ResyncMessages) {
		return false
	}
	anchored := len(s.Messages) > 0 && prefix(req.Messages, s.Messages) && len(req.Messages) > len(s.Messages)
	if !anchored {
		log.Printf("[openai] session=%s resync_rearm_loose: recorded prefix unmatched (revert/compaction/prune); anchoring on session key", id)
	}
	old := s.ResyncMessages
	s.ResyncMessages = candidate
	s.ResyncFrom = s.URL
	if err := h.save(); err != nil {
		s.ResyncMessages = old
		return false
	}
	return true
}

func (h *handler) buildResyncTurns(req completionsRequest, history []message, head string) ([]turn, error) {
	ack := "Reply with only the single word: ACK. No tool calls.\n"
	pieces := make([]piece, 0, len(history))
	for i, m := range history {
		pieces = append(pieces, piece{text: renderMsg(m), end: i + 1})
	}
	limit := 0
	if capBytes := h.pasteLimit(); capBytes > 0 {
		limit = max(1, capBytes-len(historyIntro)-len(historyEnd)-len(ack))
	}
	var turns []turn
	for _, group := range packPieces(pieces, limit) {
		text := ""
		for _, p := range group {
			text += p.text + "\n"
		}
		turns = append(turns, turn{prompt: historyIntro + text + historyEnd + ack, replay: true, synced: group[len(group)-1].end})
	}
	current, err := h.buildTurns(req, req.Messages[len(history):], head)
	if err != nil {
		return nil, err
	}
	for i := range current {
		if current[i].replay {
			current[i].synced += len(history)
		}
	}
	return append(turns, current...), nil
}
