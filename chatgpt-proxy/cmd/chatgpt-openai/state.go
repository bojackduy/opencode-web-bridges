package main

import (
	"chatgpt-proxy/internal/chatgpt"
	"context"
	"errors"
	"net/http"
)

// Read-only recovery state inspector.
//
// The adapter already had read-only adoption (/v1/sessions/recover) and
// explicit resync (/v1/sessions/resync) but no way to ask "what is the state of
// this chat, and what can I actually do about it" without reading a runbook.
// This endpoint answers that question with enums, booleans, counts and sizes.
//
// Every class below is DERIVED from the same predicates the resolvers use —
// adoptable() and resyncable() — so the inspector cannot drift from the
// behaviour it describes. Nothing new is persisted.

const (
	stateClean               = "clean"
	statePendingVerifiable   = "pending_verifiable"
	statePendingUnverifiable = "pending_unverifiable"
	stateRejectedOutput      = "rejected_output"
	stateEmptyFinal          = "empty_final"
	stateUnknownSession      = "unknown_session"
)

const (
	actionContinue      = "continue"
	actionRecover       = "recover"
	actionResync        = "resync"
	actionAbandon       = "abandon"
	actionRecheck       = "recheck"
	actionCorrectFormat = "correct_format"
)

// rejectionIdentifiable reports whether a durable rejected_output journal still
// names the exact turn it refuses: the same request fingerprint and the same
// submitted prompt. Reading that turn needs no stored hash, because
// chatgpt.ReadThreadTurn refuses any user bubble that is not this exact prompt,
// so it can only return the answer that follows it — never "whatever is
// latest". This is the same proof adoption already trusts.
func (s *session) rejectionIdentifiable() bool {
	if s == nil || s.Phase != "invalid" || s.RejectReason != rejectToolProtocol ||
		s.PendingRequest == nil || s.PendingPrompt == "" || s.URL == "" {
		return false
	}
	return s.RejectedFingerprint == requestFingerprint(*s.PendingRequest)
}

// rejectionBound additionally proves the journal already holds the digest of
// the exact reply it refuses. When it does, a recheck must see that same reply
// or change nothing.
func (s *session) rejectionBound() bool {
	return s.rejectionIdentifiable() && s.RejectedAnswerHash != "" &&
		s.RejectedPromptHash == chatgpt.PromptHash(s.PendingPrompt)
}

// recheckable is the single predicate behind /v1/sessions/recheck, its 409
// guard and the state inspector's can_recheck. A journal predating reply
// digests (rejected before this was recorded) still qualifies: the first
// recheck binds the identity it reads from the exact-prompt turn.
func (s *session) recheckable() bool { return s.rejectionIdentifiable() }

// correctable gates the ONE human-authorized correction submission. A spent
// correction (CorrectionFingerprint already recorded for this request) is never
// offered again. A journal without a retained sample reads the exact reply
// first, so the quoted defect is still the refused one.
func (s *session) correctable() bool {
	return s.rejectionIdentifiable() &&
		s.CorrectionFingerprint != requestFingerprint(*s.PendingRequest)
}

// adoptable reports whether a read-only adoption of the EXACT retained
// original request is possible. It is the single predicate behind both
// /v1/sessions/state's can_recover and the guard in recoverSession; a thread
// target may still be supplied by the caller, which thread_url_present reports
// separately.
func (s *session) adoptable() bool {
	return s != nil && !s.incompleteJournal() && s.Dirty &&
		s.Phase != "invalid" && s.Phase != "replay" &&
		s.PendingRequest != nil && len(s.PendingRequest.Messages) > 0 && s.PendingPrompt != ""
}

// resyncable reports whether the recorded completed transcript satisfies the
// explicit-resync preconditions. Shared with resyncSession.
func (s *session) resyncable() bool {
	return s != nil && s.Dirty && s.URL != "" && len(s.Messages) > 0
}

// stateClass names one session's recovery state, reusing the existing
// rejection classification so empty-final and protocol-refusal stay distinct.
func stateClass(s *session) string {
	switch {
	case s == nil:
		return stateUnknownSession
	case s.Phase == "invalid":
		if s.RejectReason == rejectEmptyFinal {
			return stateEmptyFinal
		}
		return stateRejectedOutput
	case !s.Dirty:
		return stateClean
	case s.adoptable() || s.resyncable() || s.Phase == "replay":
		// "replay" is verifiable without adoption: only inert history imports
		// were in flight, so the next ordinary completion request resumes them.
		return statePendingVerifiable
	default:
		return statePendingUnverifiable
	}
}

// stateSummary is one plain-English sentence a non-engineer understands.
func stateSummary(class string) string {
	switch class {
	case stateClean:
		return "This chat is up to date; the next message will be answered normally."
	case statePendingVerifiable:
		return "A previous reply could not be confirmed, but it can still be read back without sending anything again."
	case statePendingUnverifiable:
		return "A previous reply could not be confirmed and cannot be read back; this chat has to be explicitly cleared before it can be used again."
	case stateRejectedOutput:
		return "The model's last reply could not be used as a tool call, and nothing was run; you can check that exact reply again, ask ChatGPT to repeat it once in the right format, or send a new message."
	case stateEmptyFinal:
		return "The model's last reply was empty and nothing was run; a new message will continue."
	default:
		return "No stored chat exists for this session yet."
	}
}

// stateActions lists only the actions actually available for that session.
func stateActions(class string, s *session) []string {
	canRecover, canResync := s.adoptable(), s.resyncable()
	out := []string{}
	switch class {
	case stateClean, stateEmptyFinal:
		out = append(out, actionContinue)
	case stateRejectedOutput:
		// Read-only check first: it can only ever cache the reply that was
		// already produced. The correction is a NEW submission and is offered
		// only while its one-shot budget is unspent.
		if s != nil && s.recheckable() {
			out = append(out, actionRecheck)
		}
		if s != nil && s.correctable() {
			out = append(out, actionCorrectFormat)
		}
		out = append(out, actionContinue)
	case statePendingVerifiable:
		if canRecover {
			out = append(out, actionRecover)
		}
		if canResync {
			out = append(out, actionResync)
		}
		if s != nil && s.Phase == "replay" {
			// Only inert imports were in flight: the next ordinary completion
			// request resumes them, so "continue" is a real action here.
			out = append(out, actionContinue)
		}
		out = append(out, actionAbandon)
	case statePendingUnverifiable:
		out = append(out, actionAbandon)
	}
	return out
}

// outstandingCalls counts tool calls in the recorded transcript that have no
// recorded result, i.e. work the external client has not accounted for.
func outstandingCallCount(msgs []message) int {
	seen := map[string]bool{}
	var order []string
	for _, m := range msgs {
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
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" && contentText(m) != "" {
			delete(seen, m.ToolCallID)
		}
	}
	n := 0
	for _, id := range order {
		if seen[id] {
			n++
		}
	}
	return n
}

// describeSession renders the content-free state body. Only enums, booleans,
// counts and sizes: no prompt text, tool arguments, call ids or message bodies.
func describeSession(id string, s *session) map[string]any {
	class := stateClass(s)
	canRecover, canResync := s.adoptable(), s.resyncable()
	canRecheck, canCorrect := s.recheckable(), s.correctable()
	completed, pendingBytes, outstanding, threadURL := 0, 0, 0, false
	if s != nil {
		completed = len(s.Messages)
		pendingBytes = len(s.PendingPrompt)
		outstanding = outstandingCallCount(s.Messages)
		threadURL = s.URL != ""
	}
	return map[string]any{
		"session":                id,
		"state":                  class,
		"can_recover":            canRecover,
		"can_resync":             canResync,
		"can_recheck":            canRecheck,
		"can_correct_format":     canCorrect,
		"thread_url_present":     threadURL,
		"completed_messages":     completed,
		"pending_prompt_bytes":   pendingBytes,
		"outstanding_tool_calls": outstanding,
		"summary":                stateSummary(class),
		"actions":                stateActions(class, s),
	}
}

// sessionSnapshot returns a private COPY of a journal record taken under the
// adapter's own state lock. Cloning keeps a read-only inspector from ever
// aliasing state a live turn can mutate.
func (h *handler) sessionSnapshot(ctx context.Context, id string) (*session, bool) {
	if h.parallel != nil {
		h.parallel.mu.Lock()
		defer h.parallel.mu.Unlock()
		s, err := cloneSession(h.sessions[id])
		return s, err == nil
	}
	if !h.acquire(ctx) {
		return nil, false
	}
	defer h.release()
	s, err := cloneSession(h.sessions[id])
	return s, err == nil
}

func (h *handler) sessionState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		apiError(w, http.StatusMethodNotAllowed, errors.New("GET required"))
		return
	}
	id := sessionID(r)
	if id == "" {
		apiError(w, http.StatusBadRequest, errors.New("session header required"))
		return
	}
	s, ok := h.sessionSnapshot(r.Context(), id)
	if !ok {
		apiError(w, http.StatusServiceUnavailable, errors.New("journal is busy with a live turn; retry when the adapter is idle"))
		return
	}
	writeJSON(w, http.StatusOK, describeSession(id, s))
}
