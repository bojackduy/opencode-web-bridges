package main

import (
	"errors"
	"log"
	"net/http"
	"sort"
)

// User-facing terminal failures.
//
// A chat-visible error must let a person act. Three things belong there: what
// is unknown, the explicit statement that nothing was resent and no tools were
// re-run, and the ONE thing the user can SAY next. HTTP runbooks, endpoint
// names and state tags belong in structured detail and logs, never in the
// sentence a person reads in their chat.

const (
	// The canonical next thing a user can SAY. A plugin-side consumer maps
	// these to the actual recovery flow; the user never crafts a request.
	sayRecover      = `Next: say "run chatgpt-recover" to the assistant and I will show the options.`
	sayRecoverReset = `Next: say "run chatgpt-recover and reset this chat" to the assistant and I will clear that unknown turn so you can continue.`
	nextNewMessage  = "Next: send a new message to continue."
	nextFreshThread = "Next: send a new message to continue in a fresh thread."

	// noResend is the load-bearing promise: recovery is read-only, historical
	// tools are inert, and a refused turn is never silently retried.
	noResend = "Nothing from your previous message was resent and no tools were re-run."
)

// failureText composes one actionable chat-visible sentence.
func failureText(unknown, next string) string {
	return unknown + " " + noResend + " " + next
}

// rejectionUnknown is the plain-English stem for a known-rejected output. The
// empty-final and protocol-refusal classes stay distinct: one produced nothing,
// the other produced output that could not be used.
func rejectionUnknown(reason string) string {
	if reason == rejectEmptyFinal {
		return "The model finished without producing an answer, and no tool calls were delivered."
	}
	return "The model replied in a form that could not be used, so its answer was refused and no tool calls were delivered."
}

// apiErrorDetail keeps the adapter's existing error shape (message/type/code)
// and adds machine-actionable detail BESIDE the sentence, never inside it.
func apiErrorDetail(w http.ResponseWriter, status int, err error, details map[string]any) {
	kind := "proxy_error"
	if status == http.StatusUnprocessableEntity {
		kind = "invalid_request_error"
	}
	// Same terminal line as apiError: refusals must be traceable without
	// reproducing them. Detail keys only (reason/state codes), never values.
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	log.Printf("[openai] refused status=%d err=%v details=%v", status, err, keys)
	body := map[string]any{"message": err.Error(), "type": kind, "code": status}
	if len(details) > 0 {
		body["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// recoveryDetails is the structured half of a recovery refusal: the state tag,
// what is available, and the operator runbook. It never enters the message.
func (h *handler) recoveryDetails(id string, s *session) map[string]any {
	class := stateClass(s)
	canRecover, canResync := s.adoptable(), s.resyncable()
	detail := map[string]any{
		"session":                id,
		"state":                  class,
		"can_recover":            canRecover,
		"can_resync":             canResync,
		"thread_url_present":     s != nil && s.URL != "",
		"actions":                stateActions(class, s),
		"outstanding_tool_calls": 0,
		"runbook":                "read-only state: GET /v1/sessions/state with the same session header; read-only adoption: POST /v1/sessions/recover with the exact original request; explicit continuation boundary: POST /v1/sessions/resync with the reviewed completed transcript and recorded tool results, then send a NEW ordinary user message; explicit clear: POST /v1/sessions/abandon with this session named",
	}
	if s != nil {
		detail["outstanding_tool_calls"] = outstandingCallCount(s.Messages)
		// Content-free sent-log lineage: hashes, counts and thread URLs only.
		detail["sent_log"] = h.sentDiagnosis(id, s)
	}
	return detail
}

// uncertainFailure refuses an unresolved pending turn. The status stays 422
// (the installed host treats it as terminal instead of retrying a possibly
// submitted turn) and the classification stays as strict as before; only the
// wording becomes something a person can act on.
func (h *handler) uncertainFailure(w http.ResponseWriter, id string, s *session) {
	canRecover, canResync := s.adoptable(), s.resyncable()
	say := sayRecoverReset
	unknown := "A previous reply could not be confirmed and cannot be read back."
	if canRecover || canResync {
		say = sayRecover
		unknown = "A previous reply could not be confirmed, but it can still be read back without sending anything again."
	}
	log.Printf("[openai] session=%s uncertain refusal state=%s can_recover=%v can_resync=%v; runbook in error.details", id, stateClass(s), canRecover, canResync)
	apiErrorDetail(w, http.StatusUnprocessableEntity, errors.New(failureText(unknown, say)), h.recoveryDetails(id, s))
}
