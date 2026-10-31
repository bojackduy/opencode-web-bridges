package deepseek

import (
	"testing"
)

func TestStreamParserJSONPatch(t *testing.T) {
	parser := NewStreamParser()

	// Initial event
	chunk, err := parser.Feed("event: ready")
	if err != nil || chunk != nil {
		t.Fatalf("expected nil chunk on event line")
	}

	chunk, err = parser.Feed(`data: {"response_message_id": 12345}`)
	if err != nil || chunk != nil {
		t.Fatalf("expected nil chunk on ready data")
	}

	// Fragments append
	chunk, err = parser.Feed(`data: {"p": "response/fragments", "v": [{"type": "THINK", "content": "Thinking..."}]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || chunk.ReasoningContent != "Thinking..." {
		t.Errorf("chunk.ReasoningContent = %q; want 'Thinking...'", chunk.ReasoningContent)
	}

	// Fragment text delta
	chunk, err = parser.Feed(`data: {"p": "response/fragments/-1/content", "v": " more thinking"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || chunk.ReasoningContent != " more thinking" {
		t.Errorf("chunk.ReasoningContent = %q; want ' more thinking'", chunk.ReasoningContent)
	}

	// Fragment switch to response
	chunk, err = parser.Feed(`data: {"p": "response/fragments", "v": [{"type": "RESPONSE", "content": "Hello world"}]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || chunk.Content != "Hello world" {
		t.Errorf("chunk.Content = %q; want 'Hello world'", chunk.Content)
	}

	// Finish
	chunk, err = parser.Feed(`data: {"p": "response/status", "v": "FINISHED"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || chunk.FinishReason != "stop" {
		t.Errorf("chunk.FinishReason = %q; want 'stop'", chunk.FinishReason)
	}
}

func TestStreamParserOpenAIDelta(t *testing.T) {
	parser := NewStreamParser()

	chunk, err := parser.Feed(`data: {"choices": [{"delta": {"content": "Salom", "reasoning_content": "O'ylash"}}], "finish_reason": null}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || chunk.ReasoningContent != "O'ylash" {
		t.Errorf("expected reasoning chunk")
	}

	chunk, err = parser.Feed(`data: [DONE]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk == nil || chunk.FinishReason != "stop" {
		t.Errorf("chunk.FinishReason = %q; want 'stop'", chunk.FinishReason)
	}
}
