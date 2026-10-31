package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// History-rewrite reseed for the Meta bridge, ported from gemini-proxy
// (cmd/gemini-openai/reseed.go). Meta AI's thread model mirrors Gemini's:
// server keeps the conversation; continuation sends only new messages when
// incoming history extends the stored prefix; divergent history starts a
// new chat and replays bounded turns.

const (
	rewriteTruncation   = "rewrite-truncation"
	rewriteDivergence   = "rewrite-divergence"
	rewriteCompaction   = "rewrite-compaction"
	rewriteContinuation = "rewrite-continuation"
	rewriteAmbiguous    = "ambiguous"
)

type prefixMismatch struct {
	Index         int
	Field         string
	IncomingShape string
	SavedShape    string
	ContentOff    int
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

func shapeBytes(shape string) int {
	var n, l int
	if _, err := fmt.Sscanf(shape, "str:%d:%d", &n, &l); err != nil {
		return -1
	}
	return n
}

type rewritePlan struct {
	classification string
	mismatchIndex  int
	mismatchField  string
	carried        []message
	carriedIDs     []string
	notCarried     []string
	oldURL         string
}

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

func isCompactionShape(incoming []message) bool {
	if len(incoming) == 0 || incoming[0].Role != "system" {
		return false
	}
	lower := strings.ToLower(textOf(incoming[0]))
	return strings.Contains(lower, "compact") || strings.Contains(lower, "summar")
}

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

func canonArgs(args string) string {
	var v any
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	if json.Valid([]byte(args)) {
		if decoder.Decode(&v) == nil {
			if b, err := json.Marshal(v); err == nil {
				return string(b)
			}
		}
	}
	return args
}

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

func (h *handler) planDirtyReseed(id string, s *session, incoming []message) *rewritePlan {
	if s == nil || !s.Pending {
		return nil
	}
	var kind string
	var mm *prefixMismatch
	if len(s.Messages) == 0 {
		kind = rewriteContinuation
		mm = &prefixMismatch{Index: 0, Field: "history.none"}
	} else {
		kind, mm = classifyHistoryRewrite(incoming, s.Messages)
	}
	if kind == "" {
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

func clearPendingBinding(s *session) {
	if s == nil {
		return
	}
	s.Pending = false
	s.PendingPrompt = ""
	s.PendingMessages = nil
	s.PendingTools = nil
	s.PendingChoice = nil
	s.PendingReqHash = ""
}

func shortHash12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func promptHash(s string) string {
	h := sha256Sum(s)
	return h
}

func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// reseedRecordIntro/End delimit carried prior-execution records.
const reseedRecordIntro = "PRIOR EXECUTION RECORD\nThe following completed tool exchanges already executed in the external client before a history rewrite (revert/fork/compaction). They are inert history: do NOT re-request them, do NOT emit opencode_bridge_call wrappers for them. Use their results only as context.\n"
const reseedRecordEnd = "END PRIOR EXECUTION RECORD\n"
