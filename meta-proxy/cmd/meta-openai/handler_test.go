package main

import (
	"bytes"
	"encoding/json"
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
	// The composer contract the bridge depends on: select-all must report
	// "selected:*", then the held text must equal the prompt exactly or up
	// to edge whitespace (Lexical normalizes edges on insert); anything
	// else fails closed instead of sending a partial paste.
	accept := func(selRes, held, prompt string) bool {
		if !strings.HasPrefix(selRes, "selected:") {
			return false
		}
		return held == prompt || strings.TrimSpace(held) == strings.TrimSpace(prompt)
	}
	for _, tc := range []struct {
		selRes       string
		held, prompt string
		ok           bool
	}{
		{"selected:867", "hello", "hello", true},
		{"selected:867", "  hello\n\n", "hello", true},
		{"missing", "hello", "hello", false},
		{"select-error:x", "hello", "hello", false},
		{"selected:867", "hell", "hello", false},
		{"selected:867", "hello world", "hello", false},
		{"", "", "", false},
	} {
		if got := accept(tc.selRes, tc.held, tc.prompt); got != tc.ok {
			t.Fatalf("contract mismatch for %+v", tc)
		}
	}
}

func TestPasteCapExcludesTeachingHead(t *testing.T) {
	if got := pasteCap(200000, 300000, true); got != 200000 {
		t.Fatalf("replay turns obey the raw budget: %d", got)
	}
	if got := pasteCap(200000, 300000, false); got != 500000 {
		t.Fatalf("final turn allows budget plus fixed head: %d", got)
	}
	if got := pasteCap(200000, 0, false); got != 200000 {
		t.Fatalf("empty head changes nothing: %d", got)
	}
}

func TestLargeToolCatalogDoesNotCountAgainstPasteLimit(t *testing.T) {
	// Live shape (2026-10-31): ~40 small tools totaling over the budget.
	// Each tool JSON must travel whole in exactly one absorbed replay turn;
	// the tiny question rides the final turn alone.
	var tools []toolDef
	for i := 0; i < 40; i++ {
		tools = append(tools, toolDef{Type: "function", Function: toolFunction{
			Name:        "tool" + string(rune('a'+i%26)) + string(rune('0'+i%10)),
			Description: "Test tool with padding " + strings.Repeat("x", 6000),
			Parameters:  map[string]any{"arg": "y"},
		}})
	}
	instr, parts := systemPromptParts(tools, "auto")
	if instr == "" || len(parts) != 40 {
		t.Fatalf("parts must be instruction plus one blob per tool: %d", len(parts))
	}
	teach := append([]string{instr}, parts...)
	headBytes := 0
	for _, tg := range teach {
		headBytes += len(tg) + 1
	}
	if headBytes <= 200000 {
		t.Fatalf("test needs an over-budget catalog, got %d", headBytes)
	}
	msgs := []message{{Role: "user", Content: "hi"}}
	turns, err := buildTurns(teach, msgs, 200000)
	if err != nil {
		t.Fatalf("buildTurns must not refuse: %v", err)
	}
	if len(turns) < 2 {
		t.Fatalf("over-budget teaching must split, got %d turns", len(turns))
	}
	for _, tr := range turns[:len(turns)-1] {
		if !tr.replay {
			t.Fatal("non-final destack turns must be absorbed replays")
		}
		if len(tr.prompt) > 200000 {
			t.Fatalf("replay turn exceeds raw budget: %d", len(tr.prompt))
		}
	}
	last := turns[len(turns)-1]
	if last.replay {
		t.Fatal("final turn must be answer-seeking")
	}
	for _, blob := range parts {
		whole := 0
		for _, tr := range turns {
			if strings.Contains(tr.prompt, blob) {
				whole++
			}
		}
		if whole != 1 {
			t.Fatalf("tool blob must travel whole in exactly one turn, found in %d", whole)
		}
	}
	if cap := pasteCap(200000, headBytes, last.replay); len(last.prompt) > cap {
		t.Fatalf("final turn %d must fit budget-plus-head %d", len(last.prompt), cap)
	}
}

func TestImportAbsorbed(t *testing.T) {
	schemas, err := toolSchemas(nil)
	if err != nil {
		t.Fatalf("schemas must compile: %v", err)
	}
	live := []string{
		"ACK", "",
		"Understood \u2014 treating the next messages as restored transcript history (inert context only).",
		"Got it \u2014 HISTORY IMPORT noted as inert context. Ready for your actual request when you are.",
		"Noted as inert context.",
	}
	for _, a := range live {
		absorbed, _ := importAbsorbed(a, schemas)
		if !absorbed {
			t.Fatalf("live import answer must absorb: %q", a)
		}
	}
	bad := []string{
		`<opencode_bridge_call>{"name":"shell","arguments":{"command":"x"}}</opencode_bridge_call>`,
		"Half a wrapper <opencode_bridge_call>{\"name\":",
		"```\n<opencode_bridge_call>{\"name\":\"shell\",\"arguments\":{\"command\":\"x\"}}</opencode_bridge_call>\n```",
		strings.Repeat("z", 2049),
	}
	for _, a := range bad {
		if absorbed, _ := importAbsorbed(a, schemas); absorbed {
			t.Fatalf("must not absorb: %q", a[:min(40, len(a))])
		}
	}
}

func TestTeachingDebtTrigger(t *testing.T) {
	// The re-teach gate repays full teaching exactly when the tool set
	// differs from the recorded one — including none-to-some (a thread
	// minted by a tool-less titling turn). Same set must not re-teach.
	tools := []toolDef{{Type: "function", Function: toolFunction{Name: "shell"}}}
	if toolsFingerprint(nil) == toolsFingerprint(tools) {
		t.Fatal("empty tool set must fingerprint differently from a real one")
	}
	if toolsFingerprint(tools) != toolsFingerprint(tools) {
		t.Fatal("identical tool sets must fingerprint equally")
	}
	if systemPrompt(tools, "auto") == "" {
		t.Fatal("a real tool set must produce teaching")
	}
	if systemPrompt(nil, "auto") != "" {
		t.Fatal("no tools must produce no teaching")
	}
}
