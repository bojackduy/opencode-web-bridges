package bridgecore

import "testing"

func TestCoherentTranscript(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	if !CoherentTranscript(msgs) {
		t.Fatal("expected coherent")
	}
}

func TestClassifyHistoryRewriteTruncation(t *testing.T) {
	saved := []Message{{Role: "user", Content: "hi"}}
	incoming := []Message{}
	kind, mm := ClassifyHistoryRewrite(incoming, saved)
	if kind != RewriteTruncation || mm == nil || mm.Field != "history.truncated" {
		t.Fatalf("got %s %+v", kind, mm)
	}
}

func TestEchoBloatSkipped(t *testing.T) {
	saved := []Message{{Role: "assistant", Content: "hi"}}
	incoming := []Message{{Role: "assistant", Content: "hi there"}}
	ok, skipped := EchoBloatSkipped(saved, incoming, 1024)
	if !ok || skipped != 6 {
		t.Fatalf("ok=%v skipped=%d", ok, skipped)
	}
}
