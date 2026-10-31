package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bojackduy/deepseek-proxy/internal/deepseek"
)

type historyEntry struct {
	MessageID string `json:"message_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

// fakeUpstream implements the handful of DeepSeek web endpoints the bridge
// uses, including a solvable PoW challenge.
type fakeUpstream struct {
	mu             sync.Mutex
	creates        int
	completions    int
	prompts        []string
	parents        []string
	answer         string
	failCompletion bool
	// rejectOverBytes simulates remote prompt validation (live http_422):
	// a completion whose prompt exceeds the budget is rejected. Zero means
	// no limit.
	rejectOverBytes int
	history        []historyEntry
	historyOK      bool
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v0/chat/create_pow_challenge":
		salt, expire := "salt", int64(1700000000)
		target, _ := deepseek.DeepSeekHashV1([]byte(fmt.Sprintf("%s_%d_%d", salt, expire, 0)))
		writeTestJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"biz_data": map[string]any{"challenge": map[string]any{
				"algorithm": "DeepSeekHashV1", "challenge": target, "salt": salt,
				"difficulty": 1, "expire_at": expire, "signature": "sig",
				"target_path": "/api/v0/chat/completion",
			}}},
		})
	case "/api/v0/chat_session/create":
		f.mu.Lock()
		f.creates++
		id := fmt.Sprintf("remote-%d", f.creates)
		f.mu.Unlock()
		writeTestJSON(w, map[string]any{"code": 0, "data": map[string]any{"biz_data": map[string]any{
			"chat_session": map[string]any{"id": id},
		}}})
	case "/api/v0/chat/completion":
		var body struct {
			SessionID string `json:"chat_session_id"`
			Parent    any    `json:"parent_message_id"`
			Prompt    string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.completions++
		f.prompts = append(f.prompts, body.Prompt)
		parent := ""
		if s, ok := body.Parent.(string); ok {
			parent = s
		}
		f.parents = append(f.parents, parent)
		answer := f.answer
		fail := f.failCompletion
		reject := f.rejectOverBytes
		promptLen := len(body.Prompt)
		f.mu.Unlock()
		if reject > 0 && promptLen > reject {
			http.Error(w, `{"code":400}`, http.StatusUnprocessableEntity)
			return
		}
		if fail {
			http.Error(w, `{"code":500}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":\"m-1\"}\n\n")
		for _, part := range splitRunes(answer, 7) {
			b, _ := json.Marshal(map[string]any{"p": "response/fragments/-1/content", "v": part})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: {\"p\":\"response/status\",\"v\":\"FINISHED\"}\n\n")
	case "/api/v0/chat/history_messages":
		f.mu.Lock()
		ok, hist := f.historyOK, append([]historyEntry(nil), f.history...)
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not available", http.StatusInternalServerError)
			return
		}
		writeTestJSON(w, map[string]any{"code": 0, "data": map[string]any{"biz_data": map[string]any{
			"chat_messages": hist,
		}}})
	default:
		http.NotFound(w, r)
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type harness struct {
	bridge *httptest.Server
	up     *fakeUpstream
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	up := &fakeUpstream{answer: "hello"}
	upstream := httptest.NewServer(up)
	t.Cleanup(upstream.Close)

	store, err := deepseek.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(deepseek.AuthData{Token: "test"}, false); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{client: deepseek.NewClient(store, upstream.URL), journal: j}
	bridge := httptest.NewServer(srv.routes())
	t.Cleanup(bridge.Close)
	return &harness{bridge: bridge, up: up}
}

func (h *harness) chat(t *testing.T, session string, body map[string]any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.bridge.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-session-id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func user(s string) map[string]any { return map[string]any{"role": "user", "content": s} }
func assistant(s string) map[string]any {
	return map[string]any{"role": "assistant", "content": s}
}

func TestAppendThenRevertReseeds(t *testing.T) {
	h := newHarness(t)
	msgs := []any{user("hi")}
	code, body := h.chat(t, "s1", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 200 || !strings.Contains(string(body), "hello") {
		t.Fatalf("turn1 code=%d body=%s", code, body)
	}
	if h.up.creates != 1 || h.up.completions != 1 {
		t.Fatalf("turn1 creates=%d completions=%d", h.up.creates, h.up.completions)
	}

	// Next turn extends the committed history: append to the same remote thread.
	msgs = append(msgs, assistant("hello"), user("again"))
	code, _ = h.chat(t, "s1", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 200 || h.up.creates != 1 || h.up.completions != 2 {
		t.Fatalf("turn2 code=%d creates=%d completions=%d", code, h.up.creates, h.up.completions)
	}
	if h.up.parents[1] != "m-1" {
		t.Fatalf("append parent=%q want m-1", h.up.parents[1])
	}
	if strings.Contains(h.up.prompts[1], "User: hi") {
		t.Fatalf("append prompt replayed committed history: %q", h.up.prompts[1])
	}

	// Revert: the last turn is replaced, so the history is not an extension.
	msgs = []any{user("hi"), assistant("hello"), user("reverted")}
	code, body = h.chat(t, "s1", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 200 {
		t.Fatalf("revert code=%d body=%s", code, body)
	}
	if h.up.creates != 2 {
		t.Fatalf("revert creates=%d want 2 (fresh remote session)", h.up.creates)
	}
	if !strings.Contains(h.up.prompts[2], "User: hi") || !strings.Contains(h.up.prompts[2], "User: reverted") {
		t.Fatalf("reseed prompt missing history: %q", h.up.prompts[2])
	}
}

func TestToolCallAndCanonicalEcho(t *testing.T) {
	h := newHarness(t)
	h.up.answer = `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	tools := []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "get_weather", "description": "weather",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}},
	}}}
	msgs := []any{user("weather?")}
	code, body := h.chat(t, "s2", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	c := resp.Choices[0]
	if c.FinishReason != "tool_calls" || len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool response = %s", body)
	}
	// The client echoes the assistant turn, with null content and no call type.
	msgs = append(msgs, map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id": c.Message.ToolCalls[0].ID,
			"function": map[string]any{
				"name":      "get_weather",
				"arguments": `{ "city": "Hanoi" }`,
			},
		}},
	}, map[string]any{"role": "tool", "tool_call_id": c.Message.ToolCalls[0].ID, "name": "get_weather", "content": `{"temp":30}`})
	code, body = h.chat(t, "s2", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	if code != 200 {
		t.Fatalf("tool-result turn code=%d body=%s", code, body)
	}
	if h.up.creates != 1 {
		t.Fatalf("canonical echo should append, creates=%d", h.up.creates)
	}
	if !strings.Contains(h.up.prompts[1], "[Tool Result: get_weather]") {
		t.Fatalf("tool result not rendered: %q", h.up.prompts[1])
	}
}

func TestInterruptedTurnRecoveredFromHistory(t *testing.T) {
	h := newHarness(t)
	h.up.failCompletion = true
	msgs := []any{user("will fail")}
	code, body := h.chat(t, "s3", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 502 {
		t.Fatalf("code=%d body=%s", code, body)
	}

	// The remote turn actually completed before the transport died.
	h.up.failCompletion = false
	h.up.historyOK = true
	h.up.history = []historyEntry{{MessageID: "m-9", Role: "assistant", Content: "recovered"}}
	before := h.up.completions
	code, body = h.chat(t, "s3", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 200 || !strings.Contains(string(body), "recovered") {
		t.Fatalf("retry code=%d body=%s", code, body)
	}
	if h.up.completions != before {
		t.Fatalf("retry resent the prompt: completions=%d want %d", h.up.completions, before)
	}
}

func TestUnresolvedRetryRefusesWithoutResending(t *testing.T) {
	h := newHarness(t)
	h.up.failCompletion = true
	msgs := []any{user("will fail")}
	if code, _ := h.chat(t, "s4", map[string]any{"model": "deepseek-web", "messages": msgs}); code != 502 {
		t.Fatalf("first code=%d", code)
	}
	h.up.failCompletion = false
	h.up.historyOK = false
	before := h.up.completions
	code, body := h.chat(t, "s4", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 409 || !strings.Contains(string(body), `"pending":true`) {
		t.Fatalf("retry code=%d body=%s", code, body)
	}
	if h.up.completions != before {
		t.Fatalf("refused retry still sent: %d -> %d", before, h.up.completions)
	}

	// A new message after an unresolved turn must still work (reseed).
	msgs = append(msgs, user("new question"))
	code, body = h.chat(t, "s4", map[string]any{"model": "deepseek-web", "messages": msgs})
	if code != 200 {
		t.Fatalf("new turn after unresolved code=%d body=%s", code, body)
	}
}

func TestModelSwitchKeepsThread(t *testing.T) {
	h := newHarness(t)
	msgs := []any{user("hi")}
	if code, _ := h.chat(t, "s5", map[string]any{"model": "deepseek-web", "messages": msgs}); code != 200 {
		t.Fatalf("turn1")
	}
	msgs = append(msgs, assistant("hello"), user("switch"))
	if code, _ := h.chat(t, "s5", map[string]any{"model": "deepseek-reasoner", "messages": msgs}); code != 200 {
		t.Fatalf("turn2")
	}
	if h.up.creates != 1 {
		t.Fatalf("model switch reseeded; creates=%d", h.up.creates)
	}
}

func TestToolReplyMarkdownEnvelope(t *testing.T) {
	var tools []toolDef
	if err := json.Unmarshal([]byte(`[{"type":"function","function":{"name":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}}}]`), &tools); err != nil {
		t.Fatal(err)
	}
	call := `<opencode_bridge_call>{"name":"run","arguments":{"command":"echo ` + "`quoted`" + `"}}</opencode_bridge_call>`
	for _, fence := range []string{"```", "```json", "```xml", "~~~json"} {
		closing := fence[:3]
		out, err := parseAnswer(fence+"\n"+call+"\n"+closing, tools, nil)
		if err != nil || len(out.Calls) != 1 || out.Text != "" {
			t.Fatalf("fence %q: calls=%d text=%q err=%v", fence, len(out.Calls), out.Text, err)
		}
		if out.Calls[0].Function.Arguments != `{"command":"echo `+"`quoted`"+`"}` {
			t.Fatal("argument bytes changed")
		}
	}
	for _, reply := range []string{
		"Example:\n```json\n" + call + "\n```",
		"```json\n" + call + "\n```\nThis is an example.",
		"```json\n" + call,
		"```json\n<opencode_bridge_call>{\"name\":\"run\",\"arguments\":{\"command\":\"x\"c}}</opencode_bridge_call>\n```",
		"```json\n<opencode_bridge_call>{\"name\":\"run\",\"arguments\":{}}</opencode_bridge_call>\n```",
	} {
		if _, err := parseAnswer(reply, tools, nil); err == nil {
			t.Fatalf("unsafe or invalid call accepted: %q", reply)
		}
	}
	if _, err := parseAnswer("```json\n"+call+"\n```", tools, "none"); err == nil {
		t.Fatal("tool_choice none bypassed")
	}
	prose := "```go\nfmt.Println(1)\n```"
	if out, err := parseAnswer(prose, tools, nil); err != nil || out.Text != prose {
		t.Fatalf("ordinary Markdown changed: %q %v", out.Text, err)
	}
}

// TestLargeReseedTrimsOldestHistory reproduces the live http_422 shape:
// a reseed with hundreds of KB of history and 13 tools must fit the remote
// validation budget. Oldest history is dropped first; the newest user message
// is never dropped and the turn still completes.
func TestLargeReseedTrimsOldestHistory(t *testing.T) {
	h := newHarness(t)
	var tools []any
	for i := 0; i < 13; i++ {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name":        fmt.Sprintf("tool_%d", i),
			"description": "test tool",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}})
	}
	msgs := []any{}
	for i := 0; i < 120; i++ {
		msgs = append(msgs, user("history message number "+fmt.Sprint(i)+" "+strings.Repeat("x", 4000)))
		msgs = append(msgs, assistant("ack "+fmt.Sprint(i)))
	}
	newest := "newest user question " + strings.Repeat("y", 100)
	msgs = append(msgs, user(newest))
	code, body := h.chat(t, "s-large", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	if code != 200 {
		t.Fatalf("large reseed code=%d body=%s", code, body)
	}
	if len(h.up.prompts) != 1 {
		t.Fatalf("prompts=%d want 1", len(h.up.prompts))
	}
	if len(h.up.prompts[0]) > maxReseedPromptBytes {
		t.Fatalf("reseed bytes=%d over budget=%d", len(h.up.prompts[0]), maxReseedPromptBytes)
	}
	if !strings.Contains(h.up.prompts[0], newest) {
		t.Fatal("newest user message dropped from reseeded prompt")
	}
	if strings.Contains(h.up.prompts[0], "history message number 0") {
		t.Fatal("oldest history was not trimmed")
	}
}

// TestReproOversizedToolResultAppend is Phase-1 reproduction for the
// tool-call crash loop: a delivered tool call followed by an append turn
// carrying a large tool result exceeds the remote prompt validation budget
// (live: http_422, reseed cap maxReseedPromptBytes already exists but the
// append path is uncapped). The failed turn leaves a pending marker, and
// identical retries wedge in retry/history_error 409s. Desired: the turn
// completes on a budget-fitting prompt with the newest message intact.
func TestReproOversizedToolResultAppend(t *testing.T) {
	h := newHarness(t)
	h.up.answer = `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	h.up.rejectOverBytes = maxReseedPromptBytes
	tools := []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "get_weather", "description": "weather",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}},
	}}}
	msgs := []any{user("weather?")}
	code, body := h.chat(t, "s-repro", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	if code != 200 {
		t.Fatalf("turn1 code=%d body=%s", code, body)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("turn1 no tool call: %s", body)
	}
	callID := resp.Choices[0].Message.ToolCalls[0].ID

	// Append turn: canonical echo plus a large tool result and a new question.
	newest := "and tomorrow?"
	msgs = append(msgs, map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id":       callID,
			"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Hanoi"}`},
		}},
	}, map[string]any{"role": "tool", "tool_call_id": callID, "name": "get_weather", "content": strings.Repeat("r", 100<<10)},
		user(newest))
	code, body = h.chat(t, "s-repro", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	appendBytes := 0
	if len(h.up.prompts) > 1 {
		appendBytes = len(h.up.prompts[1])
	}
	t.Logf("REPRO evidence: append code=%d prompt_bytes=%d budget=%d creates=%d completions=%d",
		code, appendBytes, maxReseedPromptBytes, h.up.creates, h.up.completions)

	// Identical retries with history down: the reported permanent 409 loop.
	h.up.historyOK = false
	code2, _ := h.chat(t, "s-repro", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	code3, _ := h.chat(t, "s-repro", map[string]any{"model": "deepseek-web", "messages": msgs, "tools": tools})
	t.Logf("REPRO evidence: retry codes=%d,%d completions=%d (loop when both 409)", code2, code3, h.up.completions)

	if code != 200 {
		t.Fatalf("oversized tool-result turn wedged: code=%d body=%s", code, body)
	}
	for i, p := range h.up.prompts {
		if len(p) > maxReseedPromptBytes {
			t.Fatalf("prompt[%d] bytes=%d over budget=%d", i, len(p), maxReseedPromptBytes)
		}
	}
	if last := h.up.prompts[len(h.up.prompts)-1]; !strings.Contains(last, newest) {
		t.Fatal("newest user message dropped from submitted prompt")
	}
}
