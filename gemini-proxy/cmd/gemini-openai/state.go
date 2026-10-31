package main

import (
	"encoding/json"
	"net/http"
)

// Read-only recovery state inspector.
//
// Unlike the ChatGPT adapter, Gemini has no read-only adoption endpoint:
// an uncertain turn is resolved by sending a DISTINCT request (the adapter
// reseeds a fresh thread and abandons the old one, never resending it) or
// by explicit abandonment. Identical retries of the pending request are
// fenced with 422 and never touch the browser. This endpoint reports that
// truth with enums, booleans, counts and sizes only: no prompt text, tool
// arguments, call ids or message bodies.

const (
	geminiStateClean          = "clean"
	geminiStatePending        = "pending"
	geminiStatePendingUnbound = "pending_unbound"
	geminiStateUnknown        = "unknown_session"
)

func geminiStateSummary(class string) string {
	switch class {
	case geminiStateClean:
		return "This chat is up to date; the next message will be answered normally."
	case geminiStatePending:
		return "A previous submission is uncertain; it was not resubmitted. Send a distinct request to continue on a fresh thread, or clear the pending turn explicitly."
	case geminiStatePendingUnbound:
		return "A previous submission is uncertain and its record cannot be acted on; clear it explicitly before continuing."
	default:
		return "No stored chat exists for this session yet."
	}
}

func (h *handler) sessionState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "state requires GET")
		return
	}
	id := sessionID(r)
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "session header required")
		return
	}
	h.mu.Lock()
	s := h.sessions[id]
	var snap session
	has := s != nil
	if has {
		snap = *s
	}
	h.mu.Unlock()
	if !has {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session":              id,
			"state":                geminiStateUnknown,
			"completed_messages":   0,
			"pending_prompt_bytes": 0,
			"thread_url_present":   false,
			"summary":              geminiStateSummary(geminiStateUnknown),
			"actions":              []string{},
		})
		return
	}
	bound := snap.PendingPrompt != "" || len(snap.PendingMessages) > 0
	class, actions := geminiStateClean, []string{"continue"}
	if snap.Pending {
		if bound {
			class, actions = geminiStatePending, []string{"reseed", "abandon"}
		} else {
			class, actions = geminiStatePendingUnbound, []string{"abandon"}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session":              id,
		"state":                class,
		"completed_messages":   len(snap.Messages),
		"pending_prompt_bytes": len(snap.PendingPrompt),
		"thread_url_present":   snap.URL != "",
		"summary":              geminiStateSummary(class),
		"actions":              actions,
	})
}
