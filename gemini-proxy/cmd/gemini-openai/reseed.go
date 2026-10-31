package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// History-rewrite reseed for the Gemini bridge, ported from chatgpt-proxy.
//
// opencode legitimately rewrites history (revert, fork/regeneration,
// compact) which breaks the adapter's append-only prefix/delta assumption.
// The interrupt-uncertain case belongs to recoverPending (exact adoption,
// then hash-lineage auto-resync); this file covers the distinct REWRITE
// case (incoming history is internally coherent but does not prefix-match
// the journal).
//
// Order on the Dirty branch: adoption → auto-resync → dirty-reseed plan →
// 422 + diagnosis (genuinely ambiguous only). On the clean branch a
// coherent rewrite reseeds; incoherent input keeps the legacy full-history
// fresh replay (clean sessions hold no uncertainty to protect).
// The old thread is abandoned truthfully — no adoption comparison against
// it — and the turn continues on a fresh Gemini chat as a normal
// single-turn submission (bounded by maxChars like any fresh turn; there
// is no chunked replay to split, so wrappers cannot be cut).

// Rewrite classifications. "ambiguous" is not a rewrite: the incoming
// history is internally incoherent, so no reseed is attempted.
const (
	rewriteTruncation   = "rewrite-truncation"   // revert/truncation: shorter, prefix-equal
	rewriteDivergence   = "rewrite-divergence"   // fork/regeneration: same-length-but-diverged content
	rewriteCompaction   = "rewrite-compaction"   // compaction: summarized system prefix
	rewriteContinuation = "rewrite-continuation" // client moved past the uncertain turn: committed journal is a canonical prefix of incoming
	rewriteAmbiguous    = "ambiguous"            // incoherent: dangling/duplicate/unmatched tool results
)

// prefixMismatch contains structure only: which saved-journal index and
// field the incoming history first diverges on. Never text or IDs.
type prefixMismatch struct {
	Index int
	Field string
	// Content-free shape of the mismatched values: "str:<bytes>:<lines>",
	// "parts:<n>", "null" or "other". Lengths distinguish wrappers and
	// rewrites without revealing any text.
	IncomingShape string
	SavedShape    string
	// ContentOff is the first differing byte offset for string-vs-string
	// content (else -1). Offsets pinpoint append-vs-rewrite classes
	// (trailing newline, truncation marker, re-encode tail) without
	// revealing any text.
	ContentOff int
}

func contentShape(v any) string {
	switch c := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("str:%d:%d", len(c), len(strings.Split(c, "\n")))
	case []any:
		return fmt.Sprintf("parts:%d", len(c))
	default:
		return "other"
	}
}

// firstDiffOff returns the first differing byte offset of two strings, or
// min(len) when one is a prefix of the other. Structural evidence only.
func firstDiffOff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// shapeBytes parses the byte count out of a "str:<bytes>:<lines>" shape,
// or -1 when the value is not a plain string.
func shapeBytes(shape string) int {
	var n, l int
	if _, err := fmt.Sscanf(shape, "str:%d:%d", &n, &l); err != nil {
		return -1
	}
	return n
}

// rewritePlan is a validated fresh-chat reseed. carried holds committed
// executed tool exchanges absent from the incoming history, in journal
// order; notCarried names unknown-state items deliberately NOT carried
// (fail closed). oldURL is the abandoned thread.
type rewritePlan struct {
	classification string
	mismatchIndex  int
	mismatchField  string
	carried        []message
	carriedIDs     []string
	notCarried     []string
	oldURL         string
}

// firstMismatch returns the first structural divergence of the saved
// journal prefix against the incoming history, or nil when the journal is
// a prefix of incoming (coherent append-only: no rewrite).
func firstMismatch(incoming, saved []message) *prefixMismatch {
	for i := range saved {
		m := &prefixMismatch{Index: i, ContentOff: -1}
		if i >= len(incoming) {
			m.Field = "history.truncated"
			return m
		}
		a, b := incoming[i], saved[i]
		m.IncomingShape, m.SavedShape = contentShape(a.Content), contentShape(b.Content)
		switch {
		case a.Role != b.Role:
			m.Field = "role"
		case a.Name != b.Name:
			m.Field = "name"
		case a.ToolCallID != b.ToolCallID:
			m.Field = "tool_call_id"
		case textOf(a) != textOf(b):
			m.Field = "content"
			if ai, ok := a.Content.(string); ok {
				if bi, ok := b.Content.(string); ok {
					m.ContentOff = firstDiffOff(ai, bi)
				}
			}
		case len(a.ToolCalls) != len(b.ToolCalls):
			m.Field = "tool_calls.length"
		default:
			for j := range a.ToolCalls {
				other := b.ToolCalls[j]
				field := ""
				switch {
				case a.ToolCalls[j].ID != other.ID:
					field = "id"
				case a.ToolCalls[j].Type != other.Type:
					field = "type"
				case a.ToolCalls[j].Function.Name != other.Function.Name:
					field = "function.name"
				case !jsonEqual(a.ToolCalls[j].Function.Arguments, other.Function.Arguments):
					field = "function.arguments"
				}
				if field != "" {
					m.Field = fmt.Sprintf("tool_calls[%d].%s", j, field)
					break
				}
			}
		}
		if m.Field != "" {
			return m
		}
	}
	return nil
}

// echoBloatSkipped checks the saved journal against incoming with one
// narrow exception to strict equality: an assistant-role message may
// reappear with up to tol appended bytes. Live evidence shows the client
// re-echoing our just-committed answers with appended re-render notes
// (~637B, pure append at the full saved length); treating that as a
// rewrite reseeds a new chat every turn. Tolerance applies ONLY to
// assistant echoes of our own committed answers — roles, names, call IDs
// and tool calls stay exact, truncation fails closed, and user/system/tool
// divergences of any size stay significant (a user edit to old text must
// never be silently dropped). Returns total skipped bytes for the log.
func echoBloatSkipped(saved, incoming []message, tol int) (bool, int) {
	if len(incoming) < len(saved) {
		return false, 0
	}
	skipped := 0
	for i := range saved {
		if canonicalEqual(saved[i], incoming[i]) {
			continue
		}
		a, b := saved[i], incoming[i]
		if a.Role != "assistant" || b.Role != "assistant" {
			return false, 0
		}
		if a.Name != b.Name || a.ToolCallID != b.ToolCallID {
			return false, 0
		}
		if len(a.ToolCalls) != len(b.ToolCalls) {
			return false, 0
		}
		for j := range a.ToolCalls {
			other := b.ToolCalls[j]
			if a.ToolCalls[j].ID != other.ID || a.ToolCalls[j].Type != other.Type ||
				a.ToolCalls[j].Function.Name != other.Function.Name ||
				!jsonEqual(a.ToolCalls[j].Function.Arguments, other.Function.Arguments) {
				return false, 0
			}
		}
		sa, sb := textOf(a), textOf(b)
		if len(sb) < len(sa) || !strings.HasPrefix(sb, sa) || len(sb)-len(sa) > tol {
			return false, 0
		}
		skipped += len(sb) - len(sa)
	}
	return true, skipped
}

// systemOnlyDivergence reports whether saved and incoming differ only in
// system-role messages at the same positions: a regenerated system prompt
// (tool-set flip) over an untouched conversation. Non-system messages must
// be canonically identical — assistant echo bloat does NOT compose here;
// that shape keeps the echo path (or reseeds when both diverge). A shorter
// incoming (truncation) fails closed to the reseed path.
func systemOnlyDivergence(saved, incoming []message) bool {
	if len(incoming) < len(saved) {
		return false
	}
	for i := range saved {
		a, b := saved[i], incoming[i]
		if a.Role == "system" && b.Role == "system" {
			continue
		}
		if !canonicalEqual(a, b) {
			return false
		}
	}
	return true
}

// coherentTranscript reports whether msgs is internally coherent: valid
// tool-call/result pairing in order (every tool result matches a prior
// assistant call exactly once, results are nonempty, call IDs are nonempty
// and unique) with no dangling calls.
func coherentTranscript(msgs []message) bool {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if c.ID == "" || seen[c.ID] {
					return false
				}
				seen[c.ID] = true
				pending[c.ID] = true
			}
		} else if m.Role == "tool" {
			if m.ToolCallID == "" || !pending[m.ToolCallID] || textOf(m) == "" {
				return false
			}
			delete(pending, m.ToolCallID)
		}
	}
	return len(pending) == 0
}

// isCompactionShape reports whether the incoming history opens with a
// client compaction/summarization marker: a system message naming the
// compaction. Anything else falls through to divergence.
func isCompactionShape(incoming []message) bool {
	if len(incoming) == 0 || incoming[0].Role != "system" {
		return false
	}
	lower := strings.ToLower(textOf(incoming[0]))
	return strings.Contains(lower, "compact") || strings.Contains(lower, "summar")
}

// classifyHistoryRewrite distinguishes HISTORY-REWRITE from append-only
// continuation and from incoherence. It returns ("", nil) when the saved
// journal is a prefix of incoming (no rewrite, no behavior change),
// ("ambiguous", mm) when incoming is internally incoherent (never reseed),
// and otherwise one of the rewrite kinds with structural mismatch
// evidence. A nil saved journal is not a rewrite.
func classifyHistoryRewrite(incoming, saved []message) (string, *prefixMismatch) {
	mm := firstMismatch(incoming, saved)
	if mm == nil || len(saved) == 0 {
		return "", mm
	}
	if !coherentTranscript(incoming) {
		return rewriteAmbiguous, mm
	}
	if mm.Field == "history.truncated" {
		return rewriteTruncation, mm
	}
	if isCompactionShape(incoming) {
		return rewriteCompaction, mm
	}
	return rewriteDivergence, mm
}

// canonArgs normalizes tool arguments exactly the way canonicalEqual does,
// so a rewritten history that merely re-encodes arguments still matches
// the executed call instead of duplicating it.
func canonArgs(args string) string {
	var v any
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	if json.Valid([]byte(args)) && decoder.Decode(&v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return args
}

// callPresentIn reports whether the executed call survives in the incoming
// rewritten history: same stable ID, or same function with canonically
// equal arguments (a fork/regeneration that kept the call but minted IDs).
func callPresentIn(call toolCall, incoming []message) bool {
	for _, m := range incoming {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == call.ID {
				return true
			}
			if c.Function.Name == call.Function.Name && canonArgs(c.Function.Arguments) == canonArgs(call.Function.Arguments) {
				return true
			}
		}
	}
	return false
}

// carryExecuted collects executed tool exchanges (assistant calls with
// recorded results) that are ABSENT from the incoming rewritten history.
// Execution proof comes from the committed journal plus, for dirty
// sessions, the still-pending request: its contained tool results were
// recorded by the client from previously delivered answers, so they are
// known-executed even though the pending turn's own answer is unknown.
// Carried messages travel in transcript order (whole assistant messages
// plus their tool results, so the seed stays internally coherent).
// incomplete names committed pairs still missing results after consulting
// both sources: deliberately NOT carried. The pending turn's undelivered
// answer is NOT decided here: the plan builders list it as not-carried.
func carryExecuted(s *session, pending []message, incoming []message) (carried []message, carriedIDs []string, incomplete []string) {
	results := map[string]message{}
	for _, msgs := range [][]message{s.Messages, pending} {
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID != "" && textOf(m) != "" && results[m.ToolCallID].ToolCallID == "" {
				results[m.ToolCallID] = m
			}
		}
	}
	seen := map[string]bool{}
	for _, msgs := range [][]message{s.Messages, pending} {
		for _, m := range msgs {
			if m.Role != "assistant" || len(m.ToolCalls) == 0 {
				continue
			}
			complete := true
			for _, c := range m.ToolCalls {
				if _, ok := results[c.ID]; !ok {
					complete = false
					incomplete = append(incomplete, fmt.Sprintf("incomplete-committed:%s (no recorded result; not carried)", c.ID))
				}
			}
			if !complete {
				continue
			}
			needed := false
			for _, c := range m.ToolCalls {
				if seen[c.ID] {
					continue
				}
				if !callPresentIn(c, incoming) {
					needed = true
					carriedIDs = append(carriedIDs, fmt.Sprintf("%s(%s)", c.ID, c.Function.Name))
				}
			}
			if !needed {
				continue
			}
			carried = append(carried, m)
			for _, c := range m.ToolCalls {
				seen[c.ID] = true
				if r, ok := results[c.ID]; ok {
					carried = append(carried, r)
				}
			}
		}
	}
	return carried, carriedIDs, incomplete
}

// planCleanReseed validates a rewrite reseed for a non-dirty session: the
// incoming history is a coherent rewrite of committed state, so reseeding
// fresh with carried executed results is safe. It returns nil for
// append-only histories (no behavior change) and for incoherent input
// (legacy fresh replay applies; clean sessions hold no uncertainty).
func planCleanReseed(s *session, incoming []message) *rewritePlan {
	if s == nil || s.URL == "" || len(s.Messages) == 0 {
		return nil
	}
	kind, mm := classifyHistoryRewrite(incoming, s.Messages)
	if kind == "" || kind == rewriteAmbiguous {
		return nil
	}
	carried, carriedIDs, incomplete := carryExecuted(s, nil, incoming)
	return &rewritePlan{
		classification: kind,
		mismatchIndex:  mm.Index,
		mismatchField:  mm.Field,
		carried:        carried,
		carriedIDs:     carriedIDs,
		notCarried:     incomplete,
		oldURL:         s.URL,
	}
}

// planDirtyReseed records the abandonment of an uncertain remote chat for a
// pending session. Unknown-state items (the still-pending turn) are listed as
// NOT carried and the old chat is abandoned without delivery — safe because an
// undelivered answer caused no external execution. Context continuity is best
// effort, so this plan never gates the request: it returns nil when the shape
// cannot be classified (retry of the same uncertain request, or a missing
// journal) and the caller still proceeds from the client's own history.
func (h *handler) planDirtyReseed(id string, s *session, incoming []message) *rewritePlan {
	if s == nil || !s.Pending {
		return nil
	}
	var kind string
	var mm *prefixMismatch
	if len(s.Messages) == 0 {
		// No committed journal to rewrite: every incoming message is client
		// history, so the fresh chat is a continuation, not a rewrite.
		kind = rewriteContinuation
		mm = &prefixMismatch{Index: 0, Field: "history.none"}
	} else {
		kind, mm = classifyHistoryRewrite(incoming, s.Messages)
	}
	if kind == "" {
		// The committed journal is a canonical prefix of the incoming
		// history: the client moved past the uncertain turn (revert, fork or
		// model switch). Abandon the old chat without delivery and seed fresh
		// from the client's own timeline.
		if len(incoming) <= len(s.Messages) {
			return nil
		}
		log.Printf("[openai] session=%s rewrite-reseed continuation: committed journal is a prefix of the client history added=%d; uncertain turn abandoned without resubmission", id, len(incoming)-len(s.Messages))
		kind = rewriteContinuation
		mm = &prefixMismatch{Index: len(s.Messages), Field: "history.extension"}
	} else if kind == rewriteAmbiguous {
		if mm != nil {
			log.Printf("[openai] session=%s rewrite-reseed refused: not a coherent rewrite (append or ambiguous); keeping uncertainty", id)
		}
		return nil
	}
	carried, carriedIDs, incomplete := carryExecuted(s, s.PendingMessages, incoming)
	plan := &rewritePlan{
		classification: kind,
		mismatchIndex:  mm.Index,
		mismatchField:  mm.Field,
		carried:        carried,
		carriedIDs:     carriedIDs,
		notCarried:     incomplete,
		oldURL:         s.URL,
	}
	if s.PendingPrompt != "" {
		plan.notCarried = append(plan.notCarried, fmt.Sprintf("pending:%s (outcome unknown; old thread abandoned without delivery, never submitted as pending)", shortHash12(promptHash(s.PendingPrompt))))
	}
	return plan
}

// Prior-execution record delimiters. The carried exchanges travel as inert
// reviewed history ahead of the rewritten transcript: clearly delimited as
// a prior-execution record, never re-submitted as pending calls. Every
// carried message renders through formatForBrowser, which labels tool
// calls/results as inert history.
const reseedRecordIntro = "PRIOR EXECUTION RECORD\nThe following completed tool exchanges already executed in the external client before a history rewrite (revert/fork/compaction). They are inert history: do NOT re-request them, do NOT emit opencode_bridge_call wrappers for them. Use their results only as context.\n"
const reseedRecordEnd = "END PRIOR EXECUTION RECORD\n"

// evidence renders the reseed record shared by the journal and the
// sent-log: rewrite classification, old→new thread linkage, and the
// carried/not-carried item lists. Structure only: IDs and hashes, never
// text or argument values.
func (p *rewritePlan) evidence(newURL string, seedBytes int) string {
	carried := "[]"
	if len(p.carriedIDs) > 0 {
		carried = "[" + strings.Join(p.carriedIDs, ",") + "]"
	}
	notCarried := "[]"
	if len(p.notCarried) > 0 {
		notCarried = "[" + strings.Join(p.notCarried, ",") + "]"
	}
	return fmt.Sprintf("rewrite-reseed %s classification=%s mismatch=%d:%s old=%s new=%s carried=%s not-carried=%s seed=%dB record-first",
		time.Now().UTC().Format(time.RFC3339), p.classification, p.mismatchIndex, p.mismatchField, p.oldURL, newURL, carried, notCarried, seedBytes)
}

// appendEvidence renders the compaction-append record: same-thread
// continuation, mismatch position, and the carried/not-carried item lists.
// Carried exchanges were deliberately NOT re-imported (the stored thread
// already holds them); they are listed so the lineage stays auditable.
func (p *rewritePlan) appendEvidence(newURL string, seedBytes int) string {
	carried := "[]"
	if len(p.carriedIDs) > 0 {
		carried = "[" + strings.Join(p.carriedIDs, ",") + "]"
	}
	notCarried := "[]"
	if len(p.notCarried) > 0 {
		notCarried = "[" + strings.Join(p.notCarried, ",") + "]"
	}
	return fmt.Sprintf("compaction-append %s mismatch=%d:%s old=%s new=%s retained-on-thread=%s not-carried=%s seed=%dB same-thread",
		time.Now().UTC().Format(time.RFC3339), p.mismatchIndex, p.mismatchField, p.oldURL, newURL, carried, notCarried, seedBytes)
}

// note renders the sent-log form of the linkage: old→new thread plus
// classification and item lists.
func (p *rewritePlan) note(newURL string) string {
	carried := "[]"
	if len(p.carriedIDs) > 0 {
		carried = "[" + strings.Join(p.carriedIDs, ",") + "]"
	}
	notCarried := "[]"
	if len(p.notCarried) > 0 {
		notCarried = "[" + strings.Join(p.notCarried, ",") + "]"
	}
	return fmt.Sprintf("reseed old=%s new=%s classification=%s carried=%s not-carried=%s", p.oldURL, newURL, p.classification, carried, notCarried)
}
