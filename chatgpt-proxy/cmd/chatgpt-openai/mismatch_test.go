package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFirstMismatchStructureOnly(t *testing.T) {
	base := message{Role: "assistant", Content: "private text", ToolCalls: []toolCall{{ID: "private-id", Type: "function", Function: callFunction{Name: "private-name", Arguments: `{"n":1}`}}}}
	tests := []struct {
		name, field string
		change      func(*message)
	}{
		{"role", "role", func(m *message) { m.Role = "user" }},
		{"edit", "content", func(m *message) { m.Content = "different private text" }},
		{"encoding", "content", func(m *message) {
			m.Content = []any{map[string]any{"type": "text", "text": "private text", "extra": "meaningful"}}
		}},
		{"call id", "tool_calls[0].id", func(m *message) { m.ToolCalls[0].ID = "other-private-id" }},
		{"arguments", "tool_calls[0].function.arguments", func(m *message) { m.ToolCalls[0].Function.Arguments = `{"n":2}` }},
		{"tool result id", "tool_call_id", func(m *message) { m.ToolCallID = "private-result-id" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			changed.ToolCalls = append([]toolCall(nil), base.ToolCalls...)
			tt.change(&changed)
			got := firstMismatch([]message{{Role: "system", Content: "same"}, changed}, []message{{Role: "system", Content: "same"}, base})
			if got == nil || got.Index != 1 || got.Field != tt.field {
				t.Fatalf("diagnostic: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "private") {
				t.Fatal("private values leaked")
			}
		})
	}
	if got := firstMismatch(nil, []message{base}); got == nil || got.Index != 0 || got.Field != "history.truncated" {
		t.Fatalf("truncated: %+v", got)
	}
}

func TestMismatchEquivalentEncodingsAndJSON(t *testing.T) {
	a := []message{{Role: "assistant", Content: "same", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "echo", Arguments: `{"a":1,"b":2}`}}}}}
	b := []message{{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "same"}}, ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "echo", Arguments: "{\n \"b\": 2, \"a\": 1}"}}}}}
	if got := firstMismatch(a, b); got != nil {
		t.Fatalf("equivalent: %+v", got)
	}
	// Multi-text-part split around an injected advisory equals the plain
	// string in both directions: SDKs split around advisories, and the same
	// logical message must not rotate the thread for it.
	split := []message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "commit and release then\n\n"},
		map[string]any{"type": "text", "text": "<system-reminder>\nMode changed.\n</system-reminder>\n\n"},
	}}}
	plain := []message{{Role: "user", Content: "commit and release then\n\n"}}
	if got := firstMismatch(split, plain); got != nil {
		t.Fatalf("split advisory must match: %+v", got)
	}
	if got := firstMismatch(plain, split); got != nil {
		t.Fatalf("split advisory must match reversed: %+v", got)
	}
	// Genuine text change inside the split still mismatches.
	edited := []message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "commit and release LATER\n\n"},
		map[string]any{"type": "text", "text": "<system-reminder>\nMode changed.\n</system-reminder>\n\n"},
	}}}
	if got := firstMismatch(edited, plain); got == nil {
		t.Fatal("edited split text must still mismatch")
	}
	// Media parts never merge into text.
	withMedia := []message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "commit and release then\n\n"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
	}}}
	if got := firstMismatch(withMedia, plain); got == nil {
		t.Fatal("media split must stay exact")
	}
}

func rewriteBase() []message {
	return []message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "read", Arguments: `{"f":"a"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "file content"},
		{Role: "assistant", Content: "done"},
	}
}

func TestClassifyHistoryRewrite(t *testing.T) {
	saved := rewriteBase()
	// Coherent append-only continuation is not a rewrite: no behavior change.
	appended := append(append([]message{}, saved...), message{Role: "user", Content: "more"})
	if kind, mm := classifyHistoryRewrite(appended, saved); kind != "" || mm != nil {
		t.Fatalf("append classified as %q (%+v)", kind, mm)
	}
	// Empty journal is not a rewrite either: nothing executed to carry.
	if kind, _ := classifyHistoryRewrite(appended, nil); kind != "" {
		t.Fatalf("empty journal classified as %q", kind)
	}
	// Revert/truncation: shorter and prefix-equal.
	truncated := append([]message{}, saved[:2]...)
	if kind, mm := classifyHistoryRewrite(truncated, saved); kind != rewriteTruncation || mm == nil || mm.Field != "history.truncated" {
		t.Fatalf("truncation classified as %q (%+v)", kind, mm)
	}
	// Fork/regeneration: same-length-but-diverged content.
	diverged := append([]message{}, saved...)
	diverged[3] = message{Role: "tool", ToolCallID: "call_1", Content: "regenerated content"}
	if kind, mm := classifyHistoryRewrite(diverged, saved); kind != rewriteDivergence || mm == nil || mm.Index != 3 {
		t.Fatalf("divergence classified as %q (%+v)", kind, mm)
	}
	// Fork that drops the executed exchange but stays coherent.
	forked := []message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hello"}, {Role: "user", Content: "never mind"}}
	if kind, mm := classifyHistoryRewrite(forked, saved); kind != rewriteDivergence || mm == nil {
		t.Fatalf("fork classified as %q (%+v)", kind, mm)
	}
	// Compaction: summarized system prefix.
	compacted := []message{{Role: "system", Content: "Compacted summary of prior work"}, {Role: "user", Content: "continue"}}
	if kind, mm := classifyHistoryRewrite(compacted, saved); kind != rewriteCompaction || mm == nil {
		t.Fatalf("compaction classified as %q (%+v)", kind, mm)
	}
	// Incoherent histories are ambiguous, never a rewrite: dangling call,
	// duplicate IDs, unmatched result, empty result.
	for name, bad := range map[string][]message{
		"dangling":   {{Role: "user", Content: "hi"}, {Role: "assistant", ToolCalls: []toolCall{{ID: "ghost", Type: "function", Function: callFunction{Name: "read", Arguments: `{}`}}}}},
		"duplicate":  {{Role: "assistant", ToolCalls: []toolCall{{ID: "dup", Type: "function", Function: callFunction{Name: "read", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "dup", Content: "x"}, {Role: "assistant", ToolCalls: []toolCall{{ID: "dup", Type: "function", Function: callFunction{Name: "read", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "dup", Content: "y"}},
		"unmatched":  {{Role: "user", Content: "hi"}, {Role: "tool", ToolCallID: "nobody", Content: "x"}},
		"empty":      {{Role: "assistant", ToolCalls: []toolCall{{ID: "e", Type: "function", Function: callFunction{Name: "read", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "e", Content: ""}},
		"unfinished": {{Role: "user", Content: "hi"}, {Role: "assistant", ToolCalls: []toolCall{{ID: "open", Type: "function", Function: callFunction{Name: "read", Arguments: `{}`}}}}},
	} {
		if kind, mm := classifyHistoryRewrite(bad, saved); kind != rewriteAmbiguous || mm == nil {
			t.Fatalf("%s classified as %q (%+v)", name, kind, mm)
		}
	}
}
