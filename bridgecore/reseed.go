package bridgecore

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Rewrite classifications. "ambiguous" is not a rewrite.
const (
	RewriteTruncation   = "rewrite-truncation"
	RewriteDivergence   = "rewrite-divergence"
	RewriteCompaction   = "rewrite-compaction"
	RewriteContinuation = "rewrite-continuation"
	RewriteAmbiguous    = "ambiguous"
)

// Message is the shared, provider-agnostic transcript shape. Bridges convert
// at their boundary.
type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// FunctionCall is one tool call's function payload.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is one assistant tool invocation.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// PrefixMismatch contains structure only (no text/IDs).
type PrefixMismatch struct {
	Index            int
	Field            string
	IncomingEncoding string
	SavedEncoding    string
	IncomingShape    string
	SavedShape       string
	ContentOff       int
}

// ToolCall conversion helpers: bridgecore.ToolCall <-> local. (Bridges adapt.)

// textOf returns the textual content of a message.
func textOf(m Message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if obj, ok := p.(map[string]any); ok && obj["type"] == "text" {
				if s, ok := obj["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

var (
	systemReminderRe       = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>\n*`)
	skillBlockRe           = regexp.MustCompile(`(?s)<skill>.*?</skill>`)
	skillLocationRe        = regexp.MustCompile(`<location>[^<]*</location>`)
	skillDescriptionRe     = regexp.MustCompile(`(?s)<description>.*?</description>`)
	dcpMessageIDRe         = regexp.MustCompile(``)
	dcpReminderRe          = regexp.MustCompile(`(?s)\n*\n*(?:|@[1-9][0-9]*@)\s*$`)
)

func stripSystemReminder(s string) string {
	return systemReminderRe.ReplaceAllString(s, "")
}

func normalizeSystem(s string) string {
	s = skillBlockRe.ReplaceAllStringFunc(s, func(block string) string {
		block = skillLocationRe.ReplaceAllString(block, "<location></location>")
		return skillDescriptionRe.ReplaceAllString(block, "<description></description>")
	})
	return stripCompressReminder(s)
}

func stripCompressReminder(s string) string {
	const opener = "You operate in a context-constrained environment."
	lines := strings.Split(s, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, opener) {
			start = i
			break
		}
	}
	if start < 0 {
		return s
	}
	end := -1
	for i := start; i < len(lines); i++ {
		if strings.Contains(lines[i], "") {
			end = i
			break
		}
	}
	if end < 0 {
		return s
	}
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	return strings.Join(append(lines[:start], lines[end+1:]...), "\n")
}

func concatTextParts(parts []any) (string, bool) {
	if len(parts) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok || p["type"] != "text" || len(p) != 2 {
			return "", false
		}
		t, ok := p["text"].(string)
		if !ok {
			return "", false
		}
		b.WriteString(t)
	}
	return b.String(), true
}

func canonicalMessages(msgs []Message) []Message {
	out := append([]Message{}, msgs...)
	for i := range out {
		if out[i].Content == nil {
			out[i].Content = ""
		} else if parts, ok := out[i].Content.([]any); ok {
			if text, ok := concatTextParts(parts); ok {
				if out[i].Role != "system" {
					text = stripSystemReminder(text)
				}
				out[i].Content = text
			}
		}
		if out[i].Role == "system" {
			if s, ok := out[i].Content.(string); ok {
				out[i].Content = normalizeSystem(s)
			}
		}
		if s, ok := out[i].Content.(string); ok {
			if out[i].Role == "assistant" {
				s = dcpReminderRe.ReplaceAllString(s, "")
			}
			out[i].Content = dcpMessageIDRe.ReplaceAllString(s, "")
		}
		out[i].ToolCalls = append([]ToolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			var v any
			decoder := json.NewDecoder(strings.NewReader(out[i].ToolCalls[j].Function.Arguments))
			decoder.UseNumber()
			if json.Valid([]byte(out[i].ToolCalls[j].Function.Arguments)) && decoder.Decode(&v) == nil {
				if b, err := json.Marshal(v); err == nil {
					out[i].ToolCalls[j].Function.Arguments = string(b)
				}
			}
		}
	}
	return out
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

func contentEncoding(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case []any:
		return "parts"
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

func sameJSON(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && string(x) == string(y)
}

func jsonEqual(a, b string) bool {
	var va, vb any
	if !json.Valid([]byte(a)) || !json.Valid([]byte(b)) {
		return a == b
	}
	_ = json.Unmarshal([]byte(a), &va)
	_ = json.Unmarshal([]byte(b), &vb)
	x, _ := json.Marshal(va)
	y, _ := json.Marshal(vb)
	return string(x) == string(y)
}

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

func FirstMismatch(incoming, saved []Message) *PrefixMismatch {
	a, b := canonicalMessages(incoming), canonicalMessages(saved)
	for i := range b {
		m := &PrefixMismatch{Index: i, ContentOff: -1}
		if i >= len(a) {
			m.Field = "history.truncated"
			return m
		}
		m.IncomingEncoding, m.SavedEncoding = contentEncoding(incoming[i].Content), contentEncoding(saved[i].Content)
		m.IncomingShape, m.SavedShape = contentShape(a[i].Content), contentShape(b[i].Content)
		switch {
		case a[i].Role != b[i].Role:
			m.Field = "role"
		case a[i].Name != b[i].Name:
			m.Field = "name"
		case a[i].ToolCallID != b[i].ToolCallID:
			m.Field = "tool_call_id"
		case !sameJSON(a[i].Content, b[i].Content):
			m.Field = "content"
			if ai, ok := a[i].Content.(string); ok {
				if bi, ok := b[i].Content.(string); ok {
					m.ContentOff = firstDiffOff(ai, bi)
				}
			}
		case len(a[i].ToolCalls) != len(b[i].ToolCalls):
			m.Field = "tool_calls.length"
		default:
			for j, call := range a[i].ToolCalls {
				other := b[i].ToolCalls[j]
				field := ""
				switch {
				case call.ID != other.ID:
					field = "id"
				case call.Type != other.Type:
					field = "type"
				case call.Function.Name != other.Function.Name:
					field = "function.name"
				case call.Function.Arguments != other.Function.Arguments:
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

func CoherentTranscript(msgs []Message) bool {
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

func IsCompactionShape(incoming []Message) bool {
	if len(incoming) == 0 || incoming[0].Role != "system" {
		return false
	}
	lower := strings.ToLower(textOf(incoming[0]))
	return strings.Contains(lower, "compact") || strings.Contains(lower, "summar")
}

func ClassifyHistoryRewrite(incoming, saved []Message) (string, *PrefixMismatch) {
	mm := FirstMismatch(incoming, saved)
	if mm == nil || len(saved) == 0 {
		return "", mm
	}
	if !CoherentTranscript(incoming) {
		return RewriteAmbiguous, mm
	}
	if mm.Field == "history.truncated" {
		return RewriteTruncation, mm
	}
	if IsCompactionShape(incoming) {
		return RewriteCompaction, mm
	}
	return RewriteDivergence, mm
}

func EchoBloatSkipped(saved, incoming []Message, tol int) (bool, int) {
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

func SystemOnlyDivergence(saved, incoming []Message) bool {
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

func canonicalEqual(a, b Message) bool {
	ca := canonicalMessages([]Message{a})[0]
	cb := canonicalMessages([]Message{b})[0]
	x, _ := json.Marshal(ca)
	y, _ := json.Marshal(cb)
	return string(x) == string(y)
}

func callPresentIn(call ToolCall, incoming []Message) bool {
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

func CarryExecuted(saved, pending, incoming []Message) (carried []Message, carriedIDs []string, incomplete []string) {
	results := map[string]Message{}
	for _, msgs := range [][]Message{saved, pending} {
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID != "" && textOf(m) != "" && results[m.ToolCallID].ToolCallID == "" {
				results[m.ToolCallID] = m
			}
		}
	}
	seen := map[string]bool{}
	for _, msgs := range [][]Message{saved, pending} {
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
