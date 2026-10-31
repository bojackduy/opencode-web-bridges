package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// Explicit abandonment of a pending-uncertainty binding.
//
// This is the operator escape hatch for a turn that NO resolver can act on: one
// whose recorded binding is incomplete, or whose thread can no longer be read.
// Such a session refuses its own requests forever, and the only honest moves are
// to report that state (GET /v1/sessions/state) and to let the operator clear it
// BY NAME.
//
// Deliberately narrow: POST only, never a wildcard, never automatic, never on a
// timer, and never concurrent with a live turn. It clears the pending binding and
// nothing else, so the completed transcript, thread, cache identity and executed
// results all survive. Abandonment is a deliberate boundary, NOT proof the old
// turn succeeded — and it never resubmits anything.

// maxAbandonSessions bounds one operator call; the list is always explicit.
const maxAbandonSessions = 64

type abandonRequest struct {
	Sessions []string `json:"sessions"`
	Reason   string   `json:"reason"`
}

// abandonOutcome reports one session's result. Outcome is one of
// "abandoned" | "not_pending" | "unknown"; the accounting fields describe what
// the retained transcript already contains, so an operator can see whether any
// call was delivered or left unaccounted for before clearing uncertainty.
type abandonOutcome struct {
	Session          string `json:"session"`
	Outcome          string `json:"outcome"`
	StateBefore      string `json:"state_before"`
	DeliveredCalls   int    `json:"delivered_calls"`
	RecordedResults  int    `json:"recorded_results"`
	OutstandingCalls int    `json:"outstanding_calls"`
	AbandonedBytes   int    `json:"abandoned_prompt_bytes"`
}

// clearUncertaintyBinding drops the pending-turn binding and nothing else.
// Every automatic resolver clears the same set, so "pending" has exactly one
// definition in this adapter. Retained on purpose: Messages, URL, Fingerprint,
// RejectedFingerprint, RejectReason, Result, ToolsFP, RepairFingerprint (the
// durable one-repair budget, not the binding), Project and the reseed /
// auto-resync evidence.
func clearUncertaintyBinding(s *session) {
	s.Dirty = false
	s.Phase = ""
	s.PendingRequest = nil
	s.PendingPrompt = ""
	s.PendingBubbleID = ""
	s.RepairPendingPrompt = ""
	s.ResyncMessages = nil
	s.ResyncFrom = ""
}

// unresolvableID reports why one entry is not an explicit session id, so the
// whole request is refused with one actionable message instead of silently
// applying part of it.
func unresolvableID(raw string) string {
	id := strings.TrimSpace(raw)
	switch {
	case id == "":
		return "empty session id; every session must be named explicitly"
	case strings.Contains(id, "*"):
		return fmt.Sprintf("wildcard %q; every session must be named explicitly", id)
	case strings.EqualFold(id, "all"), strings.EqualFold(id, "any"):
		return fmt.Sprintf("%q is not a session id; every session must be named explicitly", id)
	}
	return ""
}

// abandonAccounting counts what the retained transcript already accounts for:
// tool calls delivered to the external client, tool results recorded back, and
// calls with no recorded result.
func abandonAccounting(s *session) (delivered, recorded, outstanding int) {
	seen := map[string]bool{}
	var order []string
	for _, m := range s.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID != "" && !seen[c.ID] {
				seen[c.ID] = true
				order = append(order, c.ID)
			}
		}
	}
	for _, m := range s.Messages {
		if m.Role == "tool" && m.ToolCallID != "" && contentText(m) != "" && seen[m.ToolCallID] {
			delete(seen, m.ToolCallID)
			recorded++
		}
	}
	for _, id := range order {
		if seen[id] {
			outstanding++
		}
	}
	return len(order), recorded, outstanding
}

func shortSessionID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}

// trySessionBatch takes the adapter's state lock for a batch journal mutation,
// or reports that it is unavailable. It never queues: a clear that lands after a
// live turn commits would silently undo the abandonment, and a half-applied
// clear cannot be undone.
func (h *handler) trySessionBatch(ids []string) (release func(), ok bool) {
	if h.parallel == nil {
		select {
		case h.gate <- struct{}{}:
			return func() { <-h.gate }, true
		default:
			return nil, false
		}
	}
	// Parallel mode: hold every named session's turn lock (non-blocking, in a
	// stable order) and then the store lock, so no commit can interleave.
	p := h.parallel
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	releases := make([]func(), 0, len(sorted))
	for _, id := range sorted {
		rel, got := p.tryLockSession(id)
		if !got {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, false
		}
		releases = append(releases, rel)
	}
	p.mu.Lock()
	return func() {
		p.mu.Unlock()
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, true
}

func (h *handler) abandonSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		apiError(w, http.StatusMethodNotAllowed, errors.New("abandon requires POST"))
		return
	}
	// An operator state mutation is not a browser API. A cross-site form or
	// fetch POST to loopback carries Origin; a command-line caller does not.
	if r.Header.Get("Origin") != "" {
		apiError(w, http.StatusForbidden, errors.New("browser-originated abandonment is forbidden"))
		return
	}
	var req abandonRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, errors.New("invalid JSON: "+err.Error()))
		return
	}
	// Validate the whole list before touching any session: a wildcard or empty
	// request is refused whole, never partially applied.
	ids := make([]string, 0, len(req.Sessions))
	seen := map[string]bool{}
	for _, raw := range req.Sessions {
		if why := unresolvableID(raw); why != "" {
			apiError(w, http.StatusBadRequest, errors.New("sessions: "+why))
			return
		}
		id := strings.TrimSpace(raw)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		apiError(w, http.StatusBadRequest, errors.New("sessions must name at least one session explicitly"))
		return
	}
	if len(ids) > maxAbandonSessions {
		apiError(w, http.StatusBadRequest, errors.New(fmt.Sprintf("at most %d sessions per request, got %d", maxAbandonSessions, len(ids))))
		return
	}
	release, ok := h.trySessionBatch(ids)
	if !ok {
		apiError(w, http.StatusServiceUnavailable, errors.New("the adapter is busy with a live turn or another operator call; retry when it is idle"))
		return
	}
	defer release()

	cleared := 0
	results := make([]abandonOutcome, 0, len(ids))
	originals := make(map[string]session, len(ids))
	for _, id := range ids {
		s := h.sessions[id]
		if s == nil {
			results = append(results, abandonOutcome{Session: id, Outcome: "unknown"})
			continue
		}
		delivered, recorded, outstanding := abandonAccounting(s)
		out := abandonOutcome{Session: id, StateBefore: stateClass(s), DeliveredCalls: delivered, RecordedResults: recorded, OutstandingCalls: outstanding}
		if !s.Dirty {
			// A known-rejected output (Phase=="invalid") is not pending
			// uncertainty: it already has a safe continuation, so it is left
			// exactly as it is.
			out.Outcome = "not_pending"
			results = append(results, out)
			continue
		}
		originals[id] = *s
		out.Outcome = "abandoned"
		out.AbandonedBytes = len(s.PendingPrompt)
		clearUncertaintyBinding(s)
		cleared++
		results = append(results, out)
	}
	if cleared > 0 {
		// Durable before the response: the journal is the restart record. A
		// clear that did not reach disk is never reported as one.
		if err := h.save(); err != nil {
			for id, previous := range originals {
				*h.sessions[id] = previous
			}
			apiError(w, http.StatusInternalServerError, errors.New("session abandonment could not be persisted; nothing was changed"))
			return
		}
	}

	// Audit line: counts and short ids only. The cleared prompt keeps its hash
	// lineage in the sent-log; no prompt text, tool arguments, call ids or media
	// are ever logged (the reason is recorded as a length).
	short := make([]string, 0, len(ids))
	for _, id := range ids {
		short = append(short, shortSessionID(id))
	}
	log.Printf("[openai] abandon requested=%d cleared=%d short_ids=%s reason_bytes=%d", len(ids), cleared, strings.Join(short, ","), len(req.Reason))

	writeJSON(w, http.StatusOK, map[string]any{
		"requested": len(ids), "abandoned": cleared, "results": results,
	})
}
