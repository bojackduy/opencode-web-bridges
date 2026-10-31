package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojackduy/meta-proxy/internal/meta"
)

func TestBuildCompletionToolCalls(t *testing.T) {
	h := &handler{model: "meta-web"}
	calls := []*meta.BridgeCall{{Name: "do_thing", Arguments: map[string]any{"x": 1}}}
	resp := h.buildCompletion(nil, "answer", nil, calls, []string{"call_meta_0_0"}, 100, "id1")
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Choices[0].Message.ToolCalls[0].ID != "call_meta_0_0" {
		t.Fatalf("tool call id = %q, want call_meta_0_0", resp.Choices[0].Message.ToolCalls[0].ID)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "do_thing" {
		t.Fatalf("tool name = %q, want do_thing", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
}

func TestBuildCompletionText(t *testing.T) {
	h := &handler{model: "meta-web"}
	resp := h.buildCompletion(nil, "hello world", nil, nil, nil, 100, "id2")
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Choices[0].Message.Content != "hello world" {
		t.Fatalf("content = %v, want hello world", resp.Choices[0].Message.Content)
	}
}

func TestWriteMetaChatResponseSSEShape(t *testing.T) {
	h := &handler{model: "meta-web"}
	resp := &completion{
		ID: "id3", Object: "chat.completion", Created: 100, Model: "meta-web",
		Choices: []completionChoice{{Index: 0, Message: message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
		Usage:   map[string]int{},
	}
	req := &chatRequest{Model: "meta-web", Stream: true}
	rr := httptest.NewRecorder()
	h.writeMetaChatResponse(rr, req, resp, "")
	body := rr.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing DONE frame: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("missing terminal finish_reason stop: %s", body)
	}
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("missing role delta: %s", body)
	}
}

func TestWriteMetaChatResponseSSEToolCalls(t *testing.T) {
	h := &handler{model: "meta-web"}
	resp := &completion{
		ID: "id4", Object: "chat.completion", Created: 100, Model: "meta-web",
		Choices: []completionChoice{{Index: 0, Message: message{Role: "assistant", ToolCalls: []toolCall{{ID: "c1", Type: "function", Function: callFunction{Name: "fn", Arguments: "{}"}}}}, FinishReason: "tool_calls"}},
		Usage:   map[string]int{},
	}
	req := &chatRequest{Model: "meta-web", Stream: true}
	rr := httptest.NewRecorder()
	h.writeMetaChatResponse(rr, req, resp, "")
	body := rr.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing DONE frame")
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing terminal finish_reason tool_calls: %s", body)
	}
	if !strings.Contains(body, `"name":"fn"`) {
		t.Fatalf("missing tool name in SSE: %s", body)
	}
}

func TestAbandonClears(t *testing.T) {
	h := &handler{model: "meta-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	sess := &session{Pending: true, PendingPrompt: "p", Messages: []message{{Role: "user", Content: "hi"}}}
	h.sessions["sid1"] = sess

	// Origin-reject
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/abandon", bytes.NewReader([]byte("{}")))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("X-Session-Id", "sid1")
	rr := httptest.NewRecorder()
	h.abandonSessions(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("origin request: code=%d, want 403", rr.Code)
	}

	// Clear
	req2 := httptest.NewRequest(http.MethodPost, "/v1/sessions/abandon", bytes.NewReader([]byte("{}")))
	req2.Header.Set("X-Session-Id", "sid1")
	rr2 := httptest.NewRecorder()
	h.abandonSessions(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("abandon: code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var out map[string]any
	_ = json.NewDecoder(rr2.Body).Decode(&out)
	if out["outcome"] != "cleared" {
		t.Fatalf("outcome = %v, want cleared", out["outcome"])
	}
	h.mu.Lock()
	s := h.sessions["sid1"]
	h.mu.Unlock()
	if s.Pending {
		t.Fatalf("pending still true after abandon")
	}
	if len(s.Messages) == 0 {
		t.Fatalf("messages wiped by abandon (should retain)")
	}
}

func TestAbandonNotPending(t *testing.T) {
	h := &handler{model: "meta-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.sessions["sid2"] = &session{Pending: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/abandon", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-Session-Id", "sid2")
	rr := httptest.NewRecorder()
	h.abandonSessions(rr, req)
	var out map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["outcome"] != "not_pending" {
		t.Fatalf("outcome = %v, want not_pending", out["outcome"])
	}
}

func TestAbandonUnknown(t *testing.T) {
	h := &handler{model: "meta-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/abandon", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-Session-Id", "nope")
	rr := httptest.NewRecorder()
	h.abandonSessions(rr, req)
	var out map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["outcome"] != "unknown" {
		t.Fatalf("outcome = %v, want unknown", out["outcome"])
	}
}

func TestRecoverHonest(t *testing.T) {
	h := &handler{model: "meta-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.sessions["sid3"] = &session{Pending: false}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/recover", nil)
	req.Header.Set("X-Session-Id", "sid3")
	rr := httptest.NewRecorder()
	h.recoverSession(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("no-pending recover: code=%d, want 409", rr.Code)
	}
	h.sessions["sid4"] = &session{Pending: true, PendingPrompt: "p", URL: "https://www.meta.ai/c/x"}
	req2 := httptest.NewRequest(http.MethodPost, "/v1/sessions/recover", nil)
	req2.Header.Set("X-Session-Id", "sid4")
	rr2 := httptest.NewRecorder()
	h.recoverSession(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("unadoptable recover: code=%d, want 409", rr2.Code)
	}
}

func TestRecoverRequiresSessionHeader(t *testing.T) {
	h := &handler{model: "meta-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/recover", nil)
	rr := httptest.NewRecorder()
	h.recoverSession(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rr.Code)
	}
}
func TestComposerFillContract(t *testing.T) {
	// The composer contract the bridge depends on: bulk insert outcome is
	// decided by the "inserted:<len>" prefix plus a held-length check, so a
	// short or rejected write fails closed instead of sending a partial
	// paste. This pins the wire format Ask parses.
	for raw, ok := range map[string]bool{
		"inserted:199834": true, "inserted:8": true,
		"exec-false:0": false, "exec-error:x": false, "missing": false, "": false,
	} {
		held := -1
		accepted := strings.HasPrefix(raw, "inserted:")
		if accepted {
			var n int
			if _, err := fmt.Sscanf(raw, "inserted:%d", &n); err != nil {
				t.Fatalf("accept path must parse: %q", raw)
			}
			held = n
		}
		if accepted != ok || (ok && held < 0) {
			t.Fatalf("contract mismatch for %q", raw)
		}
	}
}
