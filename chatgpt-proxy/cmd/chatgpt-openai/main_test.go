package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"chatgpt-proxy/internal/chatgpt"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

type fakeBackend struct {
	prompts, urls []string
	projects      []string // project target per turn: opt.ProjectURL, else opt.ProjectName
	answers       []string
	atts          []int
	thinking      []bool
	reasoning     string
	err           error
	errorURL      string
	failAt        int
	probe         int
	// projectEcho, when set, is returned as res.ProjectURL for
	// project-targeted new chats (simulating the project mint).
	projectEcho string
	turn        func(context.Context, chatgpt.TurnOptions) (chatgpt.TurnResult, error)
}

func (f *fakeBackend) AskThreadTurn(ctx context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
	if f.turn != nil {
		return f.turn(ctx, opt)
	}
	if ctx.Err() != nil {
		return chatgpt.TurnResult{}, ctx.Err()
	}
	f.prompts = append(f.prompts, opt.Prompt)
	f.urls = append(f.urls, opt.URL)
	proj := opt.ProjectURL
	if proj == "" {
		proj = opt.ProjectName
	}
	f.projects = append(f.projects, proj)
	f.atts = append(f.atts, len(opt.Attachments))
	f.thinking = append(f.thinking, opt.Thinking)
	if f.err != nil && (f.failAt == 0 || len(f.prompts) == f.failAt) {
		return chatgpt.TurnResult{URL: f.errorURL}, f.err
	}
	url := opt.URL
	if url == "" {
		url = fmt.Sprintf("https://chatgpt.com/c/thread-%d", len(f.prompts))
	}
	i := len(f.prompts) - 1
	answer := "OK"
	if strings.HasPrefix(opt.Prompt, historyIntro) {
		answer = "ACK"
	}
	if i < len(f.answers) {
		answer = f.answers[i]
	}
	resProj := ""
	if opt.ProjectURL != "" {
		resProj = opt.ProjectURL
	} else if opt.ProjectName != "" {
		resProj = f.projectEcho
	}
	return chatgpt.TurnResult{Answer: answer, URL: url, Reasoning: f.reasoning, ProjectURL: resProj}, nil
}
func (f *fakeBackend) LoginState(context.Context) (*chatgpt.Status, error) {
	f.probe++
	return &chatgpt.Status{LoggedIn: true, Composer: true, URL: "https://chatgpt.com/c/probe"}, f.err
}
func testHandler(f *fakeBackend) *handler {
	return &handler{client: f, model: "chatgpt-web", maxChars: 512000, gate: make(chan struct{}, 1), sessions: map[string]*session{}}
}

// A distinct client request supersedes an uncertain remote branch. It must
// start fresh without trying to recover a response the client never received.
func TestBestEffortDirtyContinuationWithoutReadback(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			old := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "old unanswered request"}}}
			next := old
			next.Messages = append(append([]message(nil), old.Messages...), message{Role: "user", Content: "new request must continue"})
			f := &resyncBackend{fakeBackend: &fakeBackend{}, mismatch: structuralMismatch()}
			h := testHandler(f.fakeBackend)
			h.client = f
			s := &session{Dirty: true, Phase: "final", URL: "https://chatgpt.com/c/uncertain", PendingRequest: &old, PendingPrompt: "old prompt"}
			if committed {
				s.Messages = []message{{Role: "user", Content: "committed question"}, {Role: "assistant", Content: "committed answer"}}
				next.Messages = append(append([]message(nil), s.Messages...), next.Messages...)
			}
			h.sessions["continuation"] = s
			w := request(h, "continuation", next)
			if f.reads != 0 || f.latests != 0 {
				t.Fatalf("new request performed mandatory readback: exact=%d latest=%d", f.reads, f.latests)
			}
			if w.Code != 200 {
				t.Fatalf("new request did not continue: status=%d", w.Code)
			}
			if len(f.urls) == 0 || f.urls[0] != "" {
				t.Fatalf("uncertain remote thread reused: submissions=%d", len(f.urls))
			}
			if !strings.Contains(strings.Join(f.prompts, "\n"), "new request must continue") {
				t.Fatal("new user message was lost")
			}
		})
	}
}

func TestBestEffortIdenticalUncertainRetryNeverSubmits(t *testing.T) {
	old := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "uncertain request"}}}
	f := &resyncBackend{fakeBackend: &fakeBackend{}, mismatch: structuralMismatch()}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.sessions["retry"] = &session{Dirty: true, Phase: "final", URL: "https://chatgpt.com/c/uncertain", PendingRequest: &old, PendingPrompt: "original prompt"}
	// Changing only the model cannot make an uncertain submission safe to send.
	retry := old
	retry.Model = "another-model"
	w := request(h, "retry", retry)
	if len(f.prompts) != 0 || w.Code == 200 {
		t.Fatalf("identical uncertain retry submitted: count=%d status=%d", len(f.prompts), w.Code)
	}
	if f.reads != 0 || f.latests != 0 {
		t.Fatalf("ordinary retry performed implicit recovery: exact=%d latest=%d", f.reads, f.latests)
	}
}
func request(h *handler, id string, req completionsRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(b))
	if id != "" {
		r.Header.Set("X-Session-Id", id)
	}
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}
func weatherReq() completionsRequest {
	return completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "Call weather for Hanoi"}}, Tools: []toolDef{{Type: "function", Function: toolFunction{Name: "get_weather", Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false}}}}, ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}
}

const weatherAnswer = "<opencode_bridge_call>\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n</opencode_bridge_call>"

func assistant(t *testing.T, w *httptest.ResponseRecorder) message {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %.100s", w.Code, w.Body.String())
	}
	var v struct {
		Choices []struct {
			Message      message
			FinishReason string `json:"finish_reason"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Choices) != 1 {
		t.Fatal(w.Body.String())
	}
	return v.Choices[0].Message
}
func TestSingleUserToolsRoundtripPersistenceRetry(t *testing.T) {
	args := `{"city": "printf '%s\\n' 'a\\\\b_*'"}`
	answer := `<opencode\_bridge\_call>{"name":"get_weather","arguments":` + args + `}</opencode\_bridge\_call>`
	f := &fakeBackend{answers: []string{answer, "Hanoi: 25 C"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	a := assistant(t, request(h, "session-a", req))
	if len(a.ToolCalls) != 1 || a.ToolCalls[0].Function.Name != "get_weather" || a.ToolCalls[0].Function.Arguments != args || contentText(a) != "" {
		t.Fatal(a)
	}
	if strings.Count(f.prompts[0], "Call weather for Hanoi") != 1 {
		t.Fatal("single user duplicated")
	}
	dup := assistant(t, request(h, "session-a", req))
	if dup.ToolCalls[0].ID != a.ToolCalls[0].ID || len(f.prompts) != 1 {
		t.Fatal("retry regenerated/submitted")
	}
	// Reload disk state before continuing, as launchd restart would.
	h2 := testHandler(f)
	h2.statePath = h.statePath
	if err := h2.load(); err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "25 C"})
	req.ToolChoice = "none"
	got := assistant(t, request(h2, "session-a", req))
	if contentText(got) != "Hanoi: 25 C" {
		t.Fatal(got)
	}
	if f.urls[1] != "https://chatgpt.com/c/thread-1" || strings.Contains(f.prompts[1], "Call weather for Hanoi") || !strings.Contains(f.prompts[1], "25 C") {
		t.Fatal(f.prompts, f.urls)
	}
}
func TestSSEContract(t *testing.T) {
	args := `{"city": "printf '%s\\n' 'a\\\\b_*'"}`
	answer := `<opencode\_bridge\_call>{"name":"get_weather","arguments":` + args + `}</opencode\_bridge\_call>`
	f := &fakeBackend{answers: []string{answer}}
	h := testHandler(f)
	req := weatherReq()
	req.Stream = true
	w := request(h, "sse", req)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal(w)
	}
	if strings.Contains(w.Body.String(), "opencode_bridge_call") || strings.Contains(w.Body.String(), "```") || strings.Contains(w.Body.String(), "~~~") {
		t.Fatal("protocol leak")
	}
	var calls, stops int
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var v struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    *int
						ID       string
						Function callFunction
					} `json:"tool_calls"`
				}
				Finish string `json:"finish_reason"`
			}
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatal(err)
		}
		for _, c := range v.Choices {
			for _, call := range c.Delta.ToolCalls {
				if call.Index == nil || *call.Index != 0 || call.ID == "" || call.Function.Arguments != args {
					t.Fatal(call)
				}
				calls++
			}
			if c.Finish == "tool_calls" {
				stops++
			}
		}
	}
	if calls != 1 || stops != 1 || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatal(w.Body.String())
	}
}
func TestMalformedCalls(t *testing.T) {
	// Captured ec597 source encloses a complete valid protocol in escape tags.
	// Remove the empty display envelope only after all call validation succeeds.
	plain, plainErr := parseAnswer(weatherAnswer, weatherReq())
	wrapped, wrappedErr := parseAnswer("<escape>"+weatherAnswer+"</escape>", weatherReq())
	if plainErr != nil || wrappedErr != nil || len(wrapped.Calls) != 1 || wrapped.Text != "" || wrapped.Calls[0].Function.Arguments != plain.Calls[0].Function.Arguments {
		t.Fatal("bound escape envelope changed executable arguments or leaked as text", plainErr, wrappedErr)
	}
	for _, answer := range []string{"<opencode_bridge_call>{bad}</opencode_bridge_call>", "<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":\"bad\"}</opencode_bridge_call>", "<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":null}</opencode_bridge_call>", "<opencode_bridge_call>{\"name\":\"unknown\",\"arguments\":{}}</opencode_bridge_call>", "<opencode_bridge_call>{}", "plain answer", "```tool_call\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n```", "```\n" + weatherAnswer + "\n```", "~~~\n" + weatherAnswer + "\n~~~", "Example `" + weatherAnswer + "`"} {
		t.Run(answer, func(t *testing.T) {
			req := weatherReq()
			if answer == "<opencode_bridge_call>{}" {
				answer = `<opencode\_bridge\_call>{}`
				req.ToolChoice = "auto" // must fail even without required choice
			}
			if _, err := parseAnswer(answer, req); err == nil {
				t.Fatal("accepted invalid call")
			}
		})
	}
	for _, args := range []string{`{}`, `{"city":12}`, `{"city":"Hanoi","extra":true}`, `[]`} {
		answer := "<opencode_bridge_call>\n{\"name\":\"get_weather\",\"arguments\":" + args + "}\n</opencode_bridge_call>"
		if _, err := parseAnswer(answer, weatherReq()); err == nil {
			t.Fatalf("invalid schema arguments accepted: %s", args)
		}
	}
	req := weatherReq()
	req.ToolChoice = "none"
	if _, err := parseAnswer(weatherAnswer, req); err == nil {
		t.Fatal("none honored incorrectly")
	}
	if _, err := parseAnswer(weatherAnswer, weatherReq()); err != nil {
		t.Fatal(err)
	}
	t.Run("commentary is preserved only after every call validates", func(t *testing.T) {
		out, err := parseAnswer("I will check.\n"+weatherAnswer+"\nWaiting for results.", weatherReq())
		if err != nil || len(out.Calls) != 1 || out.Text != "I will check.\n\nWaiting for results." {
			t.Fatal("valid calls or non-executable commentary lost", err)
		}
		for _, unsafe := range []string{
			"I will check.\n" + weatherAnswer + "\n<opencode_bridge_call>{",
			"I will check.\n" + weatherAnswer + "\n<opencode_bridge_call>{\"name\":\"unknown\",\"arguments\":{}}</opencode_bridge_call>",
			"I will check.\n" + weatherAnswer + "\n<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":{\"city\":12}}</opencode_bridge_call>",
		} {
			out, err := parseAnswer(unsafe, weatherReq())
			if err == nil || len(out.Calls) != 0 {
				t.Fatal("partially validated mixed output delivered")
			}
		}
	})
	// A completely received invalid response delivers no partial calls, is
	// durable across restart, and exact retries do not submit or execute again.
	bad := "<opencode_bridge_call>{bad}</opencode_bridge_call>"
	// The first submission fails validation, so exactly one repair turn
	// follows in the same thread; when that also fails the output is
	// durably rejected. Both repair-relevant answers are malformed here.
	f := &fakeBackend{answers: []string{bad, bad, "ACK", "continued"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	rejected := weatherReq()
	if w := request(h, "invalid", rejected); w.Code != 422 || h.sessions["invalid"].Dirty || h.sessions["invalid"].Phase != "invalid" || len(h.sessions["invalid"].Result.Calls) != 0 {
		t.Fatal("finished malformed output treated as unknown/delivered", w.Code)
	}
	loaded := testHandler(f)
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	if w := request(loaded, "invalid", rejected); w.Code != 422 || len(f.prompts) != 2 {
		t.Fatal("invalid retry resubmitted", w.Code)
	}
	t.Run("format repair re-asks once in the same thread", func(t *testing.T) {
		f := &fakeBackend{answers: []string{bad, weatherAnswer}}
		h := testHandler(f)
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		if w := request(h, "repair-ok", weatherReq()); w.Code != 200 || len(f.prompts) != 2 {
			t.Fatal("repair not attempted exactly once", w.Code, len(f.prompts))
		}
		if f.urls[1] == "" || f.urls[1] != h.sessions["repair-ok"].URL {
			t.Fatal("repair left the thread", f.urls, h.sessions["repair-ok"].URL)
		}
		if len(h.sessions["repair-ok"].Result.Calls) != 1 {
			t.Fatal("repaired call not delivered")
		}
	})
	t.Run("failed repair is durable rejected output", func(t *testing.T) {
		f := &fakeBackend{answers: []string{bad, bad}}
		h := testHandler(f)
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		if w := request(h, "repair-bad", weatherReq()); w.Code != 422 || h.sessions["repair-bad"].Phase != "invalid" || len(f.prompts) != 2 {
			t.Fatal("failed repair not rejected after exactly one attempt", w.Code, len(f.prompts))
		}
		if w := request(h, "repair-bad", weatherReq()); w.Code != 422 || len(f.prompts) != 2 {
			t.Fatal("retry resubmitted", w.Code, len(f.prompts))
		}
	})
	t.Run("uncertain repair survives restart and adopts only its exact prompt", func(t *testing.T) {
		for _, repairErr := range []error{context.Canceled, errors.New("transport lost after submission")} {
			f := &fakeBackend{answers: []string{bad}, err: repairErr, failAt: 2, errorURL: "https://chatgpt.com/c/repair"}
			h := testHandler(f)
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			req := weatherReq()
			if w := request(h, "repair-uncertain", req); w.Code != 422 {
				t.Fatal(w.Code)
			}
			s := h.sessions["repair-uncertain"]
			if !s.Dirty || s.Phase != "final" || s.RepairFingerprint != requestFingerprint(req) || s.PendingPrompt != f.prompts[1] || s.RepairPendingPrompt != s.PendingPrompt || requestFingerprint(*s.PendingRequest) != requestFingerprint(req) {
				t.Fatal("lost uncertain repair binding/budget")
			}
			loaded := testHandler(f)
			loaded.statePath = h.statePath
			if err := loaded.load(); err != nil {
				t.Fatal(err)
			}
			if w := request(loaded, "repair-uncertain", req); w.Code != 422 || len(f.prompts) != 2 {
				t.Fatal("restart resubmitted", w.Code)
			}
			reader := &inspectBackend{fakeBackend: f, answer: weatherAnswer, expected: s.PendingPrompt}
			loaded.client = reader
			if w := recoveryRequest(loaded, &req, "repair-uncertain"); w.Code != 200 {
				t.Fatal("explicit repair recovery failed", w.Code)
			}
			a := assistant(t, request(loaded, "repair-uncertain", req))
			dup := assistant(t, request(loaded, "repair-uncertain", req))
			if reader.reads != 1 || len(f.prompts) != 2 || a.ToolCalls[0].ID != dup.ToolCalls[0].ID || loaded.sessions["repair-uncertain"].RepairFingerprint != requestFingerprint(req) {
				t.Fatal("adoption changed IDs/budget or resubmitted")
			}
		}
	})
	t.Run("proved unsent repair rejects original and consumes budget", func(t *testing.T) {
		f := &fakeBackend{answers: []string{bad}, err: chatgpt.ErrNotSubmitted, failAt: 2}
		h := testHandler(f)
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		req := weatherReq()
		if w := request(h, "repair-unsent", req); w.Code != 422 {
			t.Fatal(w.Code)
		}
		s := h.sessions["repair-unsent"]
		if s.Dirty || s.Phase != "invalid" || s.PendingPrompt != f.prompts[0] || s.RepairPendingPrompt != "" || s.RepairFingerprint != requestFingerprint(req) {
			t.Fatal("unsent repair misclassified")
		}
		if w := request(h, "repair-unsent", req); w.Code != 422 || len(f.prompts) != 2 {
			t.Fatal("repair retried")
		}
	})
	t.Run("repair journal failure never submits or rejects uncertain binding", func(t *testing.T) {
		f := &fakeBackend{}
		h := testHandler(f)
		req := weatherReq()
		s := &session{Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "original", URL: "https://chatgpt.com/c/original"}
		h.sessions["store-fail"] = s
		h.statePath = filepath.Join(t.TempDir(), "missing", "state.json")
		// A file as parent makes atomic persistence impossible.
		if err := os.WriteFile(filepath.Dir(h.statePath), []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		_, state := h.requestFormatRepair(httptest.NewRequest("POST", "/", nil), "store-fail", "test", s.URL, req, bad, false)
		if state != repairStoreFailed || len(f.prompts) != 0 || !s.Dirty || s.PendingPrompt != "original" || s.RepairPendingPrompt != "" {
			t.Fatal("journal failure submitted/rejected or lost binding")
		}
	})
	t.Run("post submission repair store failure retains durable budget and prompt", func(t *testing.T) {
		for _, outcome := range []string{"completed", "uncertain"} {
			t.Run(outcome, func(t *testing.T) {
				f := &fakeBackend{}
				h := testHandler(f)
				req := weatherReq()
				h.statePath = filepath.Join(t.TempDir(), "state.json")
				journal := h.statePath
				s := &session{Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "original", URL: "https://chatgpt.com/c/original"}
				h.sessions["post-store"] = s
				blocked := filepath.Join(t.TempDir(), "parent-file")
				if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
				calls := 0
				f.turn = func(ctx context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
					calls++
					if opt.URL != s.URL || opt.Prompt != s.RepairPendingPrompt {
						t.Fatal("repair unbound")
					}
					h.statePath = filepath.Join(blocked, "state.json")
					res := chatgpt.TurnResult{Answer: weatherAnswer, URL: opt.URL}
					if outcome == "uncertain" {
						return res, context.Canceled
					}
					return res, nil
				}
				_, state := h.requestFormatRepair(httptest.NewRequest("POST", "/", nil), "post-store", "test", s.URL, req, bad, false)
				if (outcome == "completed" && state != repairStoreFailed) || (outcome == "uncertain" && state != repairUncertain) || calls != 1 || !s.Dirty || s.PendingPrompt == "original" || s.PendingPrompt != s.RepairPendingPrompt {
					t.Fatal("post-submit uncertainty erased", state)
				}
				loaded := testHandler(f)
				loaded.statePath = journal
				if err := loaded.load(); err != nil {
					t.Fatal(err)
				}
				persisted := loaded.sessions["post-store"]
				if !persisted.Dirty || persisted.RepairFingerprint != requestFingerprint(req) || persisted.PendingPrompt != s.PendingPrompt {
					t.Fatal("budget/binding not durable")
				}
				_, retryState := loaded.requestFormatRepair(httptest.NewRequest("POST", "/", nil), "post-store", "test", s.URL, req, bad, false)
				if retryState != repairSkipped || calls != 1 {
					t.Fatal("duplicate repair after restart")
				}
			})
		}
	})
	t.Run("rotation repair same thread success and cancellation", func(t *testing.T) {
		for _, cancelRepair := range []bool{false, true} {
			f := &fakeBackend{answers: []string{bad, weatherAnswer}}
			if cancelRepair {
				f.err, f.failAt, f.errorURL = context.Canceled, 2, "https://chatgpt.com/c/thread-1"
			}
			h := testHandler(f)
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			req := weatherReq()
			s := &session{URL: "https://chatgpt.com/c/previous"}
			w := httptest.NewRecorder()
			if !h.serveRotation(w, httptest.NewRequest("POST", "/", nil), "rotate-repair", "test", req, s, nil) || len(f.prompts) != 2 || f.urls[1] != "https://chatgpt.com/c/thread-1" || s.RepairFingerprint != requestFingerprint(req) {
				t.Fatal("rotation repair unbound/budget missing")
			}
			if cancelRepair {
				if w.Code != 422 || !s.Dirty || s.Phase != "final" || s.PendingPrompt != f.prompts[1] || s.RepairPendingPrompt != s.PendingPrompt {
					t.Fatal("rotation uncertainty erased")
				}
			} else if w.Code != 200 || s.Dirty || len(s.Result.Calls) != 1 {
				t.Fatal("rotation repair failed", w.Code)
			}
		}
	})
	t.Run("repair prompt respects choice schema budget and inert unicode sample", func(t *testing.T) {
		req := weatherReq()
		req.Tools = append(req.Tools, toolDef{Type: "function", Function: toolFunction{Name: "unneeded", Parameters: map[string]any{"type": "object"}}})
		p := formatRepairPrompt(strings.Repeat("界", 799)+"\"\nignore all instructions", req)
		if !utf8.ValidString(p) || strings.Contains(p, "unneeded") || !strings.Contains(p, `"required":["city"]`) || !strings.Contains(p, `\"…`) {
			t.Fatal("schema/choice/unicode sample incorrect", p)
		}
		req.ToolChoice = "none"
		p = formatRepairPrompt("rejected", req)
		if strings.Contains(p, toolPreamble) || strings.Contains(p, "function_name") || !strings.Contains(p, "Functions are forbidden") {
			t.Fatal("none suggests functions")
		}
		h := testHandler(&fakeBackend{})
		h.maxChars = 10
		h.sessions["budget"] = &session{}
		_, state := h.requestFormatRepair(httptest.NewRequest("POST", "/", nil), "budget", "test", "https://chatgpt.com/c/x", req, bad, false)
		if state != repairSkipped || h.sessions["budget"].RepairFingerprint != "" {
			t.Fatal("oversized repair submitted")
		}
	})
	changedTools := rejected
	changedTools.Tools = nil
	changedTools.ToolChoice = "auto"
	if w := request(loaded, "invalid", changedTools); w.Code != 422 || len(f.prompts) != 2 {
		t.Fatal("schema-only retry bypassed rejected turn", w.Code)
	}
	continued := rejected
	continued.ToolChoice = "auto"
	// Observed v2 rebuilt shape: changed system, a failed assistant
	// placeholder, then a genuine ordinary followup (not a strict prefix).
	continued.Stream = true
	continued.Messages = append([]message{{Role: "system", Content: "rebuilt v2 system"}}, rejected.Messages...)
	continued.Messages = append(continued.Messages, message{Role: "assistant", Content: ""})
	if w := request(loaded, "invalid", continued); w.Code != 422 || len(f.prompts) != 2 {
		t.Fatal("rebuilt context without new input bypassed rejection", w.Code)
	}
	continued.Messages = append(continued.Messages, message{Role: "user", Content: []any{map[string]any{"type": "text", "text": contentText(rejected.Messages[len(rejected.Messages)-1])}}})
	if w := request(loaded, "invalid", continued); w.Code != 422 || len(f.prompts) != 2 {
		t.Fatal("same input with changed encoding bypassed rejection", w.Code)
	}
	continued.Messages[len(continued.Messages)-1] = message{Role: "user", Content: "<system-update>new time and context metadata</system-update>"}
	if w := request(loaded, "invalid", continued); w.Code != 422 || len(f.prompts) != 2 {
		t.Fatal("volatile system-update masqueraded as new input", w.Code)
	}
	continued.Messages[len(continued.Messages)-1] = message{Role: "user", Content: "using loopd dude"}
	toolOnly := continued
	toolOnly.Messages = append(append([]message{}, continued.Messages...), message{Role: "tool", ToolCallID: "undelivered", Content: "result"})
	if w := request(loaded, "invalid", toolOnly); w.Code != 422 || len(f.prompts) != 2 || strings.Contains(w.Body.String(), "data:") {
		t.Fatal("tool continuation emitted calls/SSE or bypassed rejection", w.Code)
	}
	toolOnly.Messages = append(append([]message{}, rejected.Messages...), message{Role: "tool", ToolCallID: "undelivered", Content: "result"}, message{Role: "user", Content: "using loopd dude"})
	if w := request(loaded, "invalid", toolOnly); w.Code != 422 || len(f.prompts) != 2 {
		t.Fatal("new user laundered an undelivered tool result", w.Code)
	}
	blocked := *loaded.sessions["invalid"]
	if w := request(loaded, "invalid", continued); w.Code != 200 || loaded.sessions["invalid"].Dirty || loaded.sessions["invalid"].Phase != "" {
		t.Fatal("new ordinary user turn failed", w.Code, w.Body.String())
	}
	if f.urls[2] != "" {
		t.Fatal("new turn reused rejected thread")
	}
	n := len(f.prompts)
	if w := request(loaded, "invalid", rejected); w.Code != 422 || len(f.prompts) != n {
		t.Fatal("old rejected fingerprint lost after continuation", w.Code)
	}
	for _, messages := range [][]message{
		append(append([]message{}, rejected.Messages...), message{Role: "user", Content: "using loopd dude"}),
		{{Role: "system", Content: "compacted authoritative context"}, {Role: "user", Content: []any{map[string]any{"type": "text", "text": "<system-update>current metadata</system-update>using loopd dude"}}}},
	} {
		state := blocked
		loaded.sessions["invalid"] = &state
		f.prompts, f.urls, f.answers = nil, nil, []string{"continued"}
		continued.Messages = messages
		if w := request(loaded, "invalid", continued); w.Code != 200 || loaded.sessions["invalid"].Dirty || loaded.sessions["invalid"].Phase != "" || len(f.prompts) != 1 || f.urls[0] != "" || !strings.Contains(f.prompts[0], toolPrompt(continued)) {
			t.Fatal("strict/compacted new user failed fresh full-schema continuation", w.Code)
		}
		if w := request(loaded, "invalid", rejected); w.Code != 422 || len(f.prompts) != 1 {
			t.Fatal("variant lost rejected fingerprint", w.Code)
		}
	}
	// Authentic installed v2 envelope (mock_identity.py): child identity is in
	// x-opencode-session-id; every legacy/affinity header still names the parent.
	// Observed rejected child roles: system,user,assistant,6 tools,assistant,5
	// tools. The parent then continues its already-delivered subagent tool call.
	f = &fakeBackend{answers: []string{weatherAnswer}}
	h = testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	parent := weatherReq()
	a := assistant(t, request(h, "parent", parent))
	before, _ := json.Marshal(h.sessions["parent"])
	child := weatherReq()
	child.Messages = []message{{Role: "system", Content: "General subagent system"}, {Role: "user", Content: "Harmless distinct child task"}}
	for _, count := range []int{6, 5} {
		calls := []toolCall{}
		for i := 0; i < count; i++ {
			calls = append(calls, toolCall{ID: fmt.Sprintf("delivered-%d-%d", count, i), Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Hanoi"}`}})
		}
		child.Messages = append(child.Messages, message{Role: "assistant", ToolCalls: calls})
		for _, call := range calls {
			child.Messages = append(child.Messages, message{Role: "tool", ToolCallID: call.ID, Content: "prior delivered result"})
		}
	}
	// Mixed commentary alone is valid now. Retain the rejected child boundary
	// using the actual serializer failure class: illegal Markdown JSON escapes.
	badChild := "redacted source commentary\n" + strings.ReplaceAll(weatherAnswer, "Hanoi", `\*Hanoi\*`)
	f.answers = append(f.answers, "redacted source commentary\n"+strings.ReplaceAll(weatherAnswer, "Hanoi", `\*Hanoi\*`))
	f.answers = append(f.answers, badChild)
	for attempt := 0; attempt < 2; attempt++ {
		body, _ := json.Marshal(child)
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		for _, name := range []string{"X-Session-Id", "x-opencode-session", "x-session-affinity", "x-parent-session-id", "x-opencode-parent-session-id"} {
			r.Header.Set(name, "parent")
		}
		r.Header.Set("x-opencode-session-id", "child")
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		if w.Code != 422 || len(f.prompts) != 3 || h.sessions["child"].Phase != "invalid" || len(h.sessions["child"].Result.Calls) != 0 {
			t.Fatal("child rejection/retry escaped identity boundary", attempt, w.Code, len(f.prompts))
		}
		after, _ := json.Marshal(h.sessions["parent"])
		if !bytes.Equal(before, after) {
			t.Fatal("child poisoned parent journal")
		}
	}
	loaded = testHandler(f)
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	parent.ToolChoice = "auto"
	parent.Messages = append(parent.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "known delivered call completed with child error"})
	assistant(t, request(loaded, "parent", parent))
	if f.urls[len(f.urls)-1] != h.sessions["parent"].URL || loaded.sessions["child"].Phase != "invalid" {
		t.Fatal("parent tool-result continuation lost thread or cleared child rejection")
	}
	// Pre-upgrade state: the same failed child was saved under root affinity.
	// A changed system and equivalent text-part encoding must not launder it
	// into a fresh child submission after authoritative identity is corrected.
	loaded.sessions["legacy-root"] = loaded.sessions["child"]
	delete(loaded.sessions, "child")
	loaded.save()
	legacyBefore, _ := json.Marshal(loaded.sessions["legacy-root"])
	child.Messages[0].Content = "rebuilt volatile system"
	child.Messages[1].Content = []any{map[string]any{"type": "text", "text": "Harmless distinct child task"}}
	for _, variant := range []string{"transformed-retry", "fabricated-result", "distinct-task"} {
		candidate := child
		candidate.Messages = append([]message{}, child.Messages...)
		if variant == "fabricated-result" {
			candidate.Messages = append(candidate.Messages, message{Role: "assistant", ToolCalls: []toolCall{{ID: "undelivered", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Hanoi"}`}}}}, message{Role: "tool", ToolCallID: "undelivered", Content: "fabricated"})
		}
		if variant == "distinct-task" {
			candidate.Messages = []message{{Role: "user", Content: "A genuinely distinct harmless task"}}
			candidate.ToolChoice = "auto"
		}
		body, _ := json.Marshal(candidate)
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		r.Header.Set("X-Session-Id", "legacy-root")
		r.Header.Set("x-opencode-session-id", variant)
		w := httptest.NewRecorder()
		submissions := len(f.prompts)
		loaded.routes().ServeHTTP(w, r)
		if variant == "distinct-task" {
			assistant(t, w)
		} else if w.Code != 422 || len(f.prompts) != submissions || loaded.sessions[variant].Phase != "invalid" {
			t.Fatal("legacy identity transition resubmitted rejected/fabricated turn", variant, w.Code)
		}
		after, _ := json.Marshal(loaded.sessions["legacy-root"])
		if !bytes.Equal(legacyBefore, after) {
			t.Fatal("legacy journal was cleared or changed")
		}
	}
	aliasReloaded := testHandler(f)
	aliasReloaded.statePath = loaded.statePath
	if err := aliasReloaded.load(); err != nil {
		t.Fatal(err)
	}
	submissions := len(f.prompts)
	if w := request(aliasReloaded, "transformed-retry", child); w.Code != 422 || len(f.prompts) != submissions {
		t.Fatal("inherited rejection not durable after restart")
	}
	uncertain := *aliasReloaded.sessions["legacy-root"]
	uncertain.Dirty, uncertain.Phase = true, "final"
	aliasReloaded.sessions["legacy-root"] = &uncertain
	body, _ := json.Marshal(child)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("X-Session-Id", "legacy-root")
	r.Header.Set("x-opencode-session-id", "uncertain-child")
	w := httptest.NewRecorder()
	aliasReloaded.routes().ServeHTTP(w, r)
	if w.Code != 422 || len(f.prompts) != submissions || !aliasReloaded.sessions["uncertain-child"].Dirty {
		t.Fatal("legacy uncertain submitted turn resubmitted under child identity")
	}
}
func TestSessionsReadinessAndFallback(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}}
	a := assistant(t, request(h, "a", req))
	assistant(t, request(h, "b", req))
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 || f.probe != 1 || len(f.prompts) != 2 {
		t.Fatal(w)
	}
	req.Messages = append(req.Messages, a, message{Role: "user", Content: "followup"})
	assistant(t, request(h, "a", req))
	if f.urls[2] != h.sessions["a"].URL || h.sessions["a"].URL == h.sessions["b"].URL {
		t.Fatal("sessions crossed")
	}
	request(h, "", req)
	request(h, "", req)
	if f.urls[3] != "" || f.urls[4] != "" {
		t.Fatal("fallback reused a session")
	}
}
func TestErrorsAndDirtyRetry(t *testing.T) {
	if fixture := os.Getenv("CHATGPT_BRIDGE_REJECTED_FIXTURE"); fixture != "" {
		t.Run("exact recorded rejected answer offline", func(t *testing.T) {
			b, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			var captured struct {
				Request        completionsRequest
				Answer, Source string
			}
			if err := json.Unmarshal(b, &captured); err != nil {
				t.Fatal(err)
			}
			for label, answer := range map[string]string{"rendered": captured.Answer, "source": captured.Source} {
				_, err := parseAnswer(answer, captured.Request)
				if label == "rendered" {
					if err == nil || !strings.Contains(err.Error(), "invalid escape sequence") {
						t.Fatalf("rendered failure classification: %v", err)
					}
					t.Log("rendered: Markdown serialization introduced an invalid JSON escape")
					continue
				}
				out, err := parseAnswer(answer, captured.Request)
				if err != nil || len(out.Calls) != 1 || out.Text == "" {
					t.Fatalf("%s source call/commentary: %v", label, err)
				}
				blocks := toolBlock.FindAllString(answer, -1)
				if len(blocks) != 1 {
					t.Fatal("expected exact single recorded wrapper")
				}
				wrapper, err := parseAnswer(blocks[0], captured.Request)
				if err != nil || len(wrapper.Calls) != 1 || wrapper.Calls[0].Function.Name != "patch" || wrapper.Calls[0].Function.Arguments != out.Calls[0].Function.Arguments {
					t.Fatalf("%s wrapper schema/tool_choice: %v", label, err)
				}
				t.Logf("%s: complete patch wrapper validates against exact catalog; outer text retained separately and exact arguments preserved", label)
			}
		})
	}
	t.Run("valid tool then authoritative empty follow-up", func(t *testing.T) {
		f := &inspectBackend{fakeBackend: &fakeBackend{answers: []string{weatherAnswer}}}
		h := testHandler(f.fakeBackend)
		h.client = f
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		req := weatherReq()
		a := assistant(t, request(h, "post-tool-empty", req))
		if len(a.ToolCalls) != 1 {
			t.Fatal("valid prior call not delivered")
		}
		req.ToolChoice, req.Stream = "auto", true
		req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "already completed result"})
		turns1, err1 := h.buildTurns(req, req.Messages[len(req.Messages)-1:], toolReminder(req))
		if err1 != nil {
			t.Fatalf("buildTurns: %v", err1)
		}
		f.expected = turns1[0].prompt
		f.err, f.errorURL, f.readErr = chatgpt.ErrMissingAnswer, h.sessions["post-tool-empty"].URL, chatgpt.ErrCompletedEmpty
		w := request(h, "post-tool-empty", req)
		s := h.sessions["post-tool-empty"]
		if w.Code != 422 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") || s.Dirty || s.Phase != "invalid" || f.reads != 1 || len(f.prompts) != 2 || len(s.Result.Calls) != 0 || s.PendingRequest.Messages[len(req.Messages)-1].ToolCallID != a.ToolCalls[0].ID {
			t.Fatal("first empty follow-up not settled safely", w.Code, w.Body.String())
		}
		if retry := request(h, "post-tool-empty", req); retry.Code != 422 || f.reads != 1 || len(f.prompts) != 2 {
			t.Fatal("empty follow-up repeated")
		}
	})
	for _, failure := range []error{chatgpt.ErrMissingAnswer, fmt.Errorf("poll: %w", chatgpt.ErrMissingAnswer)} {
		f := &fakeBackend{err: failure, errorURL: "https://chatgpt.com/c/empty-renderer"}
		h := testHandler(f)
		req := weatherReq()
		req.Stream = true
		w := request(h, "missing", req)
		s := h.sessions["missing"]
		if w.Code != 422 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") || strings.Contains(w.Body.String(), "data:") || !s.Dirty || s.Phase != "final" || s.PendingRequest == nil || s.PendingPrompt == "" || s.URL != f.errorURL {
			t.Fatal("missing answer must be terminal before SSE, retaining exact recovery journal", w, s)
		}
		if request(h, "missing", req).Code != 422 || len(f.prompts) != 1 {
			t.Fatal("missing answer resubmitted on retry")
		}
	}
	f := &fakeBackend{err: errors.New("composer not found")}
	h := testHandler(f)
	req := weatherReq()
	req.Stream = true
	w := request(h, "failed", req)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "proxy_error") {
		t.Fatal(w)
	}
	if request(h, "failed", req).Code != 422 || len(f.prompts) != 1 {
		t.Fatal("uncertain turn retried")
	}
	if request(h, "empty", completionsRequest{}).Code != 400 {
		t.Fatal("empty accepted")
	}
	ready := httptest.NewRecorder()
	h.routes().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != 503 {
		t.Fatal("readiness backend failure must be 503", ready)
	}
	f.err = nil
	for _, tc := range []struct {
		url     string
		err     error
		handled bool
	}{
		{"https://chatgpt.com/c/reached", context.Canceled, true},
		{"", context.Canceled, true},
		{"", errors.New("navigate: page load error net::ERR_FAILED"), false},
		{"", errors.Join(chatgpt.ErrProjectSetup, errors.New("Project URL not observed")), false},
	} {
		f := &fakeBackend{err: tc.err, errorURL: tc.url}
		h := testHandler(f)
		s := &session{URL: "https://chatgpt.com/c/old"}
		w := httptest.NewRecorder()
		if handled := h.serveRotation(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), "rotate", "main", weatherReq(), s, nil); handled != tc.handled || len(f.prompts) != 1 {
			t.Fatal("rotation retried uncertain turn", handled)
		}
		if tc.handled && (!s.Dirty || s.Phase != "final" || s.URL != tc.url || w.Code != 502) {
			t.Fatal("rotation lost uncertain target", s, w.Code)
		}
		if !tc.handled && (s.Dirty || s.URL != "https://chatgpt.com/c/old") {
			t.Fatal("pre-submit fallback lost snapshot")
		}
	}
}

func TestNavigateFailureDoesNotWedgeFreshSession(t *testing.T) {
	t.Run("first journal binds crash before backend completion", func(t *testing.T) {
		t.Setenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION", "0")
		for _, replay := range []bool{false, true} {
			f := &fakeBackend{}
			h := testHandler(f)
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			req := completionsRequest{Messages: []message{{Role: "user", Content: "first"}}}
			if replay {
				h.maxChars = 1800
				req.Messages = append(req.Messages, message{Role: "assistant", Content: strings.Repeat("already answered ", 300)}, message{Role: "user", Content: "new"})
			}
			f.turn = func(_ context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
				restarted := testHandler(&fakeBackend{})
				restarted.statePath = h.statePath
				if err := restarted.load(); err != nil {
					t.Fatal(err)
				}
				s := restarted.sessions["atomic"]
				if s == nil || !s.Dirty || s.incompleteJournal() {
					t.Fatal("first durable journal incomplete")
				}
				if replay {
					if s.Phase != "replay" || s.PendingRequest != nil {
						t.Fatal("replay misclassified as final")
					}
				} else if s.Phase != "final" || s.PendingPrompt != opt.Prompt || requestFingerprint(*s.PendingRequest) != requestFingerprint(req) {
					t.Fatal("first save lost source binding")
				}
				// Fail every later save, simulating interruption between submission
				// and recording its outcome. The first atomic binding must survive.
				if err := os.Mkdir(h.statePath+".tmp", 0700); err != nil {
					t.Fatal(err)
				}
				return chatgpt.TurnResult{URL: "https://chatgpt.com/c/atomic"}, context.Canceled
			}
			if w := request(h, "atomic", req); w.Code != 502 {
				t.Fatal(w.Code)
			}
			restarted := testHandler(&fakeBackend{})
			restarted.statePath = h.statePath
			if err := restarted.load(); err != nil || !restarted.sessions["atomic"].Dirty || restarted.sessions["atomic"].incompleteJournal() {
				t.Fatal("save failure lost journal", err)
			}
			if !replay {
				if w := request(restarted, "atomic", req); w.Code != 422 || len(restarted.client.(*fakeBackend).prompts) != 0 {
					t.Fatal("unknown final resubmitted", w.Code)
				}
			}
		}
	})
	if fixture := os.Getenv("CHATGPT_BRIDGE_CONTINUATION_FIXTURE"); fixture != "" {
		t.Run("recorded pending request route without browser submission", func(t *testing.T) {
			t.Setenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION", "0")
			b, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			var s session
			if err := json.Unmarshal(b, &s); err != nil || s.PendingRequest == nil {
				t.Fatal("missing recorded pending request", err)
			}
			req := *s.PendingRequest
			before, _ := json.Marshal(s.Messages)
			url := s.URL
			s.Dirty, s.Phase, s.PendingRequest, s.PendingPrompt = false, "", nil, ""
			f := &fakeBackend{err: errors.Join(chatgpt.ErrNotSubmitted, errors.New("fixture composer mismatch")), errorURL: url}
			h := testHandler(f)
			h.maxChars = 200000
			h.sessions["recorded"] = &s
			if w := request(h, "recorded", req); w.Code != 502 || s.Dirty || s.URL != url || len(f.prompts) != 1 {
				t.Fatal("recorded unsent final route remained uncertain", w.Code)
			}
			after, _ := json.Marshal(s.Messages)
			if !bytes.Equal(before, after) || strings.HasPrefix(f.prompts[0], historyIntro) {
				t.Fatal("recorded completed replay progress was lost")
			}
			f.err = nil
			f.answers = []string{"", "fixture continued"}
			w := request(h, "recorded", req)
			if w.Code != 200 || s.Dirty || s.URL != url || len(f.prompts) != 2 {
				t.Fatal("exact recorded request could not continue", w.Code)
			}
			if cached := request(h, "recorded", req); cached.Code != 200 || cached.Body.String() != w.Body.String() || len(f.prompts) != 2 {
				t.Fatal("recorded continuation did not cache stable result")
			}
		})
	}
	t.Run("pre-submit existing thread retains completed state", func(t *testing.T) {
		for _, cause := range []error{errors.New("composer does not exactly match this turn; nothing submitted"), chatgpt.ErrNoComposer, context.Canceled} {
			f := &fakeBackend{answers: []string{"first answer"}}
			h := testHandler(f)
			req := completionsRequest{Messages: []message{{Role: "user", Content: "first"}}}
			assistant(t, request(h, "existing", req))
			s := h.sessions["existing"]
			url, completed := s.URL, len(s.Messages)
			req.Messages = append(req.Messages, message{Role: "assistant", Content: "first answer"}, message{Role: "user", Content: "next"})
			f.err, f.errorURL = errors.Join(chatgpt.ErrNotSubmitted, cause), url
			wantStatus := 502
			if errors.Is(cause, chatgpt.ErrNoComposer) {
				wantStatus = 422 // blocked composer requires intervention, not retries
			}
			if w := request(h, "existing", req); w.Code != wantStatus {
				t.Fatal(w.Code, w.Body.String())
			}
			if s.Dirty || s.Phase != "" || s.PendingRequest != nil || s.PendingPrompt != "" || s.URL != url || len(s.Messages) != completed {
				t.Fatalf("pre-send failure lost completed state: %+v", s)
			}
			f.err = nil
			f.answers = []string{"first answer", "", "continued"}
			if w := request(h, "existing", req); w.Code != 200 {
				t.Fatal("continuation wedged", w.Code, w.Body.String())
			}
		}
	})
	for _, eager := range []string{"0", "1"} {
		t.Setenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION", eager)
		for _, failure := range []error{errors.New("navigate: page load error net::ERR_FAILED"), errors.Join(chatgpt.ErrProjectSetup, errors.New(`Project URL not observed for "OpenCode Bridge — explain/sesed422"`))} {
			// A navigation failure before any conversation URL was reached provably
			// submitted nothing: the session must accept a retry instead of 409.
			f := &fakeBackend{err: failure}
			h := testHandler(f)
			req := weatherReq()
			if w := request(h, "nav", req); w.Code != 502 {
				t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
			}
			if h.sessions["nav"].Dirty {
				t.Fatal("transient pre-submission failure wedged a fresh session")
			}
			f.err = nil
			n := len(f.prompts)
			if eager == "1" && n != 2 {
				t.Fatal("proved pre-submission project failure did not fall back once")
			}
			f.answers = make([]string, n+1)
			f.answers[n] = weatherAnswer
			a := assistant(t, request(h, "nav", req))
			if len(a.ToolCalls) != 1 || len(f.prompts) != n+1 {
				t.Fatal("retry did not resubmit cleanly", a, len(f.prompts))
			}
		}
	}
}

func TestHistoryDivergenceResetsToReplayAndInputValidation(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "original"}}}
	assistant(t, request(h, "a", req))
	old := h.sessions["a"].URL
	req.Messages[0].Content = "rewritten"
	got := assistant(t, request(h, "a", req))
	if got.Role != "assistant" {
		t.Fatal("divergence did not re-answer")
	}
	if h.sessions["a"].URL == old || h.sessions["a"].URL == "" {
		t.Fatalf("divergence reused old thread: %q -> %q", old, h.sessions["a"].URL)
	}
	if last := f.prompts[len(f.prompts)-1]; !strings.Contains(last, "rewritten") || f.urls[len(f.urls)-1] != "" {
		t.Fatal("full rewritten history was not replayed into a fresh thread")
	}
	for _, choice := range []any{"invalid", 42, map[string]any{"type": "function", "function": map[string]any{"name": "unknown"}}} {
		req.ToolChoice = choice
		if request(h, "validation", req).Code != 400 {
			t.Fatalf("accepted choice %v", choice)
		}
	}
	req.ToolChoice = nil
	req.Messages[0].Content = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/image.png"}}}
	if request(h, "image", req).Code != 400 {
		t.Fatal("accepted non-data attachment URL")
	}
	req.Messages[0].Content = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("pngbytes"))}}}
	w := request(h, "image2", req)
	if w.Code != 200 {
		t.Fatalf("rejected data-URL image: %d %s", w.Code, w.Body.String())
	}
	if len(f.atts) == 0 || f.atts[len(f.atts)-1] != 1 {
		t.Fatalf("image not forwarded as attachment: %v", f.atts)
	}
	lastPrompt := f.prompts[len(f.prompts)-1]
	if !strings.Contains(lastPrompt, "attached to this ChatGPT turn") || strings.Contains(lastPrompt, "pngbytes") {
		t.Fatal("image data leaked into or missing from the text prompt")
	}
}

func TestReasoningAndThinkingForwarded(t *testing.T) {
	f := &fakeBackend{reasoning: "because 2+2"}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "why"}}, ReasoningEffort: "high"}
	w := request(h, "reason", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reasoning_content":"because 2+2"`) {
		t.Fatal(w.Body.String())
	}
	if len(f.thinking) != 1 || !f.thinking[0] {
		t.Fatal("reasoning_effort did not enable Thinking", f.thinking)
	}
}

func TestMultipleCallsAndTextSSE(t *testing.T) {
	req := weatherReq()
	req.ToolChoice = "auto"
	out, err := parseAnswer(weatherAnswer+"\n"+weatherAnswer, req)
	if err != nil || len(out.Calls) != 2 || out.Calls[0].ID == out.Calls[1].ID {
		t.Fatal(out, err)
	}
	f := &fakeBackend{answers: []string{"plain reply"}}
	h := testHandler(f)
	w := request(h, "text", completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}, Stream: true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"plain reply"`) || !strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Fatal(w.Body.String())
	}
}

func TestSDKHistoryNormalization(t *testing.T) {
	f := &fakeBackend{answers: []string{weatherAnswer, "23 C"}}
	h := testHandler(f)
	req := weatherReq()
	a := assistant(t, request(h, "sdk", req))
	a.Content = nil // AI SDK serializes an assistant-only tool turn with null content.
	a.ToolCalls[0].Function.Arguments = "{\n  \"city\": \"Hanoi\"\n}"
	req.Messages[0].Content = []any{map[string]any{"type": "text", "text": "Call weather for Hanoi"}}
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "23 C"})
	req.ToolChoice = "none"
	assistant(t, request(h, "sdk", req))
	if len(f.prompts) != 2 || strings.Contains(f.prompts[1], "Call weather for Hanoi") {
		t.Fatal("SDK history representations broke the delta")
	}
}
func TestConcurrentDuplicateAndCancellation(t *testing.T) {
	t.Run("parallel transactions share the profile guard instead of self-deadlocking", func(t *testing.T) {
		entered := make(chan string, 2)
		finish := make(chan struct{})
		hook := func(_ context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
			entered <- opt.Prompt
			<-finish
			return chatgpt.TurnResult{Answer: "done", URL: "https://chatgpt.com/c/shared-guard"}, nil
		}
		h := testHandler(&fakeBackend{})
		h.profileDir = t.TempDir()
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		h.enableParallel([]backend{&fakeBackend{turn: hook}, &fakeBackend{turn: hook}})
		done := make(chan *httptest.ResponseRecorder, 2)
		go func() {
			done <- request(h, "guard-a", completionsRequest{Messages: []message{{Role: "user", Content: "GUARD_A"}}, ToolChoice: "none"})
		}()
		go func() {
			done <- request(h, "guard-b", completionsRequest{Messages: []message{{Role: "user", Content: "GUARD_B"}}, ToolChoice: "none"})
		}()
		for range 2 {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				close(finish)
				t.Fatal("parallel transaction wedged on the profile guard (SH->EX upgrade deadlock)")
			}
		}
		close(finish)
		for range 2 {
			select {
			case w := <-done:
				if w.Code != 200 {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("transaction did not complete after guard sharing")
			}
		}
	})
	t.Run("cancel one active session without canceling peer or losing uncertainty", func(t *testing.T) {
		entered := make(chan string, 2)
		finishB := make(chan struct{})
		hook := func(ctx context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
			label := "b"
			if strings.Contains(opt.Prompt, "CANCEL_A") {
				label = "a"
			}
			entered <- label
			if label == "a" {
				<-ctx.Done()
				return chatgpt.TurnResult{URL: "https://chatgpt.com/c/cancel-a"}, ctx.Err()
			}
			select {
			case <-finishB:
				return chatgpt.TurnResult{Answer: "B_COMPLETED", URL: "https://chatgpt.com/c/cancel-b"}, nil
			case <-ctx.Done():
				return chatgpt.TurnResult{}, errors.New("peer was incorrectly canceled")
			}
		}
		h := testHandler(&fakeBackend{})
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		h.enableParallel([]backend{&fakeBackend{turn: hook}, &fakeBackend{turn: hook}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reqA := completionsRequest{Messages: []message{{Role: "user", Content: "CANCEL_A"}}, ToolChoice: "none"}
		body, _ := json.Marshal(reqA)
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("X-Session-Id", "cancel-a")
		aDone := make(chan struct{})
		go func() { h.routes().ServeHTTP(httptest.NewRecorder(), r); close(aDone) }()
		bDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			bDone <- request(h, "cancel-b", completionsRequest{Messages: []message{{Role: "user", Content: "KEEP_B"}}, ToolChoice: "none"})
		}()
		for range 2 {
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				cancel()
				close(finishB)
				t.Fatal("independent sessions failed to enter backend")
			}
		}
		// Full capacity is not an unhealthy browser; readiness doesn't borrow
		// either backend or navigate either active session.
		ready := httptest.NewRecorder()
		h.routes().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
		if ready.Code != 200 || !strings.Contains(ready.Body.String(), `"active":2`) {
			cancel()
			close(finishB)
			t.Fatalf("busy pool reported unhealthy: %s", ready.Body.String())
		}
		cancel()
		select {
		case <-aDone:
		case <-time.After(3 * time.Second):
			close(finishB)
			t.Fatal("canceled session retained its tab")
		}
		select {
		case <-bDone:
			close(finishB)
			t.Fatal("cancel A finished/canceled still-blocked B")
		default:
		}
		close(finishB)
		select {
		case w := <-bDone:
			if contentText(assistant(t, w)) != "B_COMPLETED" {
				t.Fatal("wrong peer answer")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("peer did not complete independently")
		}
		restarted := testHandler(&fakeBackend{})
		restarted.statePath = h.statePath
		if err := restarted.load(); err != nil {
			t.Fatal(err)
		}
		if a := restarted.sessions["cancel-a"]; a == nil || !a.Dirty || a.Phase != "final" || a.PendingRequest == nil || a.URL != "https://chatgpt.com/c/cancel-a" {
			t.Fatalf("cancellation discarded uncertain original binding: %+v", a)
		}
		if b := restarted.sessions["cancel-b"]; b == nil || b.Dirty || b.Result.Text != "B_COMPLETED" {
			t.Fatalf("peer committed journal lost: %+v", b)
		}
	})
	t.Run("owned parallel transactions overlap and persist without lost updates", func(t *testing.T) {
		entered := make(chan string, 2)
		finish := make(chan struct{})
		var calls atomic.Int32
		clients := []backend{}
		for _, label := range []string{"worker-a", "worker-b"} {
			clients = append(clients, &fakeBackend{turn: func(ctx context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
				calls.Add(1)
				entered <- label
				select {
				case <-finish:
					return chatgpt.TurnResult{Answer: weatherAnswer, URL: "https://chatgpt.com/c/" + label}, nil
				case <-ctx.Done():
					return chatgpt.TurnResult{URL: "https://chatgpt.com/c/" + label}, ctx.Err()
				}
			}})
		}
		h := testHandler(&fakeBackend{})
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		h.enableParallel(clients)
		responses := make(chan *httptest.ResponseRecorder, 3)
		go func() { responses <- request(h, "parallel-a", weatherReq()) }()
		go func() { responses <- request(h, "parallel-b", weatherReq()) }()
		for range 2 {
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(finish)
				t.Fatal("browser work failed to overlap before either backend finished")
			}
		}
		// A duplicate queues on the session lock, not on a second browser tab.
		go func() { responses <- request(h, "parallel-a", weatherReq()) }()
		// Both tabs occupied: a third session must abandon its queue on cancel,
		// without acquiring browser work or disturbing either active transaction.
		ctx, cancel := context.WithCancel(context.Background())
		body, _ := json.Marshal(weatherReq())
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("X-Session-Id", "queued-canceled")
		cancel()
		h.routes().ServeHTTP(httptest.NewRecorder(), r)
		if calls.Load() != 2 {
			t.Fatal("same-session or canceled queued request acquired another backend")
		}
		close(finish)
		var ids []string
		for range 3 {
			select {
			case w := <-responses:
				a := assistant(t, w)
				ids = append(ids, a.ToolCalls[0].ID)
			case <-time.After(3 * time.Second):
				t.Fatal("transaction did not release its session/tab lease")
			}
		}
		if calls.Load() != 2 {
			t.Fatal("duplicate cache caused another browser generation")
		}
		restarted := testHandler(&fakeBackend{})
		restarted.statePath = h.statePath
		if err := restarted.load(); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"parallel-a", "parallel-b"} {
			s := restarted.sessions[id]
			if s == nil || s.Dirty || s.Result.ID == "" || len(s.Result.Calls) != 1 {
				t.Fatalf("lost or dirty concurrent committed session %s: %+v", id, s)
			}
			entry := restarted.lastSentFor(id)
			if entry == nil || entry.Event != "completed" || entry.URL != s.URL || entry.PromptHash == "" {
				t.Fatalf("parallel transaction lost durable sent-log after restart: %s %+v", id, entry)
			}
			if h.lastSentFor(id) == nil {
				t.Fatal("parallel owner's shared in-memory log lost", id)
			}
		}
		if len(restarted.sessions) != 2 || len(h.parallel.locks) != 0 || len(h.parallel.clients) != 2 {
			t.Fatal("canceled queue leaked journal/session lock/tab lease")
		}
		if ids[0] != ids[1] && ids[0] != ids[2] && ids[1] != ids[2] {
			t.Fatal("same-session duplicate did not retain stable tool call ID")
		}
	})

	f := &fakeBackend{answers: []string{weatherAnswer}}
	h := testHandler(f)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(h, "same", weatherReq())
			if w.Code != 200 {
				t.Errorf("status=%d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(f.prompts) != 1 {
		t.Fatal("duplicate concurrent submissions", len(f.prompts))
	}
	h.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.acquire(ctx) {
		t.Fatal("acquired canceled wait")
	}
	h.release()
}

func TestInvalidFunctionNamesBeforeSubmission(t *testing.T) {
	for _, name := range []string{"", "name with spaces", "a.b", strings.Repeat("a", 65)} {
		f := &fakeBackend{}
		h := testHandler(f)
		req := weatherReq()
		req.Tools[0].Function.Name = name
		if request(h, "invalid-name", req).Code != 400 || len(f.prompts) != 0 {
			t.Fatalf("invalid function name %q admitted", name)
		}
	}
}

func TestOrdinaryMarkdownIsNotExecutable(t *testing.T) {
	req := weatherReq()
	req.ToolChoice = "auto"
	text := "Example only:\n```tool_call\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n```"
	out, err := parseAnswer(text, req)
	if err != nil || len(out.Calls) != 0 || out.Text != text {
		t.Fatal("generic Markdown was interpreted as a bridge request", out, err)
	}
}

func TestFailedSubmittedThreadURLPersistsForRepair(t *testing.T) {
	f := &fakeBackend{err: context.Canceled, errorURL: "https://chatgpt.com/c/uncertain"}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "sessions.json")
	if request(h, "uncertain", weatherReq()).Code != 502 {
		t.Fatal("expected backend error")
	}
	h2 := testHandler(f)
	h2.statePath = h.statePath
	if err := h2.load(); err != nil {
		t.Fatal(err)
	}
	if s := h2.sessions["uncertain"]; !s.Dirty || s.URL != f.errorURL {
		t.Fatal("repair URL or dirty journal lost", s)
	}
	if request(h2, "uncertain", weatherReq()).Code != 422 || len(f.prompts) != 1 {
		t.Fatal("uncertain submitted thread retried")
	}
}

// maxPrompt asserts every submitted turn stayed within the per-chunk budget and
// returns the prompts.
func maxPrompt(t *testing.T, f *fakeBackend, max int) {
	t.Helper()
	for i, p := range f.prompts {
		if len(p) > max {
			t.Fatalf("turn %d sent %d bytes, exceeds per-chunk budget %d", i, len(p), max)
		}
	}
}

func TestFreshLongHistoryReplaysInChunkedTurns(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	// The budget must leave room for the per-turn head (tool/rendering preamble)
	// plus the chunk envelope; otherwise pieces split mid-message and the
	// codeword fragments instead of landing in the first replay chunk.
	h.maxChars = 1800
	var msgs []message
	for i := range 24 {
		msgs = append(msgs, message{Role: "user", Content: fmt.Sprintf("turn %02d %s", i, strings.Repeat("x", 90))})
	}
	msgs[0].Content = "EARLY-CODEWORD-ZEBRA " + strings.Repeat("y", 90)
	msgs = append(msgs, message{Role: "user", Content: "What was the early codeword? Reply only with it."})
	total := 0
	for _, m := range msgs {
		total += len(renderMsg(m))
	}
	if total <= h.maxChars {
		t.Fatalf("test history %d not larger than one chunk %d", total, h.maxChars)
	}
	w := request(h, "chunked", completionsRequest{Messages: msgs, ToolChoice: "none"})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.prompts) < 2 {
		t.Fatalf("long fresh history not replayed in chunks: %d turn(s)", len(f.prompts))
	}
	maxPrompt(t, f, h.maxChars)
	if !strings.Contains(f.prompts[0], "EARLY-CODEWORD-ZEBRA") {
		t.Fatal("earliest message not in the first replay chunk")
	}
	last := f.prompts[len(f.prompts)-1]
	if !strings.Contains(last, "What was the early codeword") || !strings.Contains(last, "Now produce the next assistant reply") {
		t.Fatal("final turn missing the answer instruction")
	}
	// The single-giant-paste path is gone: no prompt holds both the first and
	// the final message once chunking is required.
	for i, p := range f.prompts {
		if len(f.prompts) > 1 && strings.Contains(p, "EARLY-CODEWORD-ZEBRA") && strings.Contains(p, "What was the early codeword") {
			t.Fatalf("turn %d flattened the whole history into one paste", i)
		}
	}
	// All chunks (except none) must reuse the same thread created by the first.
	for _, u := range f.urls[1:] {
		if u == "" {
			t.Fatal("replay chunk did not continue the created thread")
		}
	}
	if h.sessions["chunked"].URL != f.urls[len(f.urls)-1] || h.sessions["chunked"].URL == "" {
		t.Fatal("final thread URL not persisted")
	}
}

func TestOversizedSingleMessageSplitToChunkBudget(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 1600
	msgs := []message{
		{Role: "user", Content: strings.Repeat("Z", 8000)},
		{Role: "user", Content: "final question"},
	}
	w := request(h, "oversized", completionsRequest{Messages: msgs, ToolChoice: "none"})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.prompts) < 2 {
		t.Fatal("oversized single message not split")
	}
	maxPrompt(t, f, h.maxChars)
}

func TestShortFreshHistoryIsOneTurn(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	w := request(h, "short", completionsRequest{Messages: []message{{Role: "user", Content: "hello there"}}, ToolChoice: "none"})
	if w.Code != 200 || len(f.prompts) != 1 {
		t.Fatalf("short history should be one turn, got %d prompts status %d", len(f.prompts), w.Code)
	}
	maxPrompt(t, f, h.maxChars)
}

func TestDeltaContinuationSendsOnlyTheDelta(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "first question"}}, ToolChoice: "none"}
	a := assistant(t, request(h, "delta", req))
	req.Messages = append(req.Messages, a, message{Role: "user", Content: "second question"})
	assistant(t, request(h, "delta", req))
	if len(f.prompts) != 2 {
		t.Fatalf("delta continuation should add exactly one turn, got %d", len(f.prompts))
	}
	if strings.Contains(f.prompts[1], "first question") || !strings.Contains(f.prompts[1], "second question") {
		t.Fatal("continuation replayed already-known history instead of the delta")
	}
}

func TestPackPiecesSplitAtRuneBoundary(t *testing.T) {
	// A limit sitting mid-rune must not corrupt the transcript.
	pieces := []piece{{text: strings.Repeat("é", 100)}} // 200 bytes, 2 bytes/runes
	groups := packPieces(pieces, 51)
	for _, g := range groups {
		for _, p := range g {
			if !utf8.ValidString(p.text) {
				t.Fatalf("split produced invalid UTF-8: %q", p.text)
			}
		}
	}
}

func TestPackPiecesRespectsBridgeWrapperBoundaries(t *testing.T) {
	// Splits must land before an opening tag or after a closing tag: a
	// wrapper split across chunks is a guaranteed tool-parse failure.
	wrap := func(name, args string) string {
		return "<opencode_bridge_call>\n{\"name\":\"" + name + "\",\"arguments\":" + args + "}\n</opencode_bridge_call>"
	}
	text := "filler intro " + strings.Repeat("a", 40) + "\n" + wrap("fn_one", `{"x":"1"}`) + "\nbetween " + strings.Repeat("b", 40) + "\n" + wrap("fn_two", `{"y":"2"}`) + "\n" + strings.Repeat("c", 60)
	groups := packPieces([]piece{{text: text}}, 100)
	var b strings.Builder
	for _, g := range groups {
		for _, p := range g {
			b.WriteString(p.text)
		}
	}
	if b.String() != text {
		t.Fatal("chunking lost or reordered bytes")
	}
	if len(groups) < 2 {
		t.Fatalf("expected splits, got %d chunk(s)", len(groups))
	}
	for i, g := range groups {
		for _, p := range g {
			if strings.Count(p.text, "<opencode_bridge_call>") != strings.Count(p.text, "</opencode_bridge_call>") {
				t.Fatalf("chunk %d splits inside a bridge wrapper", i)
			}
		}
	}
}

func TestPackPiecesOversizedSingleWrapperStillSends(t *testing.T) {
	// One wrapper larger than the whole budget cannot sit whole in any
	// chunk: it warns but still sends (split), never drops the turn.
	big := "<opencode_bridge_call>\n{\"name\":\"big\",\"arguments\":{\"blob\":\"" + strings.Repeat("z", 500) + "\"}}\n</opencode_bridge_call>"
	groups := packPieces([]piece{{text: big}}, 100)
	var b strings.Builder
	for _, g := range groups {
		for _, p := range g {
			b.WriteString(p.text)
		}
	}
	if b.String() != big {
		t.Fatal("oversized wrapper lost bytes")
	}
	if len(groups) < 2 {
		t.Fatal("oversized wrapper was not split")
	}
}

func TestProviderSwitchImportsOnlyCompletedGap(t *testing.T) {
	f := &fakeBackend{answers: []string{"own-last-reply", "gap recalled"}}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "already-known"}}}
	a := assistant(t, request(h, "switch", req))
	url := h.sessions["switch"].URL
	req.Messages = append(req.Messages, a,
		message{Role: "user", Content: "Other provider question"},
		message{Role: "assistant", ToolCalls: []toolCall{{ID: "historical-call", Type: "function", Function: callFunction{Name: "old_function", Arguments: `{"fact":"GAP-ORCHID-91"}`}}}},
		message{Role: "tool", ToolCallID: "historical-call", Content: "GAP-ORCHID-91"},
		message{Role: "assistant", Content: "Other provider completed answer"},
		message{Role: "user", Content: "Recall gap fact"})
	req.Tools = weatherReq().Tools
	req.ToolChoice = "auto"
	w := request(h, "switch", req)
	got := assistant(t, w)
	if contentText(got) != "gap recalled" || len(got.ToolCalls) != 0 || f.urls[1] != url {
		t.Fatal(got, f.urls)
	}
	p := f.prompts[1]
	for _, s := range []string{historyIntro, historyEnd, "CURRENT REQUEST", "historical-call", "Other provider completed answer", "get_weather"} {
		if !strings.Contains(p, s) {
			t.Fatalf("missing %q: %s", s, p)
		}
	}
	if strings.Contains(p, "already-known") || strings.Contains(p, "own-last-reply") {
		t.Fatal("repeated cached prefix", p)
	}
	if strings.Index(p, "Recall gap fact") < strings.Index(p, historyEnd) {
		t.Fatal("current question in historical section")
	}
	dup := request(h, "switch", req)
	if dup.Body.String() != w.Body.String() || len(f.prompts) != 2 {
		t.Fatal("retry changed completion or resubmitted")
	}
}

func TestLargeCatchupAckAndDirtyResume(t *testing.T) {
	for _, badAck := range []bool{false, true} {
		t.Run(fmt.Sprint(badAck), func(t *testing.T) {
			f := &fakeBackend{}
			h := testHandler(f)
			h.maxChars = 1700
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			req := completionsRequest{Messages: []message{{Role: "user", Content: "known"}}}
			a := assistant(t, request(h, "large", req))
			url := h.sessions["large"].URL
			req.Messages = append(req.Messages, a)
			for i := range 8 {
				req.Messages = append(req.Messages, message{Role: "user", Content: fmt.Sprintf("gap question %d", i)}, message{Role: "assistant", Content: strings.Repeat("completed fact ", 30)})
			}
			req.Messages = append(req.Messages, message{Role: "user", Content: "current question"})
			if badAck {
				f.answers = []string{"OK", "ACK", weatherAnswer}
			}
			w := request(h, "large", req)
			if badAck {
				if w.Code != 502 || strings.Contains(w.Body.String(), `"tool_calls"`) {
					t.Fatal(w.Body.String())
				}
				h2 := testHandler(f)
				h2.statePath = h.statePath
				if err := h2.load(); err != nil {
					t.Fatal(err)
				}
				s := h2.sessions["large"]
				if !s.Dirty || s.URL != url || len(s.Messages) <= 2 || len(s.Messages) >= len(req.Messages) {
					t.Fatal("ack prefix/dirty persistence", s)
				}
				n := len(f.prompts)
				// Replay-phase interrupts resume instead of wedging: the failed
				// chunk's answer was never parsed as calls, so resubmission
				// cannot execute twice. Remaining fake answers are benign, so the
				// resumed import must complete cleanly with no tool_calls leak.
				w2 := request(h2, "large", req)
				if w2.Code != 200 || len(f.prompts) == n {
					t.Fatal("interrupted replay did not resume", w2.Code, w2.Body.String())
				}
				if strings.Contains(w2.Body.String(), `"tool_calls"`) || h2.sessions["large"].Dirty {
					t.Fatal("resumed replay leaked calls or stayed dirty")
				}
				for _, p := range f.prompts[n:] {
					if p == f.prompts[1] {
						t.Fatal("already acknowledged replay chunk was imported twice")
					}
				}
				finished := len(f.prompts)
				cached := request(h2, "large", req)
				if cached.Code != 200 || cached.Body.String() != w2.Body.String() || len(f.prompts) != finished {
					t.Fatal("resumed final completion was not cached exactly once")
				}
				return
			}
			assistant(t, w)
			maxPrompt(t, f, h.maxChars)
			if len(f.prompts) < 4 {
				t.Fatal("gap not chunked")
			}
			for i := 1; i < len(f.prompts)-1; i++ {
				if !strings.HasPrefix(f.prompts[i], historyIntro) || strings.Contains(f.prompts[i], "current question") || f.urls[i] != url {
					t.Fatal(f.prompts[i], f.urls)
				}
			}
			last := f.prompts[len(f.prompts)-1]
			if strings.Contains(last, "completed fact") || !strings.Contains(last, "CURRENT REQUEST") {
				t.Fatal(last)
			}
		})
	}
}

func TestExactPrefixRetainsMultimodalAndNameIdentity(t *testing.T) {
	oldCall := []message{{Role: "assistant", ToolCalls: []toolCall{{ID: "large-id", Function: callFunction{Name: "lookup", Arguments: `{"id":9007199254740992}`}}}}}
	editedCall := []message{{Role: "assistant", ToolCalls: []toolCall{{ID: "large-id", Function: callFunction{Name: "lookup", Arguments: `{"id":9007199254740993}`}}}}}
	if prefix(oldCall, editedCall) {
		t.Fatal("large integer edit matched cached prefix")
	}
	image := func(data string) any {
		return []any{map[string]any{"type": "text", "text": "same text"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(data))}}}
	}
	a := []message{{Role: "user", Content: image("original")}}
	b := []message{{Role: "user", Content: image("edited")}}
	if prefix(a, b) {
		t.Fatal("image edit matched old prefix")
	}
	b = []message{{Role: "user", Content: image("original"), Name: "changed"}}
	if prefix(a, b) {
		t.Fatal("name edit matched old prefix")
	}
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: a}
	reply := assistant(t, request(h, "media", req))
	url := h.sessions["media"].URL
	req.Messages = append(req.Messages, reply, message{Role: "user", Content: image("gap")}, message{Role: "assistant", Content: "media answer"}, message{Role: "user", Content: "latest"})
	assistant(t, request(h, "media", req))
	if f.urls[1] != url || f.atts[1] != 1 || strings.Count(f.prompts[1], "attached to this ChatGPT turn") != 1 {
		t.Fatal("missing gap attachment or replayed old image", f.atts)
	}
	req.Messages[0].Content = image("edited")
	assistant(t, request(h, "media", req))
	if f.urls[2] != "" || h.sessions["media"].URL == url {
		t.Fatal("edited image reused thread")
	}
}

func TestReasoningFalseDoesNotSelectThinking(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	assistant(t, request(h, "no-thinking", completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}, Reasoning: false}))
	if f.thinking[0] {
		t.Fatal("reasoning:false selected Thinking")
	}
}

func TestCatchupCurrentToolsAndImportFailure(t *testing.T) {
	for _, mode := range []string{"valid-final", "wrapper-ack", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeBackend{}
			h := testHandler(f)
			h.maxChars = 3000
			h.statePath = filepath.Join(t.TempDir(), "sessions.json")
			req := completionsRequest{Messages: []message{{Role: "user", Content: "known"}}}
			a := assistant(t, request(h, "tools-gap", req))
			url := h.sessions["tools-gap"].URL
			req.Messages = append(req.Messages, a)
			// Enough history to force several chunks at maxChars=3000 now that
			// the fixed head no longer counts against the paste budget.
			for range 12 {
				req.Messages = append(req.Messages, message{Role: "user", Content: "gap question"}, message{Role: "assistant", Content: strings.Repeat("gap answer ", 55)})
			}
			req.Messages = append(req.Messages, message{Role: "user", Content: "Call current get_weather for Hanoi"})
			req.Tools, req.ToolChoice = weatherReq().Tools, "required"
			turns, terr := h.buildTurns(req, req.Messages[2:], toolPrompt(req))
			if terr != nil {
				t.Fatalf("buildTurns: %v", terr)
			}
			if len(turns) < 3 {
				t.Fatal("test gap too small")
			}
			f.answers = []string{"OK"}
			for range len(turns) - 1 {
				f.answers = append(f.answers, "ACK")
			}
			f.answers = append(f.answers, weatherAnswer)
			if mode == "wrapper-ack" {
				f.answers[2] = weatherAnswer
			}
			if mode == "interrupted" {
				f.err, f.failAt, f.errorURL = context.Canceled, 3, url
			}
			w := request(h, "tools-gap", req)
			if mode == "valid-final" {
				got := assistant(t, w)
				if len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "get_weather" {
					t.Fatal(got)
				}
				for _, p := range f.prompts[1 : len(f.prompts)-1] {
					if strings.Contains(p, "External client functions:") {
						t.Fatal("schema in history-only chunk")
					}
				}
				if !strings.Contains(f.prompts[len(f.prompts)-1], `"required":["city"]`) {
					t.Fatal("missing current schema")
				}
				maxPrompt(t, f, h.maxChars)
				return
			}
			if w.Code != 502 || strings.Contains(w.Body.String(), `"tool_calls"`) {
				t.Fatal(w.Body.String())
			}
			h2 := testHandler(f)
			h2.statePath = h.statePath
			if err := h2.load(); err != nil {
				t.Fatal(err)
			}
			s := h2.sessions["tools-gap"]
			if !s.Dirty || s.URL != url || len(s.Messages) <= 2 || len(s.Messages) >= len(req.Messages) {
				t.Fatal("lost acknowledged-prefix/dirty journal", s)
			}
			n := len(f.prompts)
			// Replay-phase interrupts resume instead of wedging: the failed
			// chunk's output was never parsed as calls, so resubmission cannot
			// execute twice. The final fake answer is plain "OK" against a
			// required tool_choice, so the resumed request must run the full turn
			// and fail closed on protocol (422) — proving it proceeded safely
			// instead of wedging, without leaking unexecuted calls.
			w2 := request(h2, "tools-gap", req)
			if w2.Code != 422 || len(f.prompts) == n {
				t.Fatal("interrupted import neither wedged nor safely repeated", w2.Code, len(f.prompts))
			}
			if strings.Contains(w2.Body.String(), `"tool_calls"`) {
				t.Fatal("unexecuted call leaked", w2.Body.String())
			}
			if h2.sessions["tools-gap"].Dirty || h2.sessions["tools-gap"].Phase != "invalid" {
				t.Fatal("finished rejected final not durably classified")
			}
			n = len(f.prompts)
			if retry := request(h2, "tools-gap", req); retry.Code != 422 || len(f.prompts) != n {
				t.Fatal("rejected final retry resubmitted", retry.Code)
			}
		})
	}
}

func TestCompactionCreatesNewThreadAndVideoGap(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "original"}}}
	a := assistant(t, request(h, "video", req))
	url := h.sessions["video"].URL
	req.Messages = append(req.Messages, a, message{Role: "user", Content: []any{map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte("clip"))}}}}, message{Role: "assistant", Content: "video observed"}, message{Role: "user", Content: "recall video"})
	assistant(t, request(h, "video", req))
	if f.atts[1] != 1 || f.urls[1] != url {
		t.Fatal("video gap not forwarded", f.atts, f.urls)
	}
	req.Messages = []message{{Role: "system", Content: "Compacted summary: video observed"}, {Role: "user", Content: "recall video"}}
	assistant(t, request(h, "video", req))
	if f.urls[2] != "" || h.sessions["video"].URL == url {
		t.Fatal("compaction reused thread")
	}

	// Rich rotation payload (stage 2): full tool results and a wide verbatim
	// window within budget, degrading rung by rung to the legacy card shape.
	t.Setenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION", "1")
	f2 := &fakeBackend{}
	h2 := testHandler(f2)
	big := strings.Repeat("R", 1500)
	req2 := completionsRequest{Messages: []message{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", Content: "working", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "f", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_1", Content: big},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "next"},
	}, ToolChoice: "none"}
	assistant(t, request(h2, "rich", req2))
	req2.Messages = []message{{Role: "system", Content: "Compacted summary"}, {Role: "user", Content: "next after compact"}}
	assistant(t, request(h2, "rich", req2))
	// Unified rotation: the carried executed exchange travels as an inert
	// record import ahead of the rotation card (record + card).
	if len(f2.prompts) != 3 {
		t.Fatalf("rich rotation submitted %d prompts, want 3 (record + card)", len(f2.prompts))
	}
	record := f2.prompts[1]
	for _, want := range []string{"PRIOR EXECUTION RECORD", "call_1", big[:500]} {
		if !strings.Contains(record, want) {
			t.Fatalf("rotation record import missing %q", want[:20])
		}
	}
	if f2.urls[1] != "" || f2.urls[2] != h2.sessions["rich"].URL || h2.sessions["rich"].URL == "" {
		t.Fatalf("rotation record did not seed the project thread record-first: urls=%v thread=%s", f2.urls, h2.sessions["rich"].URL)
	}
	card := f2.prompts[2]
	for _, want := range []string{big, "do the thing", "next", "CONTEXT CARD"} {
		if !strings.Contains(card, want) {
			t.Fatalf("rotation prompt missing %q", want[:20])
		}
	}
	if strings.Contains(card, "call_1") {
		t.Fatal("rotation card re-submitted the carried call ID")
	}
	for _, want := range []string{"rewrite-compaction", "via=rotation", "call_1(f)"} {
		if !strings.Contains(h2.sessions["rich"].ReseedEvidence, want) {
			t.Fatalf("rotation evidence lacks %q: %s", want, h2.sessions["rich"].ReseedEvidence)
		}
	} // Ladder degrades monotonically and bottoms out at the legacy shape,
	// judged on the pre-rotation history the card was built from (the live
	// session now holds the rotated transcript).
	head := toolPrompt(req2)
	pre := &session{Messages: []message{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", Content: "working", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "f", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_1", Content: big},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "next"},
	}}
	s := pre
	var prevLen = -1
	for i, lv := range rotationLadder {
		p, _ := buildRotationPromptLevel(req2, s, head, lv)
		if prevLen >= 0 && len(p) > prevLen {
			t.Fatalf("ladder rung %d larger than previous: %d > %d", i, len(p), prevLen)
		}
		prevLen = len(p)
		if i == 0 && !strings.Contains(p, big) {
			t.Fatal("top rung dropped full tool result")
		}
		if i == len(rotationLadder)-1 {
			if strings.Contains(p, big[500:1000]) {
				t.Fatal("bottom rung kept full tool result")
			}
			if p != buildRotationPrompt(req2, s, head) {
				t.Fatal("bottom rung diverged from legacy card shape")
			}
		}
	}
}

func TestTooSmallChunkBudgetDoesNotSubmit(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 100
	if w := request(h, "budget", weatherReq()); w.Code != 400 || len(f.prompts) != 0 || h.sessions["budget"] != nil {
		t.Fatal("oversized schema submitted or dirtied", w.Body.String())
	}
}

func fatTools(n int, pad int) []toolDef {
	out := make([]toolDef, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, toolDef{Type: "function", Function: toolFunction{
			Name:        fmt.Sprintf("tool_%02d", i),
			Description: "does something useful " + strings.Repeat("x", pad),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"arg": map[string]any{"type": "string", "description": strings.Repeat("y", pad)}}},
		}})
	}
	return out
}

func TestToolSchemasTaughtOncePerThread(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 300000
	tools := fatTools(20, 2000) // full dump ~90KB: fits once, must not repeat
	schemaBytes := toolsBytes(tools)
	if schemaBytes < 80000 {
		t.Fatalf("fat toolset too small to prove anything: %d", schemaBytes)
	}
	req := completionsRequest{Messages: []message{{Role: "user", Content: "hi there"}}, Tools: tools}
	a1 := assistant(t, request(h, "teach", req))
	if !strings.Contains(f.prompts[0], "External client functions:") {
		t.Fatal("first turn did not teach full schemas")
	}
	url := h.sessions["teach"].URL
	if h.sessions["teach"].ToolsFP == "" {
		t.Fatal("taught fingerprint not persisted")
	}
	// Continuation with identical tools: same thread, compact reminder only.
	req.Messages = append(req.Messages, a1, message{Role: "user", Content: "and now?"})
	a2 := assistant(t, request(h, "teach", req))
	if f.urls[1] != url {
		t.Fatal("continuation left the thread")
	}
	if strings.Contains(f.prompts[1], "External client functions:") {
		t.Fatal("continuation re-pasted full schemas")
	}
	if !strings.Contains(f.prompts[1], "already taught") {
		t.Fatal("continuation missing compact reminder")
	}
	if len(f.prompts[1]) > schemaBytes/4 {
		t.Fatalf("reminder turn too large: %d", len(f.prompts[1]))
	}
	// Changed tool set: re-teach in full on the same thread.
	req.Tools = append(append([]toolDef{}, tools...), toolDef{Type: "function", Function: toolFunction{Name: "brand_new", Parameters: map[string]any{"type": "object"}}})
	req.Messages = append(req.Messages, a2, message{Role: "user", Content: "use it"})
	assistant(t, request(h, "teach", req))
	last := f.prompts[len(f.prompts)-1]
	if f.urls[len(f.urls)-1] != url {
		t.Fatal("tool change left the thread")
	}
	if !strings.Contains(last, "External client functions:") || !strings.Contains(last, "brand_new") {
		t.Fatal("changed toolset not re-taught")
	}
}

func TestRewriteReseedCarriesExecutedResults(t *testing.T) {
	// Two committed turns: a weather tool call with its recorded result,
	// then a text follow-up. The client then forks history, dropping the
	// executed exchange but staying internally coherent. The bridge must
	// reseed a fresh thread with the rewritten timeline plus the carried
	// executed results as inert history — never re-submitting them.
	f := &fakeBackend{answers: []string{weatherAnswer, "noted", "ACK", "fresh summary"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	a := assistant(t, request(h, "reseed", req))
	callID := a.ToolCalls[0].ID
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: callID, Content: "25 C"}, message{Role: "user", Content: "and tomorrow?"})
	req.ToolChoice = "none"
	if got := assistant(t, request(h, "reseed", req)); contentText(got) != "noted" {
		t.Fatal("follow-up did not commit", got)
	}
	oldURL := h.sessions["reseed"].URL
	if oldURL == "" || h.sessions["reseed"].Dirty {
		t.Fatal("turns did not commit cleanly")
	}
	rewritten := completionsRequest{Messages: []message{
		{Role: "user", Content: "Call weather for Hanoi"},
		{Role: "user", Content: "and tomorrow?"},
	}, ToolChoice: "none"}
	got := assistant(t, request(h, "reseed", rewritten))
	if contentText(got) != "fresh summary" {
		t.Fatalf("reseed did not answer fresh: %+v", got)
	}
	s := h.sessions["reseed"]
	if s.Dirty || s.URL == "" || s.URL == oldURL {
		t.Fatalf("reseed reused or abandoned the thread: old=%s new=%s dirty=%v", oldURL, s.URL, s.Dirty)
	}
	// Four submissions: turn1, turn2, record import, rewritten final.
	if len(f.prompts) != 4 || f.urls[2] != "" || f.urls[3] != s.URL {
		t.Fatalf("reseed did not seed a fresh thread record-first: urls=%v", f.urls)
	}
	record, final := f.prompts[2], f.prompts[3]
	for _, want := range []string{historyIntro, "PRIOR EXECUTION RECORD", callID, "get_weather", "25 C", "END PRIOR EXECUTION RECORD"} {
		if !strings.Contains(record, want) {
			t.Fatalf("record import lacks %q", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(record), "ACK. No tool calls.") && !strings.Contains(record, "ACK. No tool calls.") {
		t.Fatal("record import is not an inert ACK turn")
	}
	// The already-executed call is never re-submitted as pending: its ID
	// and no wrapper tag appear anywhere in the answer-seeking final
	// prompt. (The head instructions name the wrapper format in prose;
	// only a literal tag would be an executable re-submission.)
	if strings.Contains(final, callID) || strings.Contains(final, "<opencode_bridge_call>") || strings.Contains(final, `<opencode\_bridge\_call>`) {
		t.Fatal("carried call re-submitted in final prompt")
	}
	if !strings.Contains(final, "and tomorrow?") {
		t.Fatal("rewritten timeline missing from final prompt")
	}
	// Journal evidence: classification, linkage, carried items.
	for _, want := range []string{"rewrite-divergence", "old=" + oldURL, "new=" + s.URL, callID + "(get_weather)"} {
		if !strings.Contains(s.ReseedEvidence, want) {
			t.Fatalf("evidence lacks %q: %s", want, s.ReseedEvidence)
		}
	}
	if s.ReseedFrom != oldURL {
		t.Fatalf("linkage lost: %q", s.ReseedFrom)
	}
	// Stable duplicate cache: exact retry submits nothing further.
	body := request(h, "reseed", rewritten).Body.String()
	if retry := request(h, "reseed", rewritten); retry.Body.String() != body || len(f.prompts) != 4 {
		t.Fatal("retry after reseed resubmitted")
	}
	// Restart survival: journal linkage plus the sent-log reseed entry.
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	ls := loaded.sessions["reseed"]
	if ls.ReseedEvidence != s.ReseedEvidence || ls.ReseedFrom != oldURL || ls.Dirty {
		t.Fatal("reseed evidence lost across restart")
	}
	if retry := request(loaded, "reseed", rewritten); retry.Body.String() != body {
		t.Fatal("restarted duplicate cache missed")
	}
	last := loaded.lastSentFor("reseed")
	if last == nil || last.Event != "reseed" || last.URL != s.URL || !strings.Contains(last.Note, "old="+oldURL) || !strings.Contains(last.Note, "rewrite-divergence") || !strings.Contains(last.Note, callID) {
		t.Fatalf("sent-log reseed linkage missing: %+v", last)
	}
}

func TestRotationUnificationCarriesRecordAndFreshEvidence(t *testing.T) {
	// Live defect: a prefix-mismatched project session rotated via the
	// legacy path, shadowing the rewrite-reseed plan — no carried executed
	// results, and the journal kept stale evidence from an earlier event.
	// Rotation must travel record-first and always overwrite the evidence
	// with this event's linkage.
	t.Setenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION", "1")
	const projectURL = "https://chatgpt.com/g/g-p-proj/project"
	f := &fakeBackend{answers: []string{weatherAnswer, "noted", "ACK", "rotated summary"}, projectEcho: projectURL}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	a := assistant(t, request(h, "proj", req))
	callID := a.ToolCalls[0].ID
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: callID, Content: "25 C"}, message{Role: "user", Content: "and tomorrow?"})
	req.ToolChoice = "none"
	if got := assistant(t, request(h, "proj", req)); contentText(got) != "noted" {
		t.Fatal("follow-up did not commit", got)
	}
	s := h.sessions["proj"]
	oldURL := s.URL
	if oldURL == "" || s.Dirty {
		t.Fatal("turns did not commit cleanly")
	}
	// Plant the stale entry the defect left behind: an earlier event's
	// linkage that this rotation must overwrite, never keep.
	s.ReseedEvidence = "rewrite-reseed 2026-10-16T10:02:38Z classification=rewrite-divergence mismatch=1:content old=https://chatgpt.com/c/ancient new=https://chatgpt.com/c/stale carried=[] not-carried=[] seed=2turns/100B record-first"
	s.ReseedFrom = "https://chatgpt.com/c/ancient"
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	rewritten := completionsRequest{Messages: []message{
		{Role: "user", Content: "Call weather for Hanoi"},
		{Role: "user", Content: "and tomorrow?"},
	}, ToolChoice: "none"}
	got := assistant(t, request(h, "proj", rewritten))
	if contentText(got) != "rotated summary" {
		t.Fatalf("rotation did not answer fresh: %+v", got)
	}
	s = h.sessions["proj"]
	if s.Dirty || s.URL == "" || s.URL == oldURL {
		t.Fatalf("rotation reused or abandoned the thread: old=%s new=%s dirty=%v", oldURL, s.URL, s.Dirty)
	}
	// Four submissions: turn1, turn2, record import (mints the project
	// thread), rotation card (continues it). Nothing new lands on the old
	// thread after the rotation.
	if len(f.prompts) != 4 || f.urls[2] != "" || f.urls[3] != s.URL {
		t.Fatalf("rotation did not seed the project thread record-first: urls=%v", f.urls)
	}
	for _, u := range f.urls[2:] {
		if u == oldURL {
			t.Fatalf("rotation touched the old thread: urls=%v", f.urls)
		}
	}
	record, card := f.prompts[2], f.prompts[3]
	for _, want := range []string{historyIntro, "PRIOR EXECUTION RECORD", callID, "get_weather", "25 C", "END PRIOR EXECUTION RECORD"} {
		if !strings.Contains(record, want) {
			t.Fatalf("rotation record import lacks %q", want)
		}
	}
	if strings.Contains(card, callID) {
		t.Fatal("rotation card re-submitted the carried call ID")
	}
	for _, want := range []string{"BRIDGE SESSION ROTATION", "CONTEXT CARD", "and tomorrow?"} {
		if !strings.Contains(card, want) {
			t.Fatalf("rotation card lacks %q", want)
		}
	}
	// Fresh evidence for THIS event: classification, carried IDs, old→new
	// linkage, rotation marker — and no trace of the planted stale entry.
	for _, want := range []string{"rewrite-divergence", "old=" + oldURL, "new=" + s.URL, callID + "(get_weather)", "via=rotation"} {
		if !strings.Contains(s.ReseedEvidence, want) {
			t.Fatalf("rotation evidence lacks %q: %s", want, s.ReseedEvidence)
		}
	}
	for _, stale := range []string{"ancient", "10:02:38Z", "c/stale"} {
		if strings.Contains(s.ReseedEvidence, stale) {
			t.Fatalf("rotation kept stale evidence %q: %s", stale, s.ReseedEvidence)
		}
	}
	if s.ReseedFrom != oldURL {
		t.Fatalf("rotation linkage lost: %q", s.ReseedFrom)
	}
	// The new thread landed in the session project.
	if s.Project != projectURL {
		t.Fatalf("rotation left the session project: %q", s.Project)
	}
	if f.projects[2] == "" {
		t.Fatalf("record import did not target the session project: %v", f.projects)
	}
	// Stable duplicate cache + restart survival, like the shared path.
	body := request(h, "proj", rewritten).Body.String()
	if retry := request(h, "proj", rewritten); retry.Body.String() != body || len(f.prompts) != 4 {
		t.Fatal("retry after rotation resubmitted")
	}
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	ls := loaded.sessions["proj"]
	if ls.ReseedEvidence != s.ReseedEvidence || ls.ReseedFrom != oldURL || ls.Dirty || ls.Project != projectURL {
		t.Fatal("rotation evidence lost across restart")
	}
	last := loaded.lastSentFor("proj")
	if last == nil || last.Event != "reseed" || last.URL != s.URL || !strings.Contains(last.Note, "old="+oldURL) || !strings.Contains(last.Note, "via=rotation") {
		t.Fatalf("sent-log rotation linkage missing: %+v", last)
	}
	var imports, finals int
	for _, e := range loaded.sentLogTail(50) {
		if e.Event == "completed" && e.Session == "proj" && e.URL == s.URL {
			if e.PromptHash == promptHash(f.prompts[2]) {
				imports++
			}
			if e.PromptHash == promptHash(f.prompts[3]) {
				finals++
			}
		}
	}
	if imports != 1 || finals != 1 {
		t.Fatalf("rotation submissions missing from durable lineage: imports=%d finals=%d", imports, finals)
	}
}

const tinyPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestRasterImagesServedOnWireNotInTranscript(t *testing.T) {
	img := chatgpt.RasterImage{Alt: "Elimination; rotation 4 eliminated", DataURL: tinyPNGDataURL}
	f := &fakeBackend{turn: func(_ context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
		return chatgpt.TurnResult{Answer: "Here is the result.\n\n[SVG: Elimination; rotation 4 eliminated]", URL: "https://chatgpt.com/c/thread-1", Images: []chatgpt.RasterImage{img}}, nil
	}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "draw it"}}}
	a := assistant(t, request(h, "session-img", req))
	body := contentText(a)
	if !strings.Contains(body, "![Elimination; rotation 4 eliminated]("+tinyPNGDataURL+")") {
		t.Fatalf("wire content missing png attachment: %q", body)
	}
	if !strings.Contains(body, "[SVG: Elimination; rotation 4 eliminated]") {
		t.Fatalf("text fallback lost: %q", body)
	}
	// The stored replay transcript stays text-only: base64 payloads are never
	// pasted back into the ChatGPT thread.
	s := h.sessions["session-img"]
	if s == nil || len(s.Messages) == 0 {
		t.Fatal("no transcript stored")
	}
	for _, m := range s.Messages {
		if strings.Contains(contentText(m), "data:image/png") {
			t.Fatalf("base64 leaked into replay transcript: %q", contentText(m))
		}
	}
	// Duplicate retry re-serves the attachment without resubmitting.
	dup := assistant(t, request(h, "session-img", req))
	if !strings.Contains(contentText(dup), tinyPNGDataURL) {
		t.Fatal("duplicate cache dropped raster images")
	}
}

func TestRasterImagesInvalidNeverReachWire(t *testing.T) {
	f := &fakeBackend{turn: func(_ context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
		return chatgpt.TurnResult{Answer: "diagram below", URL: "https://chatgpt.com/c/thread-1", Images: []chatgpt.RasterImage{
			{Alt: "svg stays text", DataURL: "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="},
			{Alt: "broken]", DataURL: "data:image/png;base64,!!!not-base64!!!"},
		}}, nil
	}}
	h := testHandler(f)
	req := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "draw it"}}}
	a := assistant(t, request(h, "session-img-bad", req))
	if body := contentText(a); body != "diagram below" {
		t.Fatalf("invalid images altered the answer: %q", body)
	}
}

func TestImageMarkdownAltSanitized(t *testing.T) {
	got := imageMarkdown([]chatgpt.RasterImage{{Alt: "a]b[c\nnewline", DataURL: tinyPNGDataURL}})
	if !strings.HasPrefix(got, "\n\n![abc newline]("+tinyPNGDataURL+")") {
		t.Fatalf("alt not sanitized: %q", got)
	}
	if imageMarkdown(nil) != "" || imageMarkdown([]chatgpt.RasterImage{{Alt: "x", DataURL: "data:image/gif;base64,R0lGOD"}}) != "" {
		t.Fatal("empty/non-png input must yield no attachment")
	}
}

func TestParseAnswerToleratesImageMarkdown(t *testing.T) {
	out, err := parseAnswer("See the result.\n\n![diagram]("+tinyPNGDataURL+")", completionsRequest{})
	if err != nil || len(out.Calls) != 0 || !strings.Contains(out.Text, "![diagram](") {
		t.Fatalf("image markdown rejected: %+v %v", out, err)
	}
}

// An SVG/canvas answer reaches the text client as an unreadable placeholder,
// so the plain-text rendering rule must ride EVERY head variant: full teaching,
// compact reminder, and the no-functions path in both.
func TestRenderingRuleRidesEveryHead(t *testing.T) {
	req := weatherReq()
	for _, tc := range []struct{ name, head string }{
		{"teach full", toolPrompt(req)},
		{"reminder", toolReminder(req)},
		{"teach full without tools", toolPrompt(completionsRequest{})},
		{"reminder without tools", toolReminder(completionsRequest{})},
	} {
		if !strings.Contains(tc.head, "ASCII/Unicode art") || !strings.Contains(tc.head, "Never output SVG") {
			t.Fatalf("%s head missing the plain-text rendering rule: %q", tc.name, tc.head)
		}
	}
}

func TestRasterImagesStreamedAsDelta(t *testing.T) {
	img := chatgpt.RasterImage{Alt: "d", DataURL: tinyPNGDataURL}
	f := &fakeBackend{turn: func(_ context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
		return chatgpt.TurnResult{Answer: "done", URL: "https://chatgpt.com/c/thread-1", Images: []chatgpt.RasterImage{img}}, nil
	}}
	h := testHandler(f)
	req := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "draw it"}}, Stream: true}
	w := request(h, "session-img-stream", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), tinyPNGDataURL) || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream missing png delta: %d %q", w.Code, w.Body.String())
	}
}

// A terminal failure must be actionable by a person: what is unknown, the
// explicit no-resend/no-rerun promise, and one thing they can SAY next. The
// HTTP runbook belongs in structured detail and logs, never in the sentence.
// The four classes must stay distinct and the pre-submit path must stay
// immediately retryable instead of being handed recovery advice.
func TestTerminalFailuresAreActionableAndKeepClassification(t *testing.T) {
	type detail struct {
		State      string   `json:"state"`
		Actions    []string `json:"actions"`
		CanRecover bool     `json:"can_recover"`
		CanResync  bool     `json:"can_resync"`
		Runbook    string   `json:"runbook"`
	}
	envelope := func(t *testing.T, w *httptest.ResponseRecorder) (string, detail) {
		t.Helper()
		var v struct {
			Error struct {
				Message string `json:"message"`
				Details detail `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("not structured JSON: %d %s", w.Code, w.Body.String())
		}
		return v.Error.Message, v.Error.Details
	}

	t.Run("genuinely unverifiable gives one actionable sentence", func(t *testing.T) {
		f := &fakeBackend{}
		h := testHandler(f)
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		h.sessions["stuck"] = &session{Dirty: true}
		w := request(h, "stuck", weatherReq())
		if w.Code != 422 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		msg, det := envelope(t, w)
		for _, want := range []string{"saved record of this chat is incomplete", "Nothing from your previous message was resent", `say "run chatgpt-recover and reset this chat"`} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message missing %q: %s", want, msg)
			}
		}
		if strings.Contains(msg, "POST /v1/sessions") || strings.Contains(msg, "http") {
			t.Fatalf("runbook leaked into the chat sentence: %s", msg)
		}
		if det.State != statePendingUnverifiable || len(det.Actions) != 1 || det.Actions[0] != actionAbandon || !strings.Contains(det.Runbook, "POST /v1/sessions/abandon") {
			t.Fatalf("machine-actionable detail missing: %+v", det)
		}
		if len(f.prompts) != 0 || h.sessions["stuck"].Dirty != true {
			t.Fatal("refusal submitted a turn or cleared uncertainty")
		}
	})

	t.Run("verifiable pending offers recovery instead of a reset", func(t *testing.T) {
		f := &fakeBackend{}
		h := testHandler(f)
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		req := weatherReq()
		h.sessions["waiting"] = &session{Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "p"}
		w := request(h, "waiting", req)
		if w.Code != 422 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		msg, det := envelope(t, w)
		if !strings.Contains(msg, "can still be read back") || !strings.Contains(msg, `say "run chatgpt-recover"`) || strings.Contains(msg, "reset this chat") {
			t.Fatalf("verifiable pending misreported: %s", msg)
		}
		if det.State != statePendingVerifiable || !det.CanRecover {
			t.Fatalf("detail lost the recovery capability: %+v", det)
		}
		if len(f.prompts) != 0 {
			t.Fatal("uncertain turn was resubmitted")
		}
	})

	t.Run("rejected-but-produced output is not reported as empty", func(t *testing.T) {
		f := &fakeBackend{}
		h := testHandler(f)
		req := weatherReq()
		h.sessions["rej"] = &session{URL: "https://chatgpt.com/c/t", Phase: "invalid", RejectedFingerprint: requestFingerprint(req), RejectReason: rejectToolProtocol}
		w := request(h, "rej", req)
		if w.Code != 422 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		msg, _ := envelope(t, w)
		if !strings.Contains(msg, "replied in a form that could not be used") || strings.Contains(msg, "without producing an answer") {
			t.Fatalf("protocol refusal misreported: %s", msg)
		}
		if !strings.Contains(msg, noResend) || !strings.Contains(msg, nextFreshThread) {
			t.Fatalf("refusal is not actionable: %s", msg)
		}
		if len(f.prompts) != 0 {
			t.Fatal("rejected output was resubmitted")
		}
	})

	t.Run("genuinely empty final is not reported as a refusal", func(t *testing.T) {
		f := &fakeBackend{}
		h := testHandler(f)
		req := weatherReq()
		h.sessions["empty"] = &session{URL: "https://chatgpt.com/c/t", Phase: "invalid", RejectedFingerprint: requestFingerprint(req), RejectReason: rejectEmptyFinal}
		w := request(h, "empty", req)
		if w.Code != 422 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		msg, _ := envelope(t, w)
		if !strings.Contains(msg, "finished without producing an answer") || strings.Contains(msg, "could not be used") {
			t.Fatalf("empty final misreported: %s", msg)
		}
		if len(f.prompts) != 0 {
			t.Fatal("completed-empty turn was resubmitted")
		}
	})

	t.Run("proven pre-submit failure stays immediately retryable", func(t *testing.T) {
		f := &fakeBackend{err: chatgpt.ErrNotSubmitted}
		h := testHandler(f)
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		req := weatherReq()
		w := request(h, "pre", req)
		if w.Code == 422 || strings.Contains(w.Body.String(), "chatgpt-recover") {
			t.Fatalf("pre-submit failure handed recovery advice: %d %s", w.Code, w.Body.String())
		}
		if s := h.sessions["pre"]; s == nil || s.Dirty || s.PendingRequest != nil {
			t.Fatalf("pre-submit failure kept uncertainty: %+v", s)
		}
		if v := stateBody(t, h, "pre"); fmt.Sprint(v["state"]) != stateClean {
			t.Fatalf("pre-submit failure not reported clean: %v", v["state"])
		}
		f.err, f.answers = nil, []string{"", weatherAnswer} // index by prompt count: the failed attempt was prompt 1
		if a := assistant(t, request(h, "pre", req)); len(a.ToolCalls) != 1 {
			t.Fatalf("retry after a proven pre-submit failure did not run: %+v", a)
		}
	})
}

// A draft this bridge cannot prove it wrote must be a NON-RETRYABLE refusal
// that leaves the session clean: nothing was typed over and nothing was sent.
// The person gets what happened and what to do instead — never a selector or an
// endpoint. Recovery clears journal uncertainty only, never a browser draft, so
// the sentence must not imply otherwise.
func TestDraftRefusalsArePreSubmitAndActionable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			"foreign draft",
			fmt.Errorf("%w (target=ABC document_units=28 verdict=diverged retryable=false)", chatgpt.ErrForeignDraft),
			[]string{"cannot verify it wrote", "keep or clear that text yourself", "Nothing from your previous message was resent"},
		},
		{
			"attempted turn draft",
			fmt.Errorf("%w (target=ABC document_units=28 retryable=false)", chatgpt.ErrDraftOwned),
			[]string{"may already have been sent", "Do not resend it", "separate manual step", "Nothing from your previous message was resent"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !chatgpt.IsPreSubmit(tc.err) {
				t.Fatal("a draft refusal must be pre-submit so the session stays clean")
			}
			if errors.Is(tc.err, chatgpt.ErrNotSubmitted) {
				t.Fatal("a draft refusal must not claim the retryable pre-submit class")
			}
			h := testHandler(&fakeBackend{})
			w := httptest.NewRecorder()
			h.backendError(w, tc.err)
			if w.Code != 422 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var v struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatalf("not structured JSON: %s", w.Body.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(v.Error.Message, want) {
					t.Fatalf("message missing %q: %s", want, v.Error.Message)
				}
			}
			// A draft refusal must never claim to know who wrote the text, and
			// an uncertain turn must never invite a blind retry.
			for _, bad := range []string{"did not type", "check whether that message was sent", "before retrying"} {
				if strings.Contains(v.Error.Message, bad) {
					t.Fatalf("unsafe guidance %q: %s", bad, v.Error.Message)
				}
			}
			for _, leak := range []string{"POST /v1/sessions", "http://", "chatgpt-recover"} {
				if strings.Contains(v.Error.Message, leak) {
					t.Fatalf("runbook or recovery hint leaked into the chat sentence: %s", v.Error.Message)
				}
			}
		})
	}
}

func TestUnavailableComposerIsCleanAndNonRetryable(t *testing.T) {
	for _, err := range []error{chatgpt.ErrNoComposer, chatgpt.ErrNotLoggedIn} {
		t.Run(err.Error(), func(t *testing.T) {
			calls := 0
			h := testHandler(&fakeBackend{turn: func(context.Context, chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
				calls++
				return chatgpt.TurnResult{}, err
			}})
			w := request(h, "unavailable", completionsRequest{Messages: []message{{Role: "user", Content: "synthetic request"}}, ToolChoice: "none"})
			if w.Code != 422 || calls != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
			s := h.sessions["unavailable"]
			if s == nil || s.Dirty || s.PendingRequest != nil || s.PendingPrompt != "" {
				t.Fatal("known unsent refusal left false uncertainty")
			}
			if !strings.Contains(w.Body.String(), "nothing was submitted") {
				t.Fatal("refusal does not explain the unsent outcome")
			}
		})
	}
}

// Concurrent sessions must not share a backend, a target or a draft decision.
// One session's failed insertion is a clean, immediately retryable pre-submit
// failure that is never resubmitted; its peer's turn is unaffected, and neither
// session's text reaches the other.
func TestParallelDraftIsolationAndHandoff(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	seen := map[string][]string{}
	record := func(backend, prompt string) {
		mu.Lock()
		seen[prompt] = append(seen[prompt], backend)
		mu.Unlock()
	}
	// The reported shape: an oversized legacy insertion truncated in the
	// composer, nothing submitted. The pool hands out whichever backend is free,
	// so every backend must behave by request, not by identity.
	client := func(name string) *fakeBackend {
		return &fakeBackend{turn: func(_ context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
			record(name, opt.Prompt)
			entered <- name
			<-release
			if strings.Contains(opt.Prompt, "FAIL_A") {
				return chatgpt.TurnResult{URL: "https://chatgpt.com/c/iso-a"},
					fmt.Errorf("%w: composer did not settle to the complete prompt, nothing submitted (document_units=3327 prompt_units=180955 verdict=truncated)",
						chatgpt.ErrNotSubmitted)
			}
			if strings.Contains(opt.Prompt, "NEW_A_AFTER_HANDOFF") {
				return chatgpt.TurnResult{Answer: "A_HANDOFF", URL: "https://chatgpt.com/c/iso-a"}, nil
			}
			return chatgpt.TurnResult{Answer: "B_ANSWER", URL: "https://chatgpt.com/c/iso-b"}, nil
		}}
	}
	h := testHandler(&fakeBackend{})
	h.profileDir = t.TempDir()
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	b1, b2 := client("backend-1"), client("backend-2")
	h.enableParallel([]backend{b1, b2})

	doneA := make(chan *httptest.ResponseRecorder, 1)
	doneB := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		doneA <- request(h, "iso-a", completionsRequest{Messages: []message{{Role: "user", Content: "FAIL_A"}}, ToolChoice: "none"})
	}()
	go func() {
		doneB <- request(h, "iso-b", completionsRequest{Messages: []message{{Role: "user", Content: "KEEP_B"}}, ToolChoice: "none"})
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent sessions did not reach their own backend")
		}
	}
	unblock()
	var wA, wB *httptest.ResponseRecorder
	select {
	case wA = <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("failing session did not complete")
	}
	select {
	case wB = <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("healthy session did not complete")
	}

	// Each session was served exactly once, on its own backend, and never with
	// the other session's text. A resend would show up as a second entry.
	mu.Lock()
	var failBy, okBy []string
	var failPrompt, okPrompt string
	for prompt, backends := range seen {
		switch {
		case strings.Contains(prompt, "FAIL_A"):
			failPrompt, failBy = prompt, backends
		case strings.Contains(prompt, "KEEP_B"):
			okPrompt, okBy = prompt, backends
		}
	}
	mu.Unlock()
	// Exactly one attempt each: an automatic resend would appear as a second
	// entry for the same session's text.
	if failPrompt == "" || len(failBy) != 1 {
		t.Fatalf("failing session was attempted %d times", len(failBy))
	}
	if okPrompt == "" || len(okBy) != 1 {
		t.Fatalf("peer session was attempted %d times", len(okBy))
	}
	// Concurrent sessions must never share one backend (one browser target).
	if failBy[0] == okBy[0] {
		t.Fatalf("both concurrent sessions were served by %s", failBy[0])
	}

	if wB.Code != 200 {
		t.Fatalf("peer status %d: %s", wB.Code, wB.Body.String())
	}
	if got := contentText(assistant(t, wB)); got != "B_ANSWER" {
		t.Fatalf("peer answer = %q, want B_ANSWER", got)
	}
	if strings.Contains(wB.Body.String(), "FAIL_A") {
		t.Fatal("the failing session's text leaked into the peer response")
	}

	// A proven pre-submit failure is retryable and leaves the session clean; it
	// must not be reclassified as a foreign draft or handed recovery advice.
	if wA.Code == 200 || wA.Code == 422 {
		t.Fatalf("failed insertion status %d: %s", wA.Code, wA.Body.String())
	}
	if strings.Contains(wA.Body.String(), "chatgpt-recover") || strings.Contains(wA.Body.String(), "cannot verify it wrote") {
		t.Fatalf("failed insertion was misreported: %s", wA.Body.String())
	}
	if strings.Contains(wA.Body.String(), "KEEP_B") {
		t.Fatal("the peer's text leaked into the failing response")
	}
	if s := h.sessions["iso-a"]; s == nil || s.Dirty || s.PendingRequest != nil {
		t.Fatalf("failed insertion kept uncertainty: %+v", s)
	}
	if s := h.sessions["iso-b"]; s == nil || s.Dirty || s.PendingRequest != nil {
		t.Fatalf("peer session was left unclean: %+v", s)
	}
	if v := stateBody(t, h, "iso-a"); fmt.Sprint(v["state"]) != stateClean {
		t.Fatalf("failed insertion not reported clean: %v", v["state"])
	}
	// Force the next ordinary request onto the other worker. This is a new
	// request after proven non-submission, not a resend of uncertain history.
	<-h.parallel.clients
	<-h.parallel.clients
	other, original := backend(b1), backend(b2)
	otherName := "backend-1"
	if failBy[0] == otherName {
		other, original, otherName = b2, b1, "backend-2"
	}
	h.parallel.clients <- other
	w := request(h, "iso-a", completionsRequest{Messages: []message{{Role: "user", Content: "NEW_A_AFTER_HANDOFF"}}, ToolChoice: "none"})
	h.parallel.clients <- original
	if w.Code != 200 {
		t.Fatalf("new request after handoff: status=%d", w.Code)
	}
	if contentText(assistant(t, w)) != "A_HANDOFF" || h.sessions["iso-a"].URL == h.sessions["iso-b"].URL {
		t.Fatal("handoff mixed results or thread identities")
	}
	if h.sessions["iso-b"].Dirty || len(h.sessions["iso-b"].Messages) != 2 {
		t.Fatal("handoff changed the healthy peer's completed state")
	}
	mu.Lock()
	defer mu.Unlock()
	handoffCalls := 0
	for prompt, workers := range seen {
		if strings.Contains(prompt, "NEW_A_AFTER_HANDOFF") {
			handoffCalls++
			if len(workers) != 1 || workers[0] != otherName {
				t.Fatal("handoff did not use the other worker exactly once")
			}
		}
		if strings.Contains(prompt, "FAIL_A") && len(workers) != 1 {
			t.Fatal("handoff resubmitted the failed earlier request")
		}
	}
	if handoffCalls != 1 {
		t.Fatal("handoff request was not isolated")
	}
}

func TestProtocolReasonCodeIsContentFree(t *testing.T) {
	for msg, want := range map[string]string{
		"malformed tool JSON: unexpected end":                    "malformed_json",
		"tool arguments must be a JSON object":                   "bad_arguments",
		`tool name "evil" is not allowed by tools/tool_choice`:   "disallowed_tool",
		"tool run arguments: missing required":                   "schema_violation",
		"incomplete opencode_bridge_call wrapper":                "incomplete_wrapper",
		"bridge calls must not appear in Markdown code examples": "fenced_calls",
		"backend did not honor required tool_choice":             "ignored_tool_choice",
		"completed model output contains no actionable answer":   "empty_answer",
		"something else entirely":                                "protocol_error",
	} {
		if got := protocolReasonCode(errors.New(msg)); got != want {
			t.Fatalf("protocolReasonCode(%q) = %q, want %q", msg, got, want)
		}
	}
	if got := protocolReasonCode(nil); got != "ok" {
		t.Fatalf("protocolReasonCode(nil) = %q, want ok", got)
	}
}

// A single message is never split across submissions: an interruption would
// leave the thread holding part of a request with no answer of its own.
func TestBuildTurnsRefusesToSplitOneMessage(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.maxChars = 20000
	req := completionsRequest{Messages: []message{{Role: "user", Content: strings.Repeat("x", 40000)}}}
	turns, err := h.buildTurns(req, req.Messages, toolReminder(req))
	if err == nil || len(turns) != 0 {
		t.Fatalf("oversized single message produced %d turns, err=%v", len(turns), err)
	}
	if !strings.Contains(err.Error(), "will not split a single message") {
		t.Fatalf("refusal does not explain splitting: %v", err)
	}
}

// An unpasteable final turn is refused before anything is journaled or sent,
// because the composer stalls above the single-paste limit.
func TestBuildTurnsRefusesUnpasteableFinalTurn(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.maxChars = 200000
	h.maxPaste = 1000
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	f := &fakeBackend{}
	h.client = f
	req := completionsRequest{Messages: []message{{Role: "user", Content: strings.Repeat("x", 5000)}}}
	turns, err := h.buildTurns(req, req.Messages, toolReminder(req))
	if err == nil || len(turns) != 0 {
		t.Fatalf("unpasteable turn produced %d turns, err=%v", len(turns), err)
	}
	if !strings.Contains(err.Error(), "single-paste limit") {
		t.Fatalf("refusal does not name the paste limit: %v", err)
	}
	if len(f.prompts) != 0 {
		t.Fatalf("refused plan still submitted %d turns", len(f.prompts))
	}
}

// The HTTP path refuses with 400 and sends nothing, so the session is neither
// journaled nor left mid-submission.
func TestOversizedSingleMessageFailsFastWithoutSubmitting(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.maxChars = 20000
	f := &fakeBackend{}
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	req.Messages = []message{{Role: "user", Content: strings.Repeat("x", 40000)}}
	w := request(h, "too-big", req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "will not split a single message") {
		t.Fatalf("oversized message: %d %s", w.Code, w.Body.String())
	}
	if len(f.prompts) != 0 {
		t.Fatalf("nothing should be submitted, got %d", len(f.prompts))
	}
	if s := h.sessions["too-big"]; s != nil && (s.Dirty || s.Phase != "") {
		t.Fatalf("refusal left a dirty journal: %+v", s)
	}
}

// A long history is replayed in pasteable chunks instead of one giant paste:
// a single oversized paste is accepted by the composer and then produces no
// answer at all, which is what leaves a chat unverifiable.
func TestLongHistoryIsChunkedUnderThePasteLimit(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.maxChars = 200000
	h.maxPaste = 150000
	req := completionsRequest{Model: "chatgpt-web"}
	for range 3 {
		req.Messages = append(req.Messages,
			message{Role: "user", Content: strings.Repeat("q", 60000)},
			message{Role: "assistant", Content: strings.Repeat("a", 60000)})
	}
	req.Messages = append(req.Messages, message{Role: "user", Content: "next question"})
	turns, err := h.buildTurns(req, req.Messages, toolReminder(req))
	if err != nil {
		t.Fatalf("buildTurns: %v", err)
	}
	if len(turns) < 2 {
		t.Fatalf("history was pasted whole in %d turn(s)", len(turns))
	}
	for i, tr := range turns {
		if len(tr.prompt) > h.maxPaste {
			t.Fatalf("turn %d is %d bytes, over the %d-byte paste limit", i+1, len(tr.prompt), h.maxPaste)
		}
	}
	last := turns[len(turns)-1]
	if last.replay {
		t.Fatal("last turn must be the question, not a replay")
	}
	if !strings.Contains(last.prompt, "next question") {
		t.Fatal("last turn lost the question")
	}
}

// A large tool catalog is not chunkable, so it must not count against the
// paste budget: counting it blocked every fresh session with a big catalog
// with a 306312-byte refusal for a request that had no new content at all.
func TestLargeToolCatalogDoesNotCountAgainstPasteLimit(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.maxPaste = 150000
	req := weatherReq()
	head := strings.Repeat("S", 300000)
	turns, err := h.buildTurns(req, req.Messages, head)
	if err != nil {
		t.Fatalf("large catalog refused: %v", err)
	}
	if len(turns) != 1 || !strings.Contains(turns[0].prompt, "Call weather for Hanoi") {
		t.Fatalf("large catalog did not produce one answer turn: %d", len(turns))
	}
}

func TestAnswerlessTurnOnlyMatchesMissingAnswer(t *testing.T) {
	noAnswer := errors.New("ChatGPT generation ended without an extractable new assistant answer; inspect stored thread")
	if !answerlessTurn(noAnswer) {
		t.Fatal("missing answer must be recognised")
	}
	for _, other := range []error{
		nil,
		errors.New("send did not submit the turn"),
		errors.New("context canceled"),
	} {
		if answerlessTurn(other) {
			t.Fatalf("%v must not be treated as an answerless turn", other)
		}
	}
}

func TestReadinessReasonDistinct(t *testing.T) {
	for _, tc := range []struct {
		name   string
		st     *chatgpt.Status
		ready  bool
		reason string
	}{
		{"nil state is unknown", nil, false, "unknown"},
		{"challenge beats everything", &chatgpt.Status{Challenge: true, LoginBtn: true}, false, "challenge"},
		{"logged out", &chatgpt.Status{LoginBtn: true}, false, "logged-out"},
		{"blank loaded page", &chatgpt.Status{}, false, "blank"},
		{"ready", &chatgpt.Status{LoggedIn: true, Composer: true}, true, "ready"},
		{"rendered body without composer is not ready", &chatgpt.Status{LoggedIn: true, BodyLen: 500}, false, "not-ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready, reason, repair := readinessReason(tc.st)
			if ready != tc.ready || reason != tc.reason {
				t.Fatalf("got (%v,%q) want (%v,%q)", ready, reason, tc.ready, tc.reason)
			}
			if !ready && repair == "" {
				t.Fatal("unready state must name a repair")
			}
			if ready && repair != "" {
				t.Fatal("ready state must not carry a repair")
			}
		})
	}
}

func TestChallengeRefusalKeepsJournalClean(t *testing.T) {
	f := &fakeBackend{err: chatgpt.ErrPageChallenge}
	h := testHandler(f)
	w := request(h, "challenge", weatherReq())
	if w.Code != 422 || !strings.Contains(w.Body.String(), "human-verification") {
		t.Fatalf("challenge refusal: %d %s", w.Code, w.Body.String())
	}
	s := h.sessions["challenge"]
	if s == nil || s.Dirty || s.PendingPrompt != "" || s.Phase != "" {
		t.Fatalf("challenge must journal nothing: %+v", s)
	}
	if len(f.prompts) != 1 {
		t.Fatalf("challenge attempted %d submits, want exactly the refused one", len(f.prompts))
	}
}

func TestNearTailMismatchContinuesSameThread(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxPaste = 60000
	const threadURL = "https://chatgpt.com/c/thread-same"
	saved := make([]message, 0, 10)
	for i := 0; i < 5; i++ {
		saved = append(saved, message{Role: "user", Content: fmt.Sprintf("question %d", i)})
		saved = append(saved, message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	incoming := append([]message{}, saved...)
	incoming[9] = message{Role: "assistant", Content: "answer 4!"}
	incoming = append(incoming, message{Role: "user", Content: "follow up"})
	h.sessions["near-tail"] = &session{URL: threadURL, Messages: saved}
	h.sentLog = append(h.sentLog, sentEntry{Session: "near-tail", URL: threadURL, Event: "completed", PromptHash: "seed"})
	req := completionsRequest{Model: "chatgpt-web", Messages: incoming}
	w := request(h, "near-tail", req)
	if w.Code != 200 {
		t.Fatalf("near-tail continue: %d %s", w.Code, w.Body.String())
	}
	if len(f.prompts) != 1 || f.urls[0] != threadURL {
		t.Fatalf("must continue on the same thread once, got urls=%v", f.urls)
	}
	if got := h.sessions["near-tail"].URL; got != threadURL {
		t.Fatalf("thread moved: %s", got)
	}
	if len(h.sessions["near-tail"].Messages) != len(incoming)+1 {
		t.Fatalf("transcript not committed: %d", len(h.sessions["near-tail"].Messages))
	}
}

func TestToolsFPPinnedOnRetry(t *testing.T) {
	f := &fakeBackend{answers: []string{weatherAnswer, weatherAnswer}}
	h := testHandler(f)
	req := weatherReq()
	if w := request(h, "pin", req); w.Code != 200 {
		t.Fatalf("first turn: %d %s", w.Code, w.Body.String())
	}
	committed := append([]message{}, h.sessions["pin"].Messages...)
	req2 := weatherReq()
	req2.Messages = append(committed, message{Role: "user", Content: "another city"})
	if w := request(h, "pin", req2); w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if len(f.prompts) != 2 {
		t.Fatalf("prompts=%d", len(f.prompts))
	}
	if strings.Contains(f.prompts[1], "You are the text-generation backend") {
		t.Fatal("retry resent the full tool schemas for an unchanged tool set")
	}
	if !strings.Contains(f.prompts[1], "already taught") {
		t.Fatal("retry did not use the compact reminder")
	}
}

func TestFreshSystemDumpReplaysAsHistory(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.maxPaste = 60000
	req := completionsRequest{Model: "chatgpt-web", Messages: []message{
		{Role: "system", Content: strings.Repeat("S", 70000)},
		{Role: "user", Content: "answer this now"},
	}}
	turns, err := h.buildTurns(req, req.Messages, toolReminder(req))
	if err != nil {
		t.Fatalf("fresh dump refused: %v", err)
	}
	if len(turns) < 2 {
		t.Fatalf("dump landed whole in %d turn(s)", len(turns))
	}
	last := turns[len(turns)-1]
	if last.replay || !strings.Contains(last.prompt, "answer this now") {
		t.Fatal("last turn must be the question, not a replay")
	}
	for _, tr := range turns[:len(turns)-1] {
		if !tr.replay {
			t.Fatal("leading context must replay as history")
		}
	}
}

func TestReseedRecordCarriesDigestBeyondRecent(t *testing.T) {
	h := testHandler(&fakeBackend{})
	var carried []message
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("call-%d", i)
		carried = append(carried, message{Role: "assistant", ToolCalls: []toolCall{{ID: id, Type: "function", Function: callFunction{Name: "fetch", Arguments: fmt.Sprintf(`{"n":%d}`, i)}}}})
		carried = append(carried, message{Role: "tool", ToolCallID: id, Content: strings.Repeat(fmt.Sprintf("R%d-", i), 1500)})
	}
	verbatimAll := 0
	for _, m := range carried {
		verbatimAll += len(renderMsg(m))
	}
	turns := h.buildReseedRecordTurns(carried)
	if len(turns) == 0 {
		t.Fatal("no record turns")
	}
	var joined string
	total := 0
	for _, tr := range turns {
		if !tr.replay {
			t.Fatal("record turns must be inert replays")
		}
		joined += tr.prompt
		total += len(tr.prompt)
	}
	if total*2 >= verbatimAll {
		t.Fatalf("digest did not shrink the record: %d vs verbatim %d", total, verbatimAll)
	}
	if !strings.Contains(joined, "R9-") {
		t.Fatal("verbatim tail lost the most recent result")
	}
	if full := strings.Repeat("R0-", 1500); strings.Contains(joined, full) {
		t.Fatal("oldest result traveled verbatim instead of as a digest")
	}
	if !strings.Contains(joined, "-> result") {
		t.Fatal("digest lines missing")
	}
}

func TestRotationCardOverPasteBudgetFallsBackBeforeSubmit(t *testing.T) {
	f := &fakeBackend{err: errors.New("must not submit")}
	h := testHandler(f)
	h.maxPaste = 60000
	// Even the capped shrink rung (last 2 turns verbatim) exceeds the paste
	// budget here, so the rotation must fall back without submitting.
	msgs := make([]message, 0, 31)
	for i := 0; i < 31; i++ {
		msgs = append(msgs, message{Role: "user", Content: strings.Repeat("x", 40000)})
	}
	req := completionsRequest{Model: "chatgpt-web", Messages: msgs}
	s := &session{URL: "https://chatgpt.com/c/old", Messages: msgs[:len(msgs)-1]}
	w := httptest.NewRecorder()
	if h.serveRotation(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), "rotate-paste", "main", req, s, nil) {
		t.Fatal("oversized rotation card must fall back, not submit")
	}
	if len(f.prompts) != 0 {
		t.Fatal("oversized rotation card reached the composer")
	}
	if s.Dirty || s.PendingPrompt != "" || s.URL != "https://chatgpt.com/c/old" {
		t.Fatal("oversized rotation card dirtied the journal", s.Dirty, len(s.PendingPrompt))
	}
}

func TestRotationCappedCardShrinksInsteadOfFullReplay(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxPaste = 60000
	// The richest ladder rung exceeds the paste budget, but the capped shrink
	// rung (last 2 turns verbatim) fits: rotation submits one pasteable card
	// instead of falling back to a full chunked replay.
	msgs := make([]message, 0, 31)
	for i := 0; i < 30; i++ {
		msgs = append(msgs, message{Role: "user", Content: strings.Repeat("x", 5000)})
	}
	msgs = append(msgs, message{Role: "user", Content: "answer this now"})
	req := completionsRequest{Model: "chatgpt-web", Messages: msgs}
	s := &session{URL: "https://chatgpt.com/c/old", Messages: msgs[:len(msgs)-1]}
	w := httptest.NewRecorder()
	if !h.serveRotation(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), "rotate-capped", "main", req, s, nil) {
		t.Fatalf("capped rotation card must submit, status=%d", w.Code)
	}
	if len(f.prompts) != 1 {
		t.Fatalf("capped rotation must submit exactly one card, got %d", len(f.prompts))
	}
	head := toolPrompt(req)
	if len(f.prompts[0])-len(head) > h.pasteLimit() {
		t.Fatalf("capped card content %dB exceeds paste limit %d", len(f.prompts[0])-len(head), h.pasteLimit())
	}
	if !strings.Contains(f.prompts[0], "RECENT TURNS") || !strings.Contains(f.prompts[0], "answer this now") {
		t.Fatal("capped card lost the verbatim tail")
	}
	if w.Code != 200 {
		t.Fatalf("capped rotation did not complete: status=%d %s", w.Code, w.Body.String())
	}
}

func TestTitleTurnFailureLeavesJournalClean(t *testing.T) {
	f := &fakeBackend{err: errors.New("title answer never arrived"), errorURL: "https://chatgpt.com/c/title-1"}
	h := testHandler(f)
	req := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "Name this chat about lunch"}}}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(b))
	r.Header.Set("X-Session-Id", "title-clean")
	r.Header.Set("X-OpenCode-Request-Lane", "title")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("failing title turn must not succeed")
	}
	for id, sess := range h.sessions {
		if sess.Dirty || sess.PendingRequest != nil || sess.Phase != "" {
			t.Fatalf("title failure wedged journal %s dirty=%v phase=%q", id, sess.Dirty, sess.Phase)
		}
	}
}

func TestTitleCtxBoundsOnlyTitleLane(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	ctx, cancel := titleTimeout(r, "main")
	defer cancel()
	if ctx != r.Context() {
		t.Fatal("main lane must keep the request context untouched")
	}
	ctx, cancel = titleTimeout(r, "title")
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("title lane must carry a deadline")
	}
	if time.Until(dl) > 3*time.Minute+10*time.Second || time.Until(dl) <= 0 {
		t.Fatalf("title deadline out of range: %v", time.Until(dl))
	}
}

func TestPendingBubbleIDJournaledAndCleared(t *testing.T) {
	f := &fakeBackend{}
	f.turn = func(ctx context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
		f.prompts = append(f.prompts, opt.Prompt)
		return chatgpt.TurnResult{URL: "https://chatgpt.com/c/bound", BubbleID: "bubble-1"}, errors.New("answer never arrived")
	}
	h := testHandler(f)
	req := completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "name this chat about lunch"}}}
	if w := request(h, "bubb", req); w.Code == 200 {
		t.Fatal("failing final turn must not succeed")
	}
	s := h.sessions["bubb"]
	if s == nil || !s.Dirty {
		t.Fatal("post-submit failure must stay uncertain")
	}
	if s.PendingBubbleID != "bubble-1" {
		t.Fatalf("submit linkage not journaled: %q", s.PendingBubbleID)
	}
	if w := abandonPost(h, `{"sessions":["bubb"],"reason":"test clear"}`, ""); w.Code != 200 {
		t.Fatalf("abandon %d: %s", w.Code, w.Body.String())
	}
	if h.sessions["bubb"].PendingBubbleID != "" {
		t.Fatal("abandon must clear the submit linkage with the pending binding")
	}
}

func TestBoundBubbleErrorsAreNeverPreSubmit(t *testing.T) {
	for _, err := range []error{chatgpt.ErrBubbleGone, chatgpt.ErrPendingNeverStored} {
		if chatgpt.IsPreSubmit(err) {
			t.Fatalf("%v must not be pre-submit", err)
		}
		if errors.Is(err, chatgpt.ErrNotSubmitted) {
			t.Fatalf("%v must not look like a safe retry", err)
		}
	}
}

func TestTailTouchUpAtHighOffsetContinuesSameThread(t *testing.T) {
	// Live replica: 1530-byte saved assistant reply re-recorded with a
	// 7-char tail touch-up (first diff at offset 1530). Gating on offset
	// rotated this into a new thread plus a 39-turn reseed; gating on
	// divergence size continues on the same thread once.
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxPaste = 60000
	const threadURL = "https://chatgpt.com/c/thread-tail"
	saved := make([]message, 0, 10)
	for i := 0; i < 5; i++ {
		saved = append(saved, message{Role: "user", Content: fmt.Sprintf("question %d", i)})
		saved = append(saved, message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	saved[9] = message{Role: "assistant", Content: strings.Repeat("a", 1530)}
	incoming := append([]message{}, saved...)
	incoming[9] = message{Role: "assistant", Content: strings.Repeat("a", 1530) + "bbbbbbb"}
	incoming = append(incoming, message{Role: "user", Content: "implement it"})
	h.sessions["tail-touch"] = &session{URL: threadURL, Messages: saved}
	h.sentLog = append(h.sentLog, sentEntry{Session: "tail-touch", URL: threadURL, Event: "completed", PromptHash: "seed"})
	req := completionsRequest{Model: "chatgpt-web", Messages: incoming}
	w := request(h, "tail-touch", req)
	if w.Code != 200 {
		t.Fatalf("tail touch-up: %d %s", w.Code, w.Body.String())
	}
	if len(f.prompts) != 1 || f.urls[0] != threadURL {
		t.Fatalf("must continue on the same thread once, got urls=%v prompts=%d", f.urls, len(f.prompts))
	}
	if got := h.sessions["tail-touch"].URL; got != threadURL {
		t.Fatalf("thread moved: %s", got)
	}
}

func TestLargeRewriteRefusesDeltaContinue(t *testing.T) {
	// Same position, opposite shape: a 5KB replacement diverging near
	// offset 0 must keep the reseed/rotation path, never delta-continue.
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxPaste = 60000
	const threadURL = "https://chatgpt.com/c/thread-rewrite"
	saved := make([]message, 0, 10)
	for i := 0; i < 5; i++ {
		saved = append(saved, message{Role: "user", Content: fmt.Sprintf("question %d", i)})
		saved = append(saved, message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	incoming := append([]message{}, saved...)
	incoming[9] = message{Role: "assistant", Content: "CHANGED:" + strings.Repeat("z", 5000)}
	incoming = append(incoming, message{Role: "user", Content: "implement it"})
	h.sessions["big-rewrite"] = &session{URL: threadURL, Messages: saved}
	h.sentLog = append(h.sentLog, sentEntry{Session: "big-rewrite", URL: threadURL, Event: "completed", PromptHash: "seed"})
	req := completionsRequest{Model: "chatgpt-web", Messages: incoming}
	w := request(h, "big-rewrite", req)
	if w.Code != 200 {
		t.Fatalf("large rewrite: %d %s", w.Code, w.Body.String())
	}
	if len(f.prompts) == 1 && len(f.urls) == 1 && f.urls[0] == threadURL {
		t.Fatal("large rewrite must not delta-continue on the same thread")
	}
}

func TestCompactionAppendStaysOnThread(t *testing.T) {
	// Live replica: 20-message transcript, client swaps the last assistant
	// reply for a 70KB compaction summary and appends one user message.
	// Old path minted a new thread plus a ~40-turn full replay; the tail
	// now appends onto the current thread chunked (2 imports + final).
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxPaste = 60000
	const threadURL = "https://chatgpt.com/c/thread-compact"
	saved := []message{{Role: "system", Content: "You are a helper. Previously compacted."}}
	for i := 0; i < 9; i++ {
		saved = append(saved, message{Role: "user", Content: fmt.Sprintf("question %d", i)})
		saved = append(saved, message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	saved = append(saved, message{Role: "assistant", Content: "last short answer"})
	incoming := append([]message{}, saved...)
	incoming[len(incoming)-1] = message{Role: "assistant", Content: "SUMMARY:" + strings.Repeat("s", 70000)}
	incoming = append(incoming, message{Role: "user", Content: "go on"})
	h.sessions["compact"] = &session{URL: threadURL, Messages: saved}
	h.sentLog = append(h.sentLog, sentEntry{Session: "compact", URL: threadURL, Event: "completed", PromptHash: "seed"})
	req := completionsRequest{Model: "chatgpt-web", Messages: incoming}
	w := request(h, "compact", req)
	if w.Code != 200 {
		t.Fatalf("compaction append: %d %s", w.Code, w.Body.String())
	}
	if len(f.prompts) != 3 {
		t.Fatalf("summary must arrive chunked plus final, got %d prompts", len(f.prompts))
	}
	for _, u := range f.urls {
		if u != threadURL {
			t.Fatalf("must stay on the same thread, got urls=%v", f.urls)
		}
	}
	if got := h.sessions["compact"].URL; got != threadURL {
		t.Fatalf("thread moved: %s", got)
	}
}

func TestPacingBeforeTurnShapesBurst(t *testing.T) {
	if got := pacingBeforeTurn(0, 5); got != 0 {
		t.Fatalf("first turn must never wait: %v", got)
	}
	if got := pacingBeforeTurn(3, 0); got != 0 {
		t.Fatalf("disabled pacing must never wait: %v", got)
	}
	if got := pacingBeforeTurn(2, 5); got != 5*time.Second {
		t.Fatalf("later turns must wait the configured think-time: %v", got)
	}
}
