package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Explicit operator override for a pending record that NO code path can
// resolve — e.g. one written before the recovery stack existed, holding
// pending=true with an empty pending prompt and no bound request. Such a
// record can never be adopted (recoverPending) or reseeded (planDirtyReseed),
// so the only honest moves are to refuse that identity's requests (the
// per-session guard in chatCompletions) and to let the operator clear it BY
// NAME.
//
// Deliberately narrow: POST only, never a wildcard, never automatic, never on
// a timer, and never concurrent with a browser turn (it takes the same
// whole-turn gate every submission takes). It clears the pending binding and
// nothing else: the transcript, thread URL, duplicate-cache identity and the
// last result/tools are retained, so the session keeps working and nothing is
// ever resubmitted.

// maxAbandonSessions bounds one operator call; the list is always explicit.
const maxAbandonSessions = 64

// abandonRequest is the operator body: {"sessions":["id",...],"reason":"..."}.
type abandonRequest struct {
	Sessions []string `json:"sessions"`
	Reason   string   `json:"reason"`
}

// abandonResult reports one session's outcome:
// "abandoned" | "not_pending" | "unknown".
type abandonResult struct {
	Session string `json:"session"`
	Outcome string `json:"outcome"`
}

// clearPendingBinding drops the pending binding and nothing else. Callers hold
// h.mu. The same six fields are cleared by every automatic path that resolves
// a pending turn (normal completion, proved pre-submit failure, adoption), so
// "pending" has exactly one definition in this adapter.
func clearPendingBinding(s *session) {
	s.Pending = false
	s.PendingPrompt = ""
	s.PendingMessages = nil
	s.PendingTools = nil
	s.PendingChoice = nil
	s.PendingReqHash = ""
}

// unresolvableList reports why an explicit list is not one, so the operator
// gets one actionable message instead of a silently partial clear.
func unresolvableList(raw string) string {
	id := strings.TrimSpace(raw)
	switch {
	case id == "":
		return "empty session id; every session must be named explicitly"
	case strings.Contains(id, "*"):
		return "wildcard " + fmt.Sprintf("%q", id) + "; every session must be named explicitly"
	case strings.EqualFold(id, "all"), strings.EqualFold(id, "any"):
		return fmt.Sprintf("%q is not a session id; every session must be named explicitly", id)
	}
	return ""
}

func (h *handler) abandonSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "abandon requires POST")
		return
	}
	// This is an operator-only state mutation, not a browser API. Reject
	// browser-originated requests, including cross-site POSTs to loopback.
	if r.Header.Get("Origin") != "" {
		h.writeError(w, http.StatusForbidden, "invalid_request_error", "browser-originated abandonment is forbidden")
		return
	}
	var req abandonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	// Validate the whole list before touching any session: a wildcard or empty
	// request is refused whole, never partially applied.
	ids := make([]string, 0, len(req.Sessions))
	seen := map[string]bool{}
	for _, raw := range req.Sessions {
		if why := unresolvableList(raw); why != "" {
			h.writeError(w, http.StatusBadRequest, "invalid_request_error", "sessions: "+why)
			return
		}
		id := strings.TrimSpace(raw)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "sessions must not be empty")
		return
	}
	if len(ids) > maxAbandonSessions {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("at most %d sessions per request, got %d", maxAbandonSessions, len(ids)))
		return
	}
	// The operator path never runs next to a browser turn. Rather than queue
	// (an operator can retry; a half-applied clear cannot be undone), a busy
	// adapter refuses and changes nothing.
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

	cleared := 0
	results := make([]abandonResult, 0, len(ids))
	originals := make(map[string]session)
	h.mu.Lock()
	for _, id := range ids {
		switch s := h.sessions[id]; {
		case s == nil:
			results = append(results, abandonResult{Session: id, Outcome: "unknown"})
		case !s.Pending:
			results = append(results, abandonResult{Session: id, Outcome: "not_pending"})
		default:
			originals[id] = *s
			clearPendingBinding(s)
			cleared++
			results = append(results, abandonResult{Session: id, Outcome: "abandoned"})
		}
	}
	if cleared > 0 {
		// Durable before the response: the journal is the restart record.
		if err := h.saveLocked(); err != nil {
			for id, previous := range originals {
				*h.sessions[id] = previous
			}
			h.mu.Unlock()
			h.writeError(w, http.StatusInternalServerError, "server_error", "session abandonment could not be persisted")
			return
		}
	}
	h.mu.Unlock()

	// Audit line: counts and session ids only. The removed prompt keeps its
	// hash lineage in the sent-log; no prompt text, tool arguments, call ids
	// or media are ever logged (the reason is recorded as a length).
	short := make([]string, 0, len(ids))
	for _, id := range ids {
		short = append(short, shortSID(id))
	}
	log.Printf("[openai] abandon requested=%d cleared=%d short_ids=%s reason_bytes=%d",
		len(ids), cleared, strings.Join(short, ","), len(req.Reason))

	_ = json.NewEncoder(w).Encode(map[string]any{
		"requested": len(ids), "abandoned": cleared, "results": results,
	})
}
