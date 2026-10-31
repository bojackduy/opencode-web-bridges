package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojackduy/gemini-proxy/internal/gemini"
)

// TestMain silences the adapter's content-free logging so a failing assertion
// is the first thing a truncated check log shows.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// fakeBackend answers Ask without a browser.
type fakeBackend struct{}

func TestBestEffortPendingContinuationWithoutReadback(t *testing.T) {
	for _, hasHistory := range []bool{false, true} {
		name := "empty_committed_history"
		if hasHistory {
			name = "committed_history"
		}
		t.Run(name, func(t *testing.T) {
			reads := 0
			fake := &fakeRecoveryBackend{askAnswer: "fresh answer", urlNow: "https://gemini.google.com/app/NEW"}
			fake.answerFor = func(prompt string) string {
				if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
					return "ACK"
				}
				return "fresh answer"
			}
			fake.readTurn = func(string, string) (gemini.TurnResult, error) {
				reads++
				return gemini.TurnResult{}, structuralMismatch()
			}
			fake.readLatest = func(string) (string, gemini.TurnResult, error) {
				reads++
				return "", gemini.TurnResult{}, errors.New("unavailable")
			}
			pending := []message{{Role: "user", Content: "uncertain request"}}
			stored := []message(nil)
			if hasHistory {
				stored = []message{{Role: "user", Content: "old question"}, {Role: "assistant", Content: "old answer"}}
				pending = append(append([]message(nil), stored...), pending...)
			}
			h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
				sessions: map[string]*session{"s": {Messages: stored, Pending: true, PendingPrompt: "pending prompt", PendingMessages: pending, URL: "https://gemini.google.com/app/OLD"}}}
			h.gate <- struct{}{}
			incoming := append(append([]message(nil), pending...), message{Role: "user", Content: "continue with this new request"})
			w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
			// One bounded adopt attempt (structural refusal), then reseed
			// fallback: reads happen, resubmission never does.
			if reads != 1 || w.Code != http.StatusOK || fake.asks != 2 || len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
				t.Fatalf("new input blocked: reads=%d status=%d asks=%d fresh=%v", reads, w.Code, fake.asks, fake.freshFlags)
			}
			if !strings.Contains(fake.gotPrompt, "continue with this new request") {
				t.Fatal("new message missing from fresh seed")
			}
		})
	}
}

func TestBestEffortIdenticalPendingRetryNeverSubmits(t *testing.T) {
	reads := 0
	fake := &fakeRecoveryBackend{askAnswer: "must not send"}
	fake.readTurn = func(string, string) (gemini.TurnResult, error) {
		reads++
		return gemini.TurnResult{}, structuralMismatch()
	}
	fake.readLatest = func(string) (string, gemini.TurnResult, error) {
		reads++
		return "", gemini.TurnResult{}, errors.New("unavailable")
	}
	pending := []message{{Role: "user", Content: "uncertain request"}}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		sessions: map[string]*session{"s": {Pending: true, PendingPrompt: "pending prompt", PendingMessages: pending, URL: "https://gemini.google.com/app/OLD"}}}
	h.gate <- struct{}{}
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: pending})
	if w.Code == http.StatusOK || fake.asks != 0 || reads != 0 {
		t.Fatalf("uncertain retry sent or implicitly recovered: status=%d asks=%d reads=%d", w.Code, fake.asks, reads)
	}
}

func TestReadyBusyAndPendingAreNotAuthFailures(t *testing.T) {
	newHandler := func(client browserClient, sessions map[string]*session) *handler {
		h := &handler{model: "gemini-web", client: client, gate: make(chan struct{}, 1), sessions: sessions}
		h.gate <- struct{}{}
		return h
	}
	readyz := func(h *handler, sid string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/readyz", nil)
		if sid != "" {
			r.Header.Set("x-opencode-session-id", sid)
		}
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		return w
	}

	t.Run("busy gate is unavailable, never an auth failure", func(t *testing.T) {
		h := &handler{model: "gemini-web", client: &fakeReadyBackend{}, gate: make(chan struct{}, 1), sessions: map[string]*session{}}
		// Empty gate: a browser turn holds it.
		w := readyz(h, "")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "browser turn busy") || strings.Contains(w.Body.String(), "not logged in") {
			t.Fatalf("busy: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("one pending session does not report the provider unready", func(t *testing.T) {
		h := newHandler(&fakeReadyBackend{}, map[string]*session{"stuck": {Pending: true}})
		w := readyz(h, "")
		if w.Code != http.StatusOK {
			t.Fatalf("a pending session must not brick the provider: %d %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["ready"] != true || body["pending"] != float64(1) || !strings.Contains(w.Body.String(), "stuck") {
			t.Fatalf("readiness must count and name the pending identities: %s", w.Body.String())
		}
	})

	t.Run("the pending identity itself reports not-ready", func(t *testing.T) {
		h := newHandler(&fakeReadyBackend{}, map[string]*session{"stuck": {Pending: true}, "live": {}})
		w := readyz(h, "stuck")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "pending uncertain turn; inspect owned chat") {
			t.Fatalf("pending identity: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "not logged in") {
			t.Fatalf("pending must never be reported as an auth failure: %s", w.Body.String())
		}
		if w := readyz(h, "live"); w.Code != http.StatusOK {
			t.Fatalf("a clean identity stays ready while another is pending: %d %s", w.Code, w.Body.String())
		}
		// Journal truth outranks a transient busy gate.
		busy := newHandler(&fakeReadyBackend{}, map[string]*session{"stuck": {Pending: true}})
		<-busy.gate
		if w := readyz(busy, "stuck"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "pending uncertain turn") {
			t.Fatalf("pending identity must beat a busy gate: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("a logged-out tab still discloses the pending count", func(t *testing.T) {
		h := newHandler(&fakeRecoveryBackend{}, map[string]*session{"stuck": {Pending: true}})
		w := readyz(h, "")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"pending":1`) {
			t.Fatalf("not-ready must never hide pending sessions: %d %s", w.Code, w.Body.String())
		}
	})
}

func (f *fakeBackend) Ask(_ interface{}, _ string, _ bool, _ interface{}) (string, error) {
	return "", nil
}

func TestSessionIDPriority(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Session-Affinity", "aff")
	r.Header.Set("X-Opencode-Session", "opencode")
	r.Header.Set("X-Session-Id", "primary")
	r.Header.Set("X-Opencode-Session-Id", "child")
	if got := sessionID(r); got != "child" {
		t.Fatalf("v2 child identity priority: got %q", got)
	}
	if got := readySessionID(r); got != "child" {
		t.Fatalf("readiness and completions must agree on identity: got %q", got)
	}
	r.Header.Del("X-Opencode-Session-Id")
	if got := sessionID(r); got != "primary" {
		t.Fatalf("priority: got %q", got)
	}
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set("X-Session-Affinity", "aff")
	if got := sessionID(r2); got != "aff" {
		t.Fatalf("fallback: got %q", got)
	}
}

func TestCanonicalEqualIgnoresArgEncoding(t *testing.T) {
	a := message{Role: "assistant", ToolCalls: []toolCall{{ID: "1", Function: callFunction{Name: "f", Arguments: `{"a":1,"b":2}`}}}}
	b := message{Role: "assistant", ToolCalls: []toolCall{{ID: "1", Function: callFunction{Name: "f", Arguments: `{"b":2,"a":1}`}}}}
	if !canonicalEqual(a, b) {
		t.Fatal("equivalent argument encodings should compare equal")
	}
	c := message{Role: "assistant", ToolCalls: []toolCall{{ID: "1", Function: callFunction{Name: "f", Arguments: `{"a":1}`}}}}
	if canonicalEqual(a, c) {
		t.Fatal("different arguments must not compare equal")
	}
}

func TestEffectiveToolChoice(t *testing.T) {
	if got := effectiveToolChoice(nil, 0); got != "none" {
		t.Fatalf("no tools: %q", got)
	}
	if got := effectiveToolChoice(nil, 2); got != "auto" {
		t.Fatalf("nil choice: %q", got)
	}
	if got := effectiveToolChoice("required", 1); got != "required" {
		t.Fatalf("string choice: %q", got)
	}
	raw, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "f"}})
	var v any
	_ = json.Unmarshal(raw, &v)
	if got := effectiveToolChoice(v, 1); got != "f" {
		t.Fatalf("forced choice: %q", got)
	}
}

func TestFormatForBrowserQuotesHistory(t *testing.T) {
	m := message{Role: "assistant", ToolCalls: []toolCall{{ID: "x", Function: callFunction{Name: "f", Arguments: "{}"}}}}
	got := formatForBrowser(m)
	if !strings.Contains(got, "inert") {
		t.Fatalf("history tool calls must be labeled inert: %q", got)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := &handler{model: "gemini-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
}

func TestHealthz(t *testing.T) {
	h := &handler{model: "gemini-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

func TestEmptyMessagesRejected(t *testing.T) {
	h := &handler{model: "gemini-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-web","messages":[]}`))
	rec := httptest.NewRecorder()
	h.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestPendingTurnNeverCallsBrowser pins the per-session guard: a pending
// record with no recoverable binding at all (no pending prompt) is refused
// fast, with no browser read, no submission and no state change. The fake's
// recovery readers are nil, so any read attempt would panic here.
func TestPendingTurnNeverCallsBrowser(t *testing.T) {
	fake := &fakeRecoveryBackend{}
	h := &handler{model: "gemini-web", client: fake, sessions: map[string]*session{"s": {Pending: true}}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hello"}}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pending must fail closed non-retryably: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pending uncertain turn; inspect owned chat") {
		t.Fatalf("refusal must name the state: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "POST /v1/sessions/abandon") {
		t.Fatalf("refusal must name the operator path for an unresolvable record: %s", w.Body.String())
	}
	if fake.asks != 0 {
		t.Fatalf("a pending session must never submit: asks=%d", fake.asks)
	}
	if s := h.sessions["s"]; !s.Pending || s.PendingPrompt != "" || len(s.Messages) != 0 {
		t.Fatalf("refusal must leave the session untouched: %+v", s)
	}
}

// TestPendingSessionDoesNotBrickOthers is the anti-brick proof: an uncertain
// identity refuses only itself while a different identity completes normally.
func TestPendingSessionDoesNotBrickOthers(t *testing.T) {
	fake := &fakeRecoveryBackend{askAnswer: "other session fine"}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "openai-sessions.json"),
		sessions:  map[string]*session{"stuck": {Pending: true}}}
	h.gate <- struct{}{}
	w := postJSON(t, h, "live", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hello"}}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "other session fine") {
		t.Fatalf("a pending session must not brick another identity: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 1 {
		t.Fatalf("the other identity must submit exactly once: asks=%d", fake.asks)
	}
	if s := h.sessions["stuck"]; !s.Pending || s.PendingPrompt != "" || len(s.Messages) != 0 {
		t.Fatalf("the pending identity must be untouched by another turn: %+v", s)
	}
}

// TestAbandonClearsOnlyNamedSessions pins the operator path: an explicit,
// named, gated clear that drops only the pending binding — never a wildcard,
// never automatic, and byte-identical for every session it did not name.
func TestAbandonClearsOnlyNamedSessions(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "openai-sessions.json")
	h := &handler{model: "gemini-web", statePath: state, maxChars: 200000, gate: make(chan struct{}, 1), sessions: map[string]*session{}}
	h.gate <- struct{}{}
	h.sessions["stuck"] = &session{
		Pending: true, PendingPrompt: "legacy prompt", URL: "https://gemini.google.com/app/abc",
		PendingMessages: []message{{Role: "user", Content: "legacy"}}, PendingTools: weatherTools(),
		PendingChoice: "auto", PendingReqHash: "pending-hash",
		Messages: []message{{Role: "user", Content: "kept"}}, LastAnswer: "kept answer",
		LastReqHash: "keep-hash", LastRespID: "keep-id", LastToolIDs: []string{"call_keep"},
	}
	h.sessions["other"] = &session{
		Pending: true, PendingPrompt: "other prompt", URL: "https://gemini.google.com/app/xyz",
		PendingMessages: []message{{Role: "user", Content: "other"}},
		Messages:        []message{{Role: "user", Content: "other kept"}},
	}
	h.saveLocked()
	rawOther := func() string {
		t.Helper()
		b, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		var file map[string]json.RawMessage
		if err := json.Unmarshal(b, &file); err != nil {
			t.Fatal(err)
		}
		return string(file["other"])
	}
	before := rawOther()

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/abandon", strings.NewReader(body)))
		return w
	}

	// Refusals: nothing named, a wildcard, a mixed list, and a non-POST verb.
	for _, body := range []string{
		`{"sessions":[],"reason":"x"}`,
		`{"sessions":["*"]}`,
		`{"sessions":["all"]}`,
		`{"sessions":["stuck",""]}`,
		`{"sessions":["st*ck"]}`,
	} {
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %s must be refused whole: %d %s", body, w.Code, w.Body.String())
		}
	}
	wg := httptest.NewRecorder()
	h.routes().ServeHTTP(wg, httptest.NewRequest("GET", "/v1/sessions/abandon", nil))
	if wg.Code != http.StatusMethodNotAllowed {
		t.Fatalf("abandon must be POST only: %d", wg.Code)
	}
	crossOrigin := httptest.NewRequest("POST", "/v1/sessions/abandon", strings.NewReader(`{"sessions":["stuck"]}`))
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	blocked := httptest.NewRecorder()
	h.routes().ServeHTTP(blocked, crossOrigin)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("browser-originated abandon must be refused: %d %s", blocked.Code, blocked.Body.String())
	}
	if s := h.sessions["stuck"]; !s.Pending || s.PendingPrompt != "legacy prompt" {
		t.Fatalf("a refused request must clear nothing: %+v", s)
	}

	// If persistence fails, the endpoint must not report success or leave
	// an in-memory clear that disappears after restart.
	failedSave := &handler{
		model: "gemini-web", statePath: t.TempDir(),
		gate:     make(chan struct{}, 1),
		sessions: map[string]*session{"stuck": {Pending: true, PendingPrompt: "keep pending"}},
	}
	failedSave.gate <- struct{}{}
	wf := httptest.NewRecorder()
	failedSave.routes().ServeHTTP(wf, httptest.NewRequest("POST", "/v1/sessions/abandon", strings.NewReader(`{"sessions":["stuck"]}`)))
	if wf.Code != http.StatusInternalServerError || !failedSave.sessions["stuck"].Pending || failedSave.sessions["stuck"].PendingPrompt != "keep pending" {
		t.Fatalf("failed persistence must roll back the pending binding: %d %s", wf.Code, wf.Body.String())
	}

	// A busy gate refuses too: the operator path never races a browser turn.
	busy := &handler{model: "gemini-web", gate: make(chan struct{}, 1), sessions: map[string]*session{"stuck": {Pending: true}}}
	wb := httptest.NewRecorder()
	busy.routes().ServeHTTP(wb, httptest.NewRequest("POST", "/v1/sessions/abandon", strings.NewReader(`{"sessions":["stuck"]}`)))
	if wb.Code != http.StatusServiceUnavailable || !strings.Contains(wb.Body.String(), "browser turn busy") {
		t.Fatalf("busy gate must refuse without clearing: %d %s", wb.Code, wb.Body.String())
	}
	if !busy.sessions["stuck"].Pending {
		t.Fatal("a busy refusal must clear nothing")
	}

	// The explicit clear: one named pending session plus one unknown id.
	w := post(`{"sessions":["stuck","missing"],"reason":"legacy record no code path can resolve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("abandon: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Requested int `json:"requested"`
		Abandoned int `json:"abandoned"`
		Results   []struct {
			Session string `json:"session"`
			Outcome string `json:"outcome"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Requested != 2 || out.Abandoned != 1 || len(out.Results) != 2 ||
		out.Results[0].Session != "stuck" || out.Results[0].Outcome != "abandoned" ||
		out.Results[1].Session != "missing" || out.Results[1].Outcome != "unknown" {
		t.Fatalf("every named session needs its own outcome: %s", w.Body.String())
	}

	s := h.sessions["stuck"]
	if s.Pending || s.PendingPrompt != "" || len(s.PendingMessages) != 0 || len(s.PendingTools) != 0 || s.PendingChoice != nil || s.PendingReqHash != "" {
		t.Fatalf("abandon must clear the whole pending binding: %+v", s)
	}
	if s.URL != "https://gemini.google.com/app/abc" || len(s.Messages) != 1 || s.Messages[0].Content != "kept" ||
		s.LastAnswer != "kept answer" || s.LastReqHash != "keep-hash" || s.LastRespID != "keep-id" || len(s.LastToolIDs) != 1 {
		t.Fatalf("abandon must retain transcript, URL, cache identity, result and tools: %+v", s)
	}
	if o := h.sessions["other"]; !o.Pending || o.PendingPrompt != "other prompt" {
		t.Fatalf("only named sessions may change: %+v", o)
	}
	if after := rawOther(); after != before {
		t.Fatalf("unnamed sessions must stay byte-identical:\nbefore %s\nafter  %s", before, after)
	}

	// Durable: a restart loads the cleared binding and the untouched neighbour.
	restarted := &handler{statePath: state, sessions: map[string]*session{}}
	if err := restarted.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rs := restarted.sessions["stuck"]; rs == nil || rs.Pending || rs.PendingPrompt != "" {
		t.Fatalf("abandon must be durable: %+v", rs)
	}
	if ro := restarted.sessions["other"]; ro == nil || !ro.Pending {
		t.Fatalf("unnamed pending sessions must stay pending on disk: %+v", ro)
	}

	// The abandoned identity serves requests again (nothing was cleared but
	// the pending binding, so its transcript is still usable).
	fake := &fakeRecoveryBackend{askAnswer: "resumed"}
	h.client = fake
	w2 := postJSON(t, h, "stuck", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "continued"}}})
	if w2.Code != http.StatusOK || fake.asks != 1 {
		t.Fatalf("an abandoned session must serve again: %d asks=%d %s", w2.Code, fake.asks, w2.Body.String())
	}
}

func TestExactCacheNeverCallsBrowser(t *testing.T) {
	req := chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hello"}}}
	h := &handler{model: "gemini-web", sessions: map[string]*session{"s": {LastReqHash: reqHash(&req), LastRespID: "cached", LastAnswer: "hello", LastCreated: 1}}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	raw, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
	r.Header.Set("X-Session-Id", "s")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"cached"`) {
		t.Fatalf("cache failed: %d %s", w.Code, w.Body.String())
	}
}

func TestPromptHashUsesAdoptionNormalization(t *testing.T) {
	if got := promptHash("hello\r\nworld"); got != gemini.PromptHash("hello\nworld") {
		t.Fatal("CRLF must hash like LF: the sent-log and adoption use one normalization")
	}
	if got := promptHash("  hello  "); got != promptHash("hello") {
		t.Fatal("surrounding whitespace must be insignificant")
	}
	if promptHash("hello") == promptHash("world") {
		t.Fatal("distinct prompts must hash distinctly")
	}
}

func TestSentDiagnosisLines(t *testing.T) {
	h := &handler{sessions: map[string]*session{}}
	// Unknown: pending with no recorded submission.
	s := &session{Pending: true, PendingPrompt: "do work"}
	if got := h.sentDiagnosis("s", s); !strings.HasPrefix(got, "sent-log:") || !strings.Contains(got, "no submission recorded") {
		t.Fatalf("unknown: %q", got)
	}
	// Consistent: last submit equals the pending prompt.
	h.appendSent(sentEntry{Event: "submitted", Session: "s", URL: "https://gemini.google.com/app/abc", PromptHash: promptHash("do work"), Bytes: 7, Chunk: 1, Chunks: 1})
	s.URL = "https://gemini.google.com/app/abc"
	if got := h.sentDiagnosis("s", s); !strings.Contains(got, "consistent") || !strings.Contains(got, "==") {
		t.Fatalf("consistent: %q", got)
	}
	// MISMATCH: pending overwritten by a retry the thread never saw.
	s.PendingPrompt = "newer retry"
	if got := h.sentDiagnosis("s", s); !strings.Contains(got, "MISMATCH") || !strings.Contains(got, "pending-overwritten-by-retry") {
		t.Fatalf("mismatch: %q", got)
	}
	// Clean: no pending prompt, last submit reported.
	clean := &session{}
	if got := h.sentDiagnosis("s", clean); !strings.Contains(got, "session clean") || !strings.Contains(got, "last submit") {
		t.Fatalf("clean: %q", got)
	}
	// Clean with nothing recorded.
	h2 := &handler{sessions: map[string]*session{}}
	if got := h2.sentDiagnosis("fresh", &session{}); !strings.Contains(got, "no submission recorded") {
		t.Fatalf("clean-empty: %q", got)
	}
}

func TestPendingRefusalCarriesDiagnosis(t *testing.T) {
	h := &handler{model: "gemini-web",
		sessions: map[string]*session{"s": {Pending: true, PendingPrompt: "newer retry", URL: "https://gemini.google.com/app/abc"}},
		gate:     make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.appendSent(sentEntry{Event: "submitted", Session: "s", URL: "https://gemini.google.com/app/abc", PromptHash: promptHash("older submission"), Bytes: 16, Chunk: 1, Chunks: 1})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-web","messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("X-Session-Id", "s")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pending must fail closed non-retryably: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sent-log: MISMATCH") {
		t.Fatalf("every pending refusal must carry the sent-log diagnosis line: %s", w.Body.String())
	}
}

func TestSentLogSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "openai-sessions.json")
	h := &handler{statePath: state, sessions: map[string]*session{}}
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/abc", PromptHash: promptHash("do work"), Bytes: 7, Chunk: 1, Chunks: 1, AnswerHash: gemini.AnswerHash("done")})
	// A restarted adapter loads the journal but not the memory ring.
	restarted := &handler{statePath: state, sessions: map[string]*session{}}
	last := restarted.lastSentFor("s")
	if last == nil || last.Event != "completed" || last.AnswerHash != gemini.AnswerHash("done") {
		t.Fatalf("durable tail must survive restart: %+v", last)
	}
	got := restarted.sentDiagnosis("s", &session{Pending: true, PendingPrompt: "do work", URL: "https://gemini.google.com/app/abc"})
	if !strings.Contains(got, "consistent") {
		t.Fatalf("post-restart diagnosis must work from the file tail: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent-log.jsonl")); err != nil {
		t.Fatalf("sent-log must live alongside the state file: %v", err)
	}
}

func TestOversizeTurnNeverSubmitsOrLogs(t *testing.T) {
	h := &handler{model: "gemini-web", maxChars: 10, sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	big := strings.Repeat("x", 100)
	raw, _ := json.Marshal(chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: big}}})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
	r.Header.Set("X-Session-Id", "s")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize must be 413: %d %s", w.Code, w.Body.String())
	}
	// Documented chunking behavior: the submit path never splits. An
	// over-budget turn is rejected whole before any submission intent
	// exists, so no sent-log entry may record it and the session must
	// hold no uncertain pending state (a wrapper can never be cut
	// mid-turn because turns are never cut at all).
	if last := h.lastSentFor("s"); last != nil {
		t.Fatalf("rejected oversize turn must not log a submission: %+v", last)
	}
	if s := h.sessions["s"]; s != nil && (s.Pending || s.PendingPrompt != "") {
		t.Fatalf("rejected oversize turn must leave no pending state: %+v", s)
	}
}

// fakeRecoveryBackend is a read-only thread double: it answers recovery
// reads without a browser and counts Ask calls, which must stay zero —
// recovery records answers, never submits or executes.
type fakeRecoveryBackend struct {
	asks       int
	reads      int
	latests    int
	readTurn   func(url, prompt string) (gemini.TurnResult, error)
	readLatest func(url string) (string, gemini.TurnResult, error)
	// Ask double: records the submission for reseed/happy-path tests.
	askAnswer string
	askErr    error
	gotPrompt string
	gotFresh  bool
	urlNow    string
	// Multi-turn recording for chunking tests: every prompt and fresh
	// flag in order. answerFor, when set, answers per prompt.
	prompts    []string
	freshFlags []bool
	answerFor  func(prompt string) string
	// Thread-binding simulation: boundURLs records the bound target per
	// thread-aware submit; mintURL is adopted as urlNow on fresh submits
	// (new-chat mint); restoreTo is adopted on bound submits (rebuild
	// routed back); ensureErr fails bound submits with zero sends.
	// dropToHomeAfter simulates a tab rebuild between chunks: when set,
	// every continuation submit finds the tab freshly rebuilt on the
	// homepage and must be routed back via restoreTo before sending.
	boundURLs       []string
	mintURL         string
	restoreTo       string
	ensureErr       error
	dropToHomeAfter int
}

func (f *fakeRecoveryBackend) Ask(_ context.Context, prompt string, newSession bool, _ time.Duration) (string, error) {
	f.asks++
	f.gotPrompt = prompt
	f.gotFresh = newSession
	f.prompts = append(f.prompts, prompt)
	f.freshFlags = append(f.freshFlags, newSession)
	if f.askErr != nil {
		return "", f.askErr
	}
	if f.answerFor != nil {
		return f.answerFor(prompt), nil
	}
	return f.askAnswer, nil
}

func (f *fakeRecoveryBackend) CurrentURL(_ context.Context) string { return f.urlNow }

func (f *fakeRecoveryBackend) LoginState(_ context.Context) (*gemini.Status, error) {
	return nil, errors.New("unused in recovery tests")
}

func (f *fakeRecoveryBackend) ReadThreadTurn(_ context.Context, url, prompt string) (gemini.TurnResult, error) {
	f.reads++
	return f.readTurn(url, prompt)
}

func (f *fakeRecoveryBackend) ReadLatestThreadTurn(_ context.Context, url string) (string, gemini.TurnResult, error) {
	f.latests++
	return f.readLatest(url)
}

// Thread binding double: records the bound target per submit and enforces
// it like the real client. restoreTo simulates a tab rebuild routed back:
// when set, binding calls move urlNow to restoreTo (the saved thread)
// before submitting. ensureErr simulates a failed restore: binding calls
// return it with zero submits.
func (f *fakeRecoveryBackend) AskWithImagesThread(_ context.Context, prompt, threadURL string, newSession bool, _ time.Duration) (string, []gemini.ResponseImage, error) {
	f.boundURLs = append(f.boundURLs, threadURL)
	if newSession {
		// Fresh chats mint their thread; later chunks bind the minted URL.
		if f.mintURL != "" {
			f.urlNow = f.mintURL
		}
		s, err := f.Ask(context.Background(), prompt, true, 0)
		return s, nil, err
	}
	if f.ensureErr != nil {
		return "", nil, f.ensureErr
	}
	if !gemini.ValidThreadURL(threadURL) {
		return "", nil, threadBindingErr()
	}
	// A tab rebuilt between chunks sits on the homepage at submit time;
	// the binding routes it back before anything is sent.
	if f.dropToHomeAfter > 0 {
		f.urlNow = geminiHomePage()
	}
	// A rebuilt tab (homepage) is routed back before any submit.
	if f.restoreTo != "" {
		f.urlNow = f.restoreTo
	}
	if threadURL != f.urlNow {
		return "", nil, threadBindingErr()
	}
	s, err := f.Ask(context.Background(), prompt, false, 0)
	return s, nil, err
}

func (f *fakeRecoveryBackend) AskThread(_ context.Context, prompt, threadURL string, newSession bool, d time.Duration) (string, error) {
	s, _, err := f.AskWithImagesThread(context.Background(), prompt, threadURL, newSession, d)
	return s, err
}

func threadBindingErr() error {
	return fmt.Errorf("thread binding wrong-conversation: %w", gemini.ErrThreadBinding)
}

// fakeReadyBackend reports a signed-in tab so readiness paths can be exercised
// without a browser. Only the login probe differs from fakeRecoveryBackend.
type fakeReadyBackend struct{ fakeRecoveryBackend }

func (f *fakeReadyBackend) LoginState(context.Context) (*gemini.Status, error) {
	return &gemini.Status{LoggedIn: true, Composer: true}, nil
}

func structuralMismatch() *gemini.UserBubbleMismatchError {
	return &gemini.UserBubbleMismatchError{Path: "user-query", Rendered: 5000, Want: 60, DiffOff: 12, Spans: "12-4012/12-4012"}
}

func weatherTools() []toolDef {
	return []toolDef{{Type: "function", Function: toolFunction{
		Name: "get_weather", Description: "weather",
		Parameters: map[string]any{"type": "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}}},
	}}}
}

func TestSpansStructural(t *testing.T) {
	if spansStructural("") {
		t.Fatal("empty evidence must fail closed, not structural")
	}
	if spansStructural("моха") {
		t.Fatal("unparseable evidence must fail closed, not structural")
	}
	if !spansStructural("12-4012/12-4012") {
		t.Fatal("multi-KB interior divergence is structural")
	}
	if !spansStructural("0-10/0-10,500-900/500-900") {
		t.Fatal("scattered spans are structural")
	}
	if !spansStructural("4096+rest/60+rest") {
		t.Fatal("anchors that never re-converge are structural")
	}
	if spansStructural("120-148/120-150") {
		t.Fatal("one small span that re-converges is a bounded difference, not structural")
	}
}

func TestMismatchMessageContract(t *testing.T) {
	m := structuralMismatch()
	if !strings.Contains(m.Error(), "does not exactly match") {
		t.Fatalf("mismatch text must keep the deterministic() contract: %q", m.Error())
	}
}

func TestAdoptInterruptedClearsDirtyAndServesCache(t *testing.T) {
	req := chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hello"}}}
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			if url != "https://gemini.google.com/app/abc" || prompt != "SUBMITTED-PROMPT" {
				return gemini.TurnResult{}, structuralMismatch()
			}
			return gemini.TurnResult{Answer: "adopted answer", URL: url}, nil
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "", gemini.TurnResult{}, errors.New("unused on the adoption path")
		},
	}
	h := &handler{model: "gemini-web", client: fake, sessions: map[string]*session{
		"s": {Pending: true, PendingPrompt: "SUBMITTED-PROMPT", URL: "https://gemini.google.com/app/abc",
			PendingMessages: req.Messages, PendingReqHash: reqHash(&req)},
	}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if !h.recoverPending(r, "s") {
		t.Fatal("exact thread answer must adopt")
	}
	s := h.sessions["s"]
	if s.Pending || s.PendingPrompt != "" || len(s.PendingMessages) != 0 {
		t.Fatalf("adoption must clear Dirty: %+v", s)
	}
	if s.LastAnswer != "adopted answer" || s.LastReqHash != reqHash(&req) || s.LastRespID == "" {
		t.Fatalf("adoption must record the completed turn for duplicate-cache: %+v", s)
	}
	if len(s.Messages) != len(req.Messages)+1 || s.Messages[len(s.Messages)-1].Content != "adopted answer" {
		t.Fatalf("adoption must extend the transcript: %+v", s.Messages)
	}
	if fake.asks != 0 {
		t.Fatalf("recovery must never submit: asks=%d", fake.asks)
	}
	// The exact retry now hits the duplicate cache end to end.
	raw, _ := json.Marshal(req)
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
	r2.Header.Set("X-Session-Id", "s")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r2)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "adopted answer") {
		t.Fatalf("post-adoption retry must serve the cache: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 0 {
		t.Fatalf("duplicate cache must not submit either: asks=%d", fake.asks)
	}
}

func TestAutoResyncTransplantsClearCutShape(t *testing.T) {
	answer := `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, structuralMismatch()
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "ORIGINAL SUBMISSION", gemini.TurnResult{Answer: answer, URL: url}, nil
		},
	}
	h := &handler{model: "gemini-web", client: fake, sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	pending := []message{{Role: "user", Content: "newer retry"}}
	h.sessions["s"] = &session{Pending: true, PendingPrompt: "NEWER RETRY PROMPT", URL: "https://gemini.google.com/app/abc",
		PendingMessages: pending, PendingTools: weatherTools()}
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/abc",
		PromptHash: gemini.PromptHash("ORIGINAL SUBMISSION"), Bytes: 19, Chunk: 1, Chunks: 1})
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if !h.recoverPending(r, "s") {
		t.Fatal("clear-cut desync shape must auto-resync")
	}
	s := h.sessions["s"]
	if s.Pending {
		t.Fatal("auto-resync must clear Dirty")
	}
	if s.LastAnswer != answer {
		t.Fatalf("transplant must record the real thread answer: %q", s.LastAnswer)
	}
	if len(s.Messages) != len(pending)+1 || len(s.Messages[len(s.Messages)-1].ToolCalls) != 1 ||
		s.Messages[len(s.Messages)-1].ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("transplanted calls must be recorded for the client: %+v", s.Messages)
	}
	if !strings.Contains(s.AutoResyncEvidence, "auto-resync") || !strings.Contains(s.AutoResyncEvidence, "pending-overwritten-by-retry") {
		t.Fatalf("evidence must explain the transplant: %q", s.AutoResyncEvidence)
	}
	last := h.lastSentFor("s")
	if last == nil || last.Event != "completed" || last.AnswerHash != gemini.AnswerHash(answer) {
		t.Fatalf("transplant must append a sent-log completion with the answer hash: %+v", last)
	}
	if fake.asks != 0 {
		t.Fatal("auto-resync records calls; it never executes or submits them")
	}
}

func TestAutoResyncRefusesWithoutLineage(t *testing.T) {
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, structuralMismatch()
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "SOMEONE ELSE'S TURN", gemini.TurnResult{Answer: "real answer", URL: url}, nil
		},
	}
	h := &handler{model: "gemini-web", client: fake, adoptWait: time.Millisecond, sessions: map[string]*session{
		"s": {Pending: true, PendingPrompt: "NEWER", URL: "https://gemini.google.com/app/abc",
			PendingMessages: []message{{Role: "user", Content: "newer"}}, PendingTools: weatherTools()},
	}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	// No sent-log entry ties this thread to this session: refuse.
	if h.recoverPending(httptest.NewRequest("POST", "/v1/chat/completions", nil), "s") {
		t.Fatal("auto-resync without sent-log lineage must refuse")
	}
	s := h.sessions["s"]
	if !s.Pending || s.AutoResyncEvidence != "" {
		t.Fatalf("refusal must leave Dirty untouched and record no evidence: %+v", s)
	}
	// End to end the identical retry still refuses with the diagnosis line:
	// a session whose recovery refused must never resubmit that turn.
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-web","messages":[{"role":"user","content":"newer"}]}`))
	r.Header.Set("X-Session-Id", "s")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "sent-log:") {
		t.Fatalf("identical retry after refused recovery must stay refused with diagnosis: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 0 {
		t.Fatal("refused recovery must not submit")
	}
}

func TestAmbiguousRecoveryKeepsDirty(t *testing.T) {
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, errors.New("stored thread answer is not a finished rendering; inspect before recovery")
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "", gemini.TurnResult{}, errors.New("stored thread holds no complete assistant answer to adopt")
		},
	}
	h := &handler{model: "gemini-web", client: fake, adoptWait: time.Millisecond, sessions: map[string]*session{
		"s": {Pending: true, PendingPrompt: "P", URL: "https://gemini.google.com/app/abc",
			PendingMessages: []message{{Role: "user", Content: "p"}}},
	}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	if h.recoverPending(httptest.NewRequest("POST", "/v1/chat/completions", nil), "s") {
		t.Fatal("unfinished thread state must not recover")
	}
	if !h.sessions["s"].Pending {
		t.Fatal("ambiguous cases keep Dirty untouched")
	}
	if fake.asks != 0 {
		t.Fatal("unknown-state turns are never submitted")
	}
}

func TestRecoveryEvidenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "openai-sessions.json")
	answer := `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, structuralMismatch()
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "ORIGINAL SUBMISSION", gemini.TurnResult{Answer: answer, URL: url}, nil
		},
	}
	h := &handler{model: "gemini-web", client: fake, statePath: state, sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	pending := []message{{Role: "user", Content: "newer retry"}}
	h.sessions["s"] = &session{Pending: true, PendingPrompt: "NEWER RETRY PROMPT", URL: "https://gemini.google.com/app/abc",
		PendingMessages: pending, PendingTools: weatherTools()}
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/abc",
		PromptHash: gemini.PromptHash("ORIGINAL SUBMISSION"), Bytes: 19, Chunk: 1, Chunks: 1})
	if !h.recoverPending(httptest.NewRequest("POST", "/v1/chat/completions", nil), "s") {
		t.Fatal("setup: auto-resync must succeed")
	}
	restarted := &handler{statePath: state, sessions: map[string]*session{}}
	if err := restarted.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := restarted.sessions["s"]
	if s == nil || s.Pending || !strings.Contains(s.AutoResyncEvidence, "auto-resync") || s.LastAnswer != answer {
		t.Fatalf("journal + evidence must survive restart: %+v", s)
	}
	if got := restarted.sentDiagnosis("s", s); !strings.Contains(got, "session clean") {
		t.Fatalf("post-restart diagnosis must see the clean session: %q", got)
	}
}

func TestClassifyHistoryRewrite(t *testing.T) {
	u := func(s string) message { return message{Role: "user", Content: s} }
	a := func(s string) message { return message{Role: "assistant", Content: s} }
	// Append-only: saved is a prefix of incoming.
	if kind, _ := classifyHistoryRewrite([]message{u("1"), u("2")}, []message{u("1")}); kind != "" {
		t.Fatalf("append must not classify: %q", kind)
	}
	// Truncation: incoming shorter, prefix-equal.
	if kind, mm := classifyHistoryRewrite([]message{u("1")}, []message{u("1"), a("old")}); kind != rewriteTruncation || mm.Field != "history.truncated" {
		t.Fatalf("truncation: %q %+v", kind, mm)
	}
	// Divergence: same shape, changed content.
	if kind, _ := classifyHistoryRewrite([]message{u("1"), u("changed")}, []message{u("1"), u("2")}); kind != rewriteDivergence {
		t.Fatalf("divergence: %q", kind)
	}
	// Compaction: system summary prefix.
	incoming := []message{{Role: "system", Content: "Compacted conversation summary so far"}, u("next")}
	if kind, _ := classifyHistoryRewrite(incoming, []message{u("1"), a("old")}); kind != rewriteCompaction {
		t.Fatalf("compaction: %q", kind)
	}
	// Ambiguous: dangling tool call with no result.
	dangling := []message{u("x"), {Role: "assistant", ToolCalls: []toolCall{{ID: "c1", Type: "function", Function: callFunction{Name: "f", Arguments: "{}"}}}}}
	if kind, _ := classifyHistoryRewrite(dangling, []message{u("1")}); kind != rewriteAmbiguous {
		t.Fatalf("incoherent input must be ambiguous, never reseeded: %q", kind)
	}
}

func committedWeatherHistory() []message {
	return []message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Hanoi"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "72F"},
	}
}

func postJSON(t *testing.T, h *handler, sid string, req chatRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
	r.Header.Set("X-Session-Id", sid)
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}

func TestCleanReseedCarriesExecutedTools(t *testing.T) {
	fake := &fakeRecoveryBackend{askAnswer: "fresh answer", urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000,
		sessions: map[string]*session{
			"s": {Messages: committedWeatherHistory(), URL: "https://gemini.google.com/app/OLD"},
		}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	// Revert: the client drops the executed exchange and asks anew.
	incoming := []message{{Role: "user", Content: "weather?"}, {Role: "user", Content: "something else"}}
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fresh answer") {
		t.Fatalf("reseed must continue as a normal submission: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatal("reseed must abandon the old thread for a fresh Gemini chat")
	}
	if !strings.Contains(strings.Join(fake.prompts, "\n"), "PRIOR EXECUTION RECORD") || !strings.Contains(strings.Join(fake.prompts, "\n"), "call_1") {
		t.Fatalf("seed must carry the executed exchange as inert record: %q", fake.prompts)
	}
	if !strings.Contains(strings.Join(fake.prompts, "\n"), "inert") {
		t.Fatalf("carried calls must render inert, never as pending: %q", fake.prompts)
	}
	s := h.sessions["s"]
	if len(s.Messages) != len(incoming)+1 {
		t.Fatalf("journal must hold the rewritten timeline plus our answer, not the carried record: %+v", s.Messages)
	}
	if s.ReseedFrom != "https://gemini.google.com/app/OLD" || s.URL != "https://gemini.google.com/app/NEW" {
		t.Fatalf("journal must link old→new thread: from=%q url=%q", s.ReseedFrom, s.URL)
	}
	if !strings.Contains(s.ReseedEvidence, "rewrite-divergence") || !strings.Contains(s.ReseedEvidence, "call_1(get_weather)") {
		t.Fatalf("evidence must carry classification + carried list: %q", s.ReseedEvidence)
	}
	var reseedNote string
	for i := len(h.sentLog) - 1; i >= 0; i-- {
		if h.sentLog[i].Event == "reseed" {
			reseedNote = h.sentLog[i].Note
		}
	}
	if !strings.Contains(reseedNote, "old=https://gemini.google.com/app/OLD") || !strings.Contains(reseedNote, "classification=rewrite-divergence") {
		t.Fatalf("sent-log must persist the linkage: %q", reseedNote)
	}
}

func TestDirtyReseedAbandonsUnknown(t *testing.T) {
	fake := &fakeRecoveryBackend{
		askAnswer: "reseed answer", urlNow: "https://gemini.google.com/app/NEW",
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, errors.New("stored thread answer is not a finished rendering; inspect before recovery")
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "", gemini.TurnResult{}, errors.New("stored thread holds no complete assistant answer to adopt")
		},
	}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "reseed answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, adoptWait: time.Millisecond,
		sessions: map[string]*session{
			"s": {Pending: true, PendingPrompt: "PENDING PROMPT", URL: "https://gemini.google.com/app/OLD",
				Messages:        []message{{Role: "user", Content: "q0"}, {Role: "assistant", Content: "a0"}},
				PendingMessages: []message{{Role: "user", Content: "q0"}, {Role: "assistant", Content: "a0"}, {Role: "user", Content: "q-pending"}}},
		}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.appendSent(sentEntry{Event: "submitted", Session: "s", URL: "https://gemini.google.com/app/OLD",
		PromptHash: promptHash("PENDING PROMPT"), Bytes: 14, Chunk: 1, Chunks: 1})
	// Rewrite over uncertainty: committed prefix rewritten, pending unknown.
	incoming := []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q-new"}}
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "reseed answer") {
		t.Fatalf("dirty rewrite must reseed and continue: %d %s", w.Code, w.Body.String())
	}
	s := h.sessions["s"]
	if s.Pending {
		t.Fatal("reseed must clear the abandoned uncertainty")
	}
	if !strings.Contains(s.ReseedEvidence, "pending:") || !strings.Contains(s.ReseedEvidence, "never submitted as pending") {
		t.Fatalf("unknown turn must be listed as not-carried: %q", s.ReseedEvidence)
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatal("dirty reseed must start a fresh chat, never reuse the uncertain thread")
	}
}

func TestLoadBearingUnknownContinuesFresh(t *testing.T) {
	fake := &fakeRecoveryBackend{
		askAnswer: "fresh answer", urlNow: "https://gemini.google.com/app/NEW",
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, errors.New("stored thread answer is not a finished rendering; inspect before recovery")
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "", gemini.TurnResult{}, errors.New("stored thread holds no complete assistant answer to adopt")
		},
	}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, adoptWait: time.Millisecond,
		sessions: map[string]*session{
			"s": {Pending: true, PendingPrompt: "PENDING PROMPT", URL: "https://gemini.google.com/app/OLD",
				Messages:        []message{{Role: "user", Content: "q0"}},
				PendingMessages: []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q-pending"}}},
		}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	// The sent-log proves a COMPLETED answer for the pending prompt exists on
	// the old thread, but the read returns no finished rendering: one
	// bounded adopt attempt, then best-effort fresh continuation as before.
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/OLD",
		PromptHash: promptHash("PENDING PROMPT"), Bytes: 14, Chunk: 1, Chunks: 1, AnswerHash: "abc"})
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q-new"}}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fresh answer") {
		t.Fatalf("distinct request must continue fresh: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatal("abandoned chat must never be reused")
	}
	if fake.reads < 1 || fake.latests != 0 {
		t.Fatalf("one bounded adopt attempt, no auto-resync: exact=%d latest=%d", fake.reads, fake.latests)
	}
	if fake.asks != 2 {
		t.Fatalf("expected import plus final submission, got %d", fake.asks)
	}
	s := h.sessions["s"]
	if s.Pending || s.PendingPrompt != "" {
		t.Fatal("continuation must clear the abandoned uncertainty")
	}
	if !strings.Contains(s.ReseedEvidence, "pending:") || !strings.Contains(s.ReseedEvidence, "never submitted as pending") {
		t.Fatalf("unknown turn must be listed as not-carried: %q", s.ReseedEvidence)
	}
	if s.ReseedFrom != "https://gemini.google.com/app/OLD" {
		t.Fatalf("linkage lost: %q", s.ReseedFrom)
	}
}

func TestHappyPathAppendUnchanged(t *testing.T) {
	// Tab sits on the stored thread: delta binds it and sends only the
	// suffix.
	fake := &fakeRecoveryBackend{askAnswer: "delta answer", urlNow: "https://gemini.google.com/app/OLD"}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000,
		sessions: map[string]*session{
			"s": {Messages: []message{{Role: "user", Content: "q0"}}, URL: "https://gemini.google.com/app/OLD"},
		}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.hasActive, h.activeSID = true, "s"
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q1"}}})
	if w.Code != http.StatusOK {
		t.Fatalf("delta must succeed: %d %s", w.Code, w.Body.String())
	}
	if fake.gotFresh || strings.Contains(fake.gotPrompt, "PRIOR EXECUTION RECORD") {
		t.Fatalf("append continuation must never reseed: fresh=%v prompt=%q", fake.gotFresh, fake.gotPrompt)
	}
	if s := h.sessions["s"]; s.ReseedEvidence != "" || len(s.Messages) != 3 {
		t.Fatalf("happy path must not journal reseed state: %+v", s)
	}
}

func TestReseedOversizeNeverSplits(t *testing.T) {
	fake := &fakeRecoveryBackend{askAnswer: "must never submit"}
	h := &handler{model: "gemini-web", client: fake, maxChars: 50,
		sessions: map[string]*session{
			"s": {Messages: committedWeatherHistory(), URL: "https://gemini.google.com/app/OLD"},
		}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "weather?"}, {Role: "user", Content: "something else"}}
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize reseed seed must be 413: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 0 {
		t.Fatal("rejected seeds are never submitted, whole or split")
	}
	if last := h.lastSentFor("s"); last != nil {
		t.Fatalf("rejected reseed must log no submission: %+v", last)
	}
	if len(h.sessions["s"].Messages) != len(committedWeatherHistory()) {
		t.Fatal("rejected reseed must leave the journal untouched")
	}
}

func TestCarrySkipsIncompleteCommitted(t *testing.T) {
	s := &session{Messages: []message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "call_9", Type: "function", Function: callFunction{Name: "f", Arguments: "{}"}}}},
	}}
	carried, _, incomplete := carryExecuted(s, nil, []message{{Role: "user", Content: "reverted"}})
	if len(carried) != 0 {
		t.Fatalf("calls without recorded results must not carry: %+v", carried)
	}
	if len(incomplete) != 1 || !strings.Contains(incomplete[0], "call_9") {
		t.Fatalf("dropped unknowns must be named: %+v", incomplete)
	}
}

func TestReseedEvidenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "openai-sessions.json")
	fake := &fakeRecoveryBackend{askAnswer: "fresh answer", urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, statePath: state,
		sessions: map[string]*session{
			"s": {Messages: committedWeatherHistory(), URL: "https://gemini.google.com/app/OLD"},
		}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "weather?"}, {Role: "user", Content: "something else"}}
	if w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming}); w.Code != http.StatusOK {
		t.Fatalf("setup: reseed must succeed: %d %s", w.Code, w.Body.String())
	}
	restarted := &handler{statePath: state, sessions: map[string]*session{}}
	if err := restarted.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := restarted.sessions["s"]
	if s == nil || s.Pending || s.ReseedFrom != "https://gemini.google.com/app/OLD" ||
		!strings.Contains(s.ReseedEvidence, "rewrite-divergence") || len(s.Messages) != len(incoming)+1 {
		t.Fatalf("reseed linkage must survive restart: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// Output images: multipart emission, rich-turn preference, recovery carriage.
// ---------------------------------------------------------------------------

func testImageParts() []gemini.ResponseImage {
	return []gemini.ResponseImage{{Alt: "sales chart", URL: "data:image/png;base64,QUJD"}}
}

func TestAssistantContentMultipart(t *testing.T) {
	// Text-only turns stay a plain string: wire-identical to before.
	if c := assistantContent("hello", nil); c != "hello" {
		t.Fatalf("text-only must stay a string: %#v", c)
	}
	parts, ok := assistantContent("Here: [Image: sales chart]", testImageParts()).([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("image turns must be multipart: %#v", assistantContent("x", testImageParts()))
	}
	text, ok := parts[0].(map[string]any)
	if !ok || text["type"] != "text" {
		t.Fatalf("first part must be text: %#v", parts[0])
	}
	img, ok := parts[1].(map[string]any)
	if !ok || img["type"] != "image_url" {
		t.Fatalf("second part must be image_url: %#v", parts[1])
	}
	if url, _ := img["image_url"].(map[string]any)["url"].(string); url != "data:image/png;base64,QUJD" {
		t.Fatalf("image URL must pass through: %#v", img)
	}
	// Empty-URL slots never become parts; all-empty degrades to the string.
	if c := assistantContent("t", []gemini.ResponseImage{{URL: " "}}); c != "t" {
		t.Fatalf("empty URLs must degrade to text: %#v", c)
	}
}

func TestStreamTextOf(t *testing.T) {
	if got := streamTextOf("hello"); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("string content: %#v", got)
	}
	if got := streamTextOf(""); len(got) != 0 {
		t.Fatalf("empty string: %#v", got)
	}
	got := streamTextOf(assistantContent("Here", testImageParts()))
	if len(got) != 1 || got[0] != "Here" {
		t.Fatalf("multipart streams text only (fixtures describe images): %#v", got)
	}
}

// fakeImageBackend offers the rich AskWithImages surface.
type fakeImageBackend struct {
	fakeRecoveryBackend
	richAnswer string
	richImages []gemini.ResponseImage
	richErr    error
	usedRich   bool
}

func (f *fakeImageBackend) AskWithImages(_ context.Context, prompt string, newSession bool, _ time.Duration) (string, []gemini.ResponseImage, error) {
	f.usedRich = true
	f.gotPrompt = prompt
	f.gotFresh = newSession
	if f.richErr != nil {
		return "", nil, f.richErr
	}
	return f.richAnswer, f.richImages, nil
}

func TestAskTurnPrefersRichSurface(t *testing.T) {
	rich := &fakeImageBackend{richAnswer: "Here: [Image: sales chart]", richImages: testImageParts()}
	h := &handler{model: "gemini-web", client: rich, sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	answer, images, err := h.askTurn(context.Background(), "draw", true)
	if err != nil || answer != "Here: [Image: sales chart]" || len(images) != 1 {
		t.Fatalf("rich surface must win: %q %+v %v", answer, images, err)
	}
	if !rich.usedRich {
		t.Fatal("AskWithImages must be preferred when offered")
	}
	// Legacy backends without the rich surface keep working (no images).
	legacy := &fakeRecoveryBackend{askAnswer: "plain"}
	h2 := &handler{model: "gemini-web", client: legacy, sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	answer, images, err = h2.askTurn(context.Background(), "hi", true)
	if err != nil || answer != "plain" || len(images) != 0 {
		t.Fatalf("legacy Ask fallback broken: %q %+v %v", answer, images, err)
	}
}

func TestCompletionCarriesImagePartsEndToEnd(t *testing.T) {
	fake := &fakeImageBackend{richAnswer: "Here: [Image: sales chart]", richImages: testImageParts()}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "draw a chart"}}})
	if w.Code != http.StatusOK {
		t.Fatalf("image turn must succeed: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(resp.Choices[0].Message.Content)
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 2 {
		t.Fatalf("completion must be text + image_url parts: %s", string(raw))
	}
	if parts[0].Type != "text" || !strings.Contains(parts[0].Text, "[Image: sales chart]") {
		t.Fatalf("text part must carry the fixture: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,QUJD" {
		t.Fatalf("image part must carry the bytes URL: %+v", parts[1])
	}
	// Exact retry serves the duplicate cache with the same parts.
	w2 := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "draw a chart"}}})
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "data:image/png;base64,QUJD") {
		t.Fatalf("duplicate cache must replay image parts: %d %s", w2.Code, w2.Body.String())
	}
	if fake.asks != 0 {
		t.Fatalf("cache must not resubmit: asks=%d", fake.asks)
	}
}

func TestRecordCompletedTurnCarriesImages(t *testing.T) {
	req := chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "draw"}}}
	h := &handler{model: "gemini-web", sessions: map[string]*session{}, gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	s := &session{Pending: true, PendingPrompt: "DRAW", URL: "https://gemini.google.com/app/abc",
		PendingMessages: req.Messages, PendingReqHash: reqHash(&req)}
	h.sessions["s"] = s
	if !h.recordCompletedTurn("s", s, "Here: [Image: sales chart]", testImageParts(), "", "test") {
		t.Fatal("recovery must record the image turn")
	}
	if len(s.LastImages) != 1 {
		t.Fatalf("journal must keep the parts for duplicate-cache: %+v", s)
	}
	last := s.Messages[len(s.Messages)-1]
	raw, _ := json.Marshal(last.Content)
	if !strings.Contains(string(raw), "image_url") {
		t.Fatalf("transcript must hold the multipart message: %s", string(raw))
	}
	// Unretrievable images (no parts) still record text + note, never fail.
	s2 := &session{Pending: true, PendingPrompt: "DRAW2", URL: "https://gemini.google.com/app/abc",
		PendingMessages: req.Messages, PendingReqHash: reqHash(&req)}
	h.sessions["s2"] = s2
	if !h.recordCompletedTurn("s2", s2, "[Image 1: visual content not retrievable from web UI]", nil, "", "test") {
		t.Fatal("fallback text must record without parts")
	}
	if c, ok := s2.Messages[len(s2.Messages)-1].Content.(string); !ok || !strings.Contains(c, "not retrievable") {
		t.Fatalf("fallback stays a plain string: %#v", s2.Messages[len(s2.Messages)-1].Content)
	}
}

// TestDraftRefusalMappingIsNonRetryable pins the distinction the chat depends
// on: a draft the bridge cannot verify is a 422 the person must act on, while a
// genuine pre-submit transport failure stays a retryable 502. Neither sentence
// may name an endpoint or echo the prompt.
func TestDraftRefusalMappingIsNonRetryable(t *testing.T) {
	const secret = "the private prompt body"
	newHandler := func(err error) *handler {
		be := &fakeRecoveryBackend{askErr: err, urlNow: ""}
		be.readTurn = func(string, string) (gemini.TurnResult, error) { return gemini.TurnResult{}, nil }
		be.readLatest = func(string) (string, gemini.TurnResult, error) { return "", gemini.TurnResult{}, nil }
		h := &handler{model: "gemini-web", client: be, maxChars: 200000, gate: make(chan struct{}, 1), sessions: map[string]*session{}}
		h.gate <- struct{}{}
		return h
	}
	post := func(h *handler) *httptest.ResponseRecorder {
		body := `{"model":"gemini-web","messages":[{"role":"user","content":"` + secret + `"}]}`
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("X-Session-Id", "s")
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		return w
	}

	t.Run("foreign draft is non-retryable and actionable", func(t *testing.T) {
		h := newHandler(gemini.ErrForeignDraft)
		w := post(h)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("foreign draft must be 422, not a retryable backend failure: %d %s", w.Code, w.Body.String())
		}
		out := w.Body.String()
		for _, want := range []string{"cannot verify it wrote", "Nothing from your previous message was resent", "dedicated Gemini Web Chrome window"} {
			if !strings.Contains(out, want) {
				t.Fatalf("refusal missing %q: %s", want, out)
			}
		}
		for _, bad := range []string{"BACKEND FAILURE", "http://", "POST /v1/", secret} {
			if strings.Contains(out, bad) {
				t.Fatalf("refusal leaks %q: %s", bad, out)
			}
		}
		if h.sessions["s"] != nil && h.sessions["s"].Pending {
			t.Fatal("a refusal must leave no pending binding")
		}
	})

	t.Run("attempted draft is non-retryable", func(t *testing.T) {
		h := newHandler(gemini.ErrDraftOwned)
		w := post(h)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempted draft must be 422: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "may already have been sent") {
			t.Fatalf("attempted draft must say the turn may exist: %s", w.Body.String())
		}
		// Uncertain turns must never invite a blind retry.
		for _, want := range []string{"Do not resend", "no tools were re-run"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("attempted draft guidance missing %q: %s", want, w.Body.String())
			}
		}
	})

	t.Run("pre-submit transport failure stays retryable", func(t *testing.T) {
		h := newHandler(gemini.ErrNotSubmitted)
		w := post(h)
		if w.Code == http.StatusUnprocessableEntity {
			t.Fatalf("a retryable transport failure must not be reported as a draft refusal: %d %s", w.Code, w.Body.String())
		}
		if w.Code != http.StatusBadGateway {
			t.Fatalf("transport failure: %d %s", w.Code, w.Body.String())
		}
	})
}

func TestSessionStateInspector(t *testing.T) {
	get := func(h *handler, sid string, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/sessions/state", nil)
		if sid != "" {
			r.Header.Set("x-opencode-session-id", sid)
		}
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		return w
	}
	decode := func(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("state body is not JSON: %v", err)
		}
		return body
	}
	newH := func(sessions map[string]*session) *handler {
		return &handler{model: "gemini-web", client: &fakeReadyBackend{}, gate: make(chan struct{}, 1), sessions: sessions}
	}

	if w := get(newH(map[string]*session{}), "", "GET"); w.Code != http.StatusBadRequest {
		t.Fatalf("missing session header must be 400, got %d", w.Code)
	}
	if w := get(newH(map[string]*session{}), "s", "POST"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("state requires GET, got %d", w.Code)
	}

	body := decode(t, get(newH(map[string]*session{}), "nope", "GET"))
	if body["state"] != "unknown_session" || len(body["actions"].([]any)) != 0 {
		t.Fatalf("unknown session misreported: %v", body)
	}

	body = decode(t, get(newH(map[string]*session{"s": {Messages: []message{{Role: "user", Content: "hi"}}}}), "s", "GET"))
	if body["state"] != "clean" {
		t.Fatalf("clean session misreported: %v", body)
	}

	pending := &session{
		Pending: true, URL: "https://gemini.google.com/app/THREAD",
		PendingPrompt: "secret-prompt-bytes", PendingMessages: []message{{Role: "user", Content: "uncertain"}},
		Messages: []message{{Role: "user", Content: "hi"}},
	}
	w := get(newH(map[string]*session{"s": pending}), "s", "GET")
	body = decode(t, w)
	if body["state"] != "pending" || body["pending_prompt_bytes"].(float64) == 0 || body["thread_url_present"] != true {
		t.Fatalf("pending session misreported: %v", body)
	}
	if strings.Contains(w.Body.String(), "secret-prompt-bytes") {
		t.Fatal("state inspector leaked prompt text")
	}

	body = decode(t, get(newH(map[string]*session{"s": {Pending: true}}), "s", "GET"))
	if body["state"] != "pending_unbound" {
		t.Fatalf("unbound pending misreported: %v", body)
	}
}

func chunkedHandler(fake *fakeRecoveryBackend, t *testing.T) *handler {
	h := &handler{model: "gemini-web", client: fake, maxChars: 3000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{}}
	h.gate <- struct{}{}
	return h
}

func ackAnswerFor(prompt string) string {
	if strings.Contains(prompt, "HISTORY IMPORT") {
		return "ACK"
	}
	return "all good"
}

func TestChunkedReplaySplitsHistoryAndKeepsOneFinal(t *testing.T) {
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = ackAnswerFor
	h := chunkedHandler(fake, t)
	msgs := []message{
		{Role: "user", Content: "Q1 " + strings.Repeat("a", 1200)},
		{Role: "assistant", Content: "A1 " + strings.Repeat("b", 1200)},
		{Role: "user", Content: "Q2 " + strings.Repeat("c", 1200)},
		{Role: "assistant", Content: "A2 " + strings.Repeat("d", 1200)},
		{Role: "user", Content: "final question"},
	}
	w := postJSON(t, h, "chunk1", chatRequest{Model: "gemini-web", Messages: msgs})
	if w.Code != http.StatusOK {
		t.Fatalf("chunked replay: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 3 {
		t.Fatalf("history must arrive as 2 imports plus final, got %d asks", fake.asks)
	}
	if len(fake.freshFlags) != 3 || !fake.freshFlags[0] || fake.freshFlags[1] || fake.freshFlags[2] {
		t.Fatalf("only the first turn starts a chat: %v", fake.freshFlags)
	}
	for i, p := range fake.prompts {
		if len(p) > 3000 {
			t.Fatalf("turn %d exceeds the paste ceiling: %d bytes", i+1, len(p))
		}
	}
	if !strings.Contains(fake.prompts[2], "final question") {
		t.Fatal("final turn must carry the question")
	}
	sess := h.sessions["chunk1"]
	if sess == nil || len(sess.Messages) != len(msgs)+1 || sess.URL == "" {
		t.Fatalf("transcript plus our answer not committed: %+v", sess)
	}
}

func TestChunkedReplayRejectsNonAckImport(t *testing.T) {
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") {
			return "here is a whole essay instead"
		}
		return "all good"
	}
	h := chunkedHandler(fake, t)
	msgs := []message{
		{Role: "user", Content: "Q1 " + strings.Repeat("a", 1200)},
		{Role: "assistant", Content: "A1 " + strings.Repeat("b", 1200)},
		{Role: "user", Content: "final question"},
	}
	w := postJSON(t, h, "chunk2", chatRequest{Model: "gemini-web", Messages: msgs})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("non-ACK import must fail closed: %d %s", w.Code, w.Body.String())
	}
	if !h.sessions["chunk2"].Pending {
		t.Fatal("refused import must keep the pending binding")
	}
}

func TestChunkedReplayAcceptsEmptyImport(t *testing.T) {
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") {
			return ""
		}
		return "all good"
	}
	h := chunkedHandler(fake, t)
	msgs := []message{
		{Role: "user", Content: "Q1 " + strings.Repeat("a", 1200)},
		{Role: "assistant", Content: "A1 " + strings.Repeat("b", 1200)},
		{Role: "user", Content: "final question"},
	}
	w := postJSON(t, h, "chunk3", chatRequest{Model: "gemini-web", Messages: msgs})
	if w.Code != http.StatusOK {
		t.Fatalf("empty import acknowledgement must not wedge: %d %s", w.Code, w.Body.String())
	}
}

func TestSingleOversizeQuestionRefusedNotSplit(t *testing.T) {
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = ackAnswerFor
	h := chunkedHandler(fake, t)
	msgs := []message{{Role: "user", Content: strings.Repeat("q", 5000)}}
	w := postJSON(t, h, "chunk4", chatRequest{Model: "gemini-web", Messages: msgs})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize single question must be refused: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "will not split") {
		t.Fatalf("refusal must name the no-split rule: %s", w.Body.String())
	}
	if fake.asks != 0 {
		t.Fatalf("refused question must never reach the browser: %d asks", fake.asks)
	}
}

func TestSecondTurnSkipsOwnAnswerImport(t *testing.T) {
	// Live symptom: every other prompt replayed a ~1KB import because the
	// journal never absorbed our own answers, so the client echo of the
	// previous answer always looked like new history. Two consecutive
	// turns: the second must send only its question.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "answer text"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{}}
	h.gate <- struct{}{}
	first := []message{{Role: "user", Content: "first question"}}
	if w := postJSON(t, h, "steady", chatRequest{Model: "gemini-web", Messages: first}); w.Code != http.StatusOK {
		t.Fatalf("first turn: %d %s", w.Code, w.Body.String())
	}
	if n := len(h.sessions["steady"].Messages); n != 2 {
		t.Fatalf("journal must absorb our answer: %d messages", n)
	}
	second := append(append([]message{}, h.sessions["steady"].Messages...), message{Role: "user", Content: "second question"})
	fake.prompts, fake.freshFlags, fake.asks = nil, nil, 0
	if w := postJSON(t, h, "steady", chatRequest{Model: "gemini-web", Messages: second}); w.Code != http.StatusOK {
		t.Fatalf("second turn: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 1 {
		t.Fatalf("second turn must send only its question, got %d submissions: %q", fake.asks, fake.prompts)
	}
	if strings.Contains(fake.prompts[0], "HISTORY IMPORT") {
		t.Fatalf("no import on a clean continuation: %q", fake.prompts[0])
	}
}

func TestThreadRejoinAfterSlotLoss(t *testing.T) {
	const threadURL = "https://gemini.google.com/app/THREAD"
	newRejoinHandler := func(urlNow string) (*fakeRecoveryBackend, *handler) {
		fake := &fakeRecoveryBackend{urlNow: urlNow}
		fake.answerFor = func(prompt string) string {
			if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
				return "ACK"
			}
			return "rejoin answer"
		}
		// No hasActive slot: simulates a wiped single-slot store after
		// an adapter restart. Delta must still engage when the tab never
		// moved, proven read-only against the stored thread URL.
		h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
			statePath: filepath.Join(t.TempDir(), "state.json"),
			sessions:  map[string]*session{"rj": {Messages: []message{{Role: "user", Content: "q0"}}, URL: threadURL}}}
		h.gate <- struct{}{}
		return fake, h
	}
	incoming := []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q1"}}
	t.Run("tab unmoved rejoins delta", func(t *testing.T) {
		fake, h := newRejoinHandler(threadURL)
		w := postJSON(t, h, "rj", chatRequest{Model: "gemini-web", Messages: incoming})
		if w.Code != http.StatusOK {
			t.Fatalf("rejoin: %d %s", w.Code, w.Body.String())
		}
		if fake.asks != 1 || len(fake.freshFlags) != 1 || fake.freshFlags[0] {
			t.Fatalf("tab on stored thread must delta once: asks=%d fresh=%v", fake.asks, fake.freshFlags)
		}
		if got := h.sessions["rj"].URL; got != threadURL {
			t.Fatalf("thread moved: %s", got)
		}
	})
	t.Run("tab moved fails closed without a new chat", func(t *testing.T) {
		fake, h := newRejoinHandler("https://gemini.google.com/app/ELSEWHERE")
		w := postJSON(t, h, "rj", chatRequest{Model: "gemini-web", Messages: incoming})
		if w.Code == http.StatusOK {
			t.Fatalf("moved tab must refuse, got 200 %s", w.Body.String())
		}
		if fake.asks != 0 {
			t.Fatalf("zero sends when the tab left the saved thread: asks=%d", fake.asks)
		}
		for _, p := range fake.prompts {
			if strings.Contains(p, "HISTORY IMPORT") || strings.Contains(p, "PRIOR EXECUTION RECORD") {
				t.Fatalf("no replacement chat on a moved tab: %q", p)
			}
		}
		if got := h.sessions["rj"].URL; got != threadURL {
			t.Fatalf("saved thread retained: %s", got)
		}
		if h.sessions["rj"].Pending {
			t.Fatalf("pending must clear on proved pre-submit failure")
		}
	})
}

func TestAssistantEchoBloatAbsorbed(t *testing.T) {
	// Live shape: our 130-byte committed answer re-echoed with ~637
	// appended re-render bytes. The echo is an assistant-role append of
	// our own bytes, so the turn continues delta on the bound thread
	// instead of reseeding a new chat every turn. User/system/tool
	// appends stay significant (companion tests below).
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/THREAD", mintURL: "https://gemini.google.com/app/NEW2"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fine"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"tol": {Messages: []message{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: strings.Repeat("x", 130)},
		}, URL: "https://gemini.google.com/app/THREAD"}}}
	h.gate <- struct{}{}
	incoming := []message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: strings.Repeat("x", 130) + strings.Repeat("y", 637)},
		{Role: "user", Content: "next"},
	}
	w := postJSON(t, h, "tol", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("echo bloat must continue delta: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 1 || len(fake.freshFlags) != 1 || fake.freshFlags[0] {
		t.Fatalf("one delta submit on the bound thread: asks=%d fresh=%v", fake.asks, fake.freshFlags)
	}
	if got := h.sessions["tol"].URL; got != "https://gemini.google.com/app/THREAD" {
		t.Fatalf("thread moved: %s", got)
	}
}

func TestAssistantOverBoundAppendStillReseeds(t *testing.T) {
	// Same shape at +2000B: beyond tolerance, so it reseeds fresh rather
	// than risk silently dropping a genuine user edit to our answer.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW2", mintURL: "https://gemini.google.com/app/NEW2"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fine"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"tol2": {Messages: []message{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: strings.Repeat("x", 130)},
		}, URL: "https://gemini.google.com/app/THREAD"}}}
	h.gate <- struct{}{}
	incoming := []message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: strings.Repeat("x", 130) + strings.Repeat("y", 2000)},
		{Role: "user", Content: "next"},
	}
	w := postJSON(t, h, "tol2", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("over-bound append: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("over-bound divergence must start a new chat: fresh=%v", fake.freshFlags)
	}
}

func TestUnknownUserSuffixNotIgnored(t *testing.T) {
	// A user-message edit (same length class, different tail) is real
	// intent: delta is refused and the full timeline travels.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/THREAD", mintURL: "https://gemini.google.com/app/NEW3"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fine"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"u": {Messages: []message{
			{Role: "user", Content: "deploy to staging"},
		}, URL: "https://gemini.google.com/app/THREAD"}}}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "deploy to production"}}
	w := postJSON(t, h, "u", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("user rewrite must reseed, not refuse: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("edited user message must not delta: fresh=%v", fake.freshFlags)
	}
	joined := strings.Join(fake.prompts, "\n")
	if !strings.Contains(joined, "deploy to production") {
		t.Fatalf("edited text must travel in full: %q", joined)
	}
}

func TestTwoConsecutivePromptsSameURL(t *testing.T) {
	// One session, two turns: the second sends only its suffix on the
	// same thread, with zero imports.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/STEADY", mintURL: "https://gemini.google.com/app/STEADY"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "answer text"
	}
	dir := t.TempDir()
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(dir, "state.json"), sessions: map[string]*session{}}
	h.gate <- struct{}{}
	first := []message{{Role: "user", Content: "first question"}}
	if w := postJSON(t, h, "steady", chatRequest{Model: "gemini-web", Messages: first}); w.Code != http.StatusOK {
		t.Fatalf("first turn: %d %s", w.Code, w.Body.String())
	}
	thread := h.sessions["steady"].URL
	if !gemini.ValidThreadURL(thread) {
		t.Fatalf("first turn must mint a valid thread URL: %q", thread)
	}
	second := append(append([]message{}, h.sessions["steady"].Messages...), message{Role: "user", Content: "second question"})
	fake.prompts, fake.freshFlags, fake.asks, fake.boundURLs = nil, nil, 0, nil
	if w := postJSON(t, h, "steady", chatRequest{Model: "gemini-web", Messages: second}); w.Code != http.StatusOK {
		t.Fatalf("second turn: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 1 {
		t.Fatalf("second turn must submit once, got %d: %q", fake.asks, fake.prompts)
	}
	if len(fake.freshFlags) != 1 || fake.freshFlags[0] {
		t.Fatalf("second turn must continue (fresh=false): %v", fake.freshFlags)
	}
	if len(fake.boundURLs) != 1 || fake.boundURLs[0] != thread {
		t.Fatalf("second turn must bind the saved thread: %q", fake.boundURLs)
	}
	if got := h.sessions["steady"].URL; got != thread {
		t.Fatalf("thread must not move on delta: %q", got)
	}
	if strings.Contains(fake.prompts[0], "HISTORY IMPORT") {
		t.Fatalf("no import on a clean continuation: %q", fake.prompts[0])
	}
}

func TestSerializedReloadRestoresURL(t *testing.T) {
	// The saved thread URL survives an adapter restart via the state file.
	dir := t.TempDir()
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/RELOAD", mintURL: "https://gemini.google.com/app/RELOAD"}
	fake.answerFor = func(prompt string) string { return "ok" }
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(dir, "state.json"), sessions: map[string]*session{}}
	h.gate <- struct{}{}
	if w := postJSON(t, h, "rl", chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hi"}}}); w.Code != http.StatusOK {
		t.Fatalf("first turn: %d %s", w.Code, w.Body.String())
	}
	h2 := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(dir, "state.json")}
	h2.gate <- struct{}{}
	if err := h2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := h2.sessions["rl"]
	if s == nil || !gemini.ValidThreadURL(s.URL) {
		t.Fatalf("reloaded journal must carry the thread URL: %+v", s)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("reloaded journal must carry the transcript: %d messages", len(s.Messages))
	}
}

func TestAlternatingSessionsKeepTargets(t *testing.T) {
	// A->B->A: each submit binds its own session's thread, never the
	// other session's.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/SHARED"}
	fake.answerFor = func(prompt string) string { return "ok" }
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]*session{}}
	h.gate <- struct{}{}
	mkURL := func(id string) string { return "https://gemini.google.com/app/" + id }
	for _, sid := range []string{"A", "B"} {
		fake.mintURL = mkURL("THREAD" + sid)
		if w := postJSON(t, h, sid, chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hello " + sid}}}); w.Code != http.StatusOK {
			t.Fatalf("first %s: %d %s", sid, w.Code, w.Body.String())
		}
	}
	if h.sessions["A"].URL == h.sessions["B"].URL {
		t.Fatalf("sessions must hold distinct threads: %q", h.sessions["A"].URL)
	}
	// Interleave continuations: each binds its own thread.
	for _, sid := range []string{"A", "B", "A"} {
		fake.urlNow = h.sessions[sid].URL
		fake.mintURL = ""
		before := len(fake.boundURLs)
		next := append(append([]message{}, h.sessions[sid].Messages...), message{Role: "user", Content: "again " + sid})
		if w := postJSON(t, h, sid, chatRequest{Model: "gemini-web", Messages: next}); w.Code != http.StatusOK {
			t.Fatalf("continue %s: %d %s", sid, w.Code, w.Body.String())
		}
		got := fake.boundURLs[before:]
		if len(got) != 1 || got[0] != h.sessions[sid].URL {
			t.Fatalf("%s must bind its own thread: %q session=%q", sid, got, h.sessions[sid].URL)
		}
	}
}

func TestTabRebuiltBeforeRequestRestoresTarget(t *testing.T) {
	// The tab died and tryEnsure rebuilt it on the homepage: the bound
	// submit restores the saved thread with zero homepage submits.
	const thread = "https://gemini.google.com/app/SAVEDTHREAD"
	fake := &fakeRecoveryBackend{urlNow: geminiHomePage(), restoreTo: thread}
	fake.answerFor = func(prompt string) string { return "restored answer" }
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"rb": {Messages: []message{{Role: "user", Content: "q0"}}, URL: thread}}}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q1"}}
	if w := postJSON(t, h, "rb", chatRequest{Model: "gemini-web", Messages: incoming}); w.Code != http.StatusOK {
		t.Fatalf("restored submit: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || fake.freshFlags[0] {
		t.Fatalf("restored tab must delta, never mint: fresh=%v", fake.freshFlags)
	}
	if len(fake.boundURLs) == 0 || fake.boundURLs[0] != thread {
		t.Fatalf("submit must bind the saved thread: %q", fake.boundURLs)
	}
	if got := h.sessions["rb"].URL; got != thread {
		t.Fatalf("journal must keep the saved thread: %q", got)
	}
}

func TestRebuildBetweenChunksRestoresTarget(t *testing.T) {
	// A fresh reseed travels as import + final. The tab rebuilds on the
	// homepage between the two chunks; the final still binds the minted
	// thread — the suffix never lands on the homepage.
	const minted = "https://gemini.google.com/app/MINTEDBETWEEN"
	fake := &fakeRecoveryBackend{urlNow: geminiHomePage(), mintURL: minted, restoreTo: minted, dropToHomeAfter: 1}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "reseeded answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"bc": {Messages: committedWeatherHistory(), URL: "https://gemini.google.com/app/OLD"}}}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "weather?"}, {Role: "user", Content: "something else"}}
	w := postJSON(t, h, "bc", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("reseed across rebuild: %d %s", w.Code, w.Body.String())
	}
	if fake.asks < 2 {
		t.Fatalf("import plus final expected, got %d asks %q", fake.asks, fake.prompts)
	}
	last := fake.boundURLs[len(fake.boundURLs)-1]
	if last != minted {
		t.Fatalf("final chunk must bind the minted thread: %q", fake.boundURLs)
	}
	if got := h.sessions["bc"].URL; got != minted {
		t.Fatalf("journal must adopt the minted thread: %q", got)
	}
}

func TestUnknownSystemSuffixNotIgnored(t *testing.T) {
	// A rewritten system message is significant too: no delta, full
	// timeline travels on a new thread.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/THREAD", mintURL: "https://gemini.google.com/app/NEWSYS"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fine"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"sy": {Messages: []message{
			{Role: "system", Content: "you are a cat"},
			{Role: "user", Content: "q"},
		}, URL: "https://gemini.google.com/app/THREAD"}}}
	h.gate <- struct{}{}
	incoming := []message{
		{Role: "system", Content: "you are a dog"},
		{Role: "user", Content: "q"},
		{Role: "user", Content: "next"},
	}
	w := postJSON(t, h, "sy", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("system rewrite must reseed, not refuse: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("edited system message must not delta: fresh=%v", fake.freshFlags)
	}
}

func TestFailedRestoreZeroSend(t *testing.T) {
	// Restore fails (auth/challenge/redirect class): zero sends, zero
	// replacement chat, URL and transcript retained, pending cleared.
	const thread = "https://gemini.google.com/app/STUCKTHREAD"
	fake := &fakeRecoveryBackend{urlNow: geminiHomePage(), ensureErr: threadBindingErr()}
	fake.answerFor = func(prompt string) string { return "must not send" }
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"st": {Messages: []message{{Role: "user", Content: "q0"}}, URL: thread}}}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q1"}}
	if w := postJSON(t, h, "st", chatRequest{Model: "gemini-web", Messages: incoming}); w.Code == http.StatusOK {
		t.Fatalf("failed restore must refuse, got 200 %s", w.Body.String())
	}
	if fake.asks != 0 {
		t.Fatalf("zero sends on failed restore: asks=%d", fake.asks)
	}
	for _, p := range fake.prompts {
		if strings.Contains(p, "HISTORY IMPORT") || strings.Contains(p, "PRIOR EXECUTION RECORD") {
			t.Fatalf("no replacement chat on failed restore: %q", p)
		}
	}
	s := h.sessions["st"]
	if s.URL != thread || len(s.Messages) != 1 {
		t.Fatalf("URL and transcript retained: url=%q msgs=%d", s.URL, len(s.Messages))
	}
	if s.Pending {
		t.Fatalf("pending must clear on proved pre-submit failure")
	}
}

func TestInvalidBindingRefused(t *testing.T) {
	// Homepage or empty journal URLs are never continuation targets: no
	// delta submit may bind them.
	for name, url := range map[string]string{"homepage": geminiHomePage(), "empty": ""} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRecoveryBackend{urlNow: geminiHomePage()}
			fake.answerFor = func(prompt string) string { return "must not send" }
			h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
				statePath: filepath.Join(t.TempDir(), "state.json"),
				sessions:  map[string]*session{"bad": {Messages: []message{{Role: "user", Content: "q0"}}, URL: url}}}
			h.gate <- struct{}{}
			incoming := []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q1"}}
			postJSON(t, h, "bad", chatRequest{Model: "gemini-web", Messages: incoming})
			// Every recorded submit is either fresh (unbound mint) or
			// bound to a valid URL — never the invalid journal URL, and
			// never a delta (fresh=false) at all here.
			for i, b := range fake.boundURLs {
				if !fake.freshFlags[i] && b == url {
					t.Fatalf("invalid URL must never be a continuation target: %q", b)
				}
				if b != "" && !gemini.ValidThreadURL(b) {
					t.Fatalf("only valid URLs are bindable: %q", b)
				}
			}
			for _, f := range fake.freshFlags {
				if !f {
					t.Fatalf("no delta may run without a valid binding: %v", fake.freshFlags)
				}
			}
		})
	}
}

func TestFreshAuthorizesExactlyOneThread(t *testing.T) {
	// A genuine fresh session mints exactly one thread: chunk 1 submits
	// fresh, every later chunk binds the minted URL.
	fake := &fakeRecoveryBackend{urlNow: geminiHomePage(), mintURL: "https://gemini.google.com/app/MINTED1"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]*session{}}
	h.gate <- struct{}{}
	incoming := []message{{Role: "user", Content: "weather?"}, {Role: "user", Content: "something else"}}
	w := postJSON(t, h, "fr", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("fresh: %d %s", w.Code, w.Body.String())
	}
	freshCount := 0
	for _, f := range fake.freshFlags {
		if f {
			freshCount++
		}
	}
	if freshCount != 1 {
		t.Fatalf("exactly one fresh submit per new session: fresh=%v", fake.freshFlags)
	}
	if got := h.sessions["fr"].URL; got != "https://gemini.google.com/app/MINTED1" {
		t.Fatalf("journal must adopt the minted thread: %q", got)
	}
}

func TestDeltaPerformsNoToolReplay(t *testing.T) {
	// A delta continuation submits only the suffix with no history
	// imports, so committed tool calls are never re-executed.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NOTOOLS"}
	fake.answerFor = func(prompt string) string { return "plain answer" }
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"nr": {Messages: committedWeatherHistory(), URL: "https://gemini.google.com/app/NOTOOLS"}}}
	h.gate <- struct{}{}
	next := append(append([]message{}, h.sessions["nr"].Messages...), message{Role: "user", Content: "and tomorrow?"})
	w := postJSON(t, h, "nr", chatRequest{Model: "gemini-web", Messages: next, Tools: weatherTools()})
	if w.Code != http.StatusOK {
		t.Fatalf("delta with tools: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 1 {
		t.Fatalf("one suffix submit, got %d", fake.asks)
	}
	joined := strings.Join(fake.prompts, "\n")
	if strings.Contains(joined, "HISTORY IMPORT") || strings.Contains(joined, "PRIOR EXECUTION RECORD") {
		t.Fatalf("delta must not import history: %q", joined)
	}
	if strings.Contains(joined, "call_1") {
		t.Fatalf("delta must not replay committed tool calls: %q", joined)
	}
}

func geminiHomePage() string { return "https://gemini.google.com/app" }

func TestSystemOnlyDivergenceContinuesDelta(t *testing.T) {
	// Live shape: regenerated 172KB system prompt (tool-set flip 0->12)
	// over an untouched 2-message conversation. Must continue on the
	// bound thread with the new tool names reminded — not reseed.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/THREAD"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fine"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"sys": {Messages: []message{
			{Role: "system", Content: "trimmed instructions"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		}, ToolsFP: toolsFingerprint(nil), URL: "https://gemini.google.com/app/THREAD"}}}
	h.gate <- struct{}{}
	incoming := []message{
		{Role: "system", Content: "full instructions " + strings.Repeat("s", 5000)},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "what tool can you use?"},
	}
	req := chatRequest{Model: "gemini-web", Messages: incoming, Tools: weatherTools()}
	w := postJSON(t, h, "sys", req)
	if w.Code != http.StatusOK {
		t.Fatalf("system-only divergence must continue delta: %d %s", w.Code, w.Body.String())
	}
	if fake.asks != 1 || len(fake.freshFlags) != 1 || fake.freshFlags[0] {
		t.Fatalf("one delta submit on the bound thread: asks=%d fresh=%v", fake.asks, fake.freshFlags)
	}
	if !strings.Contains(fake.prompts[0], "get_weather") || !strings.Contains(fake.prompts[0], "what tool can you use?") {
		t.Fatalf("final must remind new tools and carry the question: %q", fake.prompts[0][:200])
	}
	if got := h.sessions["sys"].URL; got != "https://gemini.google.com/app/THREAD" {
		t.Fatalf("thread moved: %s", got)
	}
}

func TestSystemPlusUserDivergenceReseeds(t *testing.T) {
	// System change AND a rewritten user message: not system-only, must
	// reseed fresh rather than silently continue.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW3", mintURL: "https://gemini.google.com/app/NEW3"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fine"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"sys2": {Messages: []message{
			{Role: "system", Content: "trimmed instructions"},
			{Role: "user", Content: "hi"},
		}, URL: "https://gemini.google.com/app/THREAD"}}}
	h.gate <- struct{}{}
	incoming := []message{
		{Role: "system", Content: "full instructions " + strings.Repeat("s", 5000)},
		{Role: "user", Content: "something else entirely"},
	}
	w := postJSON(t, h, "sys2", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("system+user divergence: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("non-system divergence must start a new chat: fresh=%v", fake.freshFlags)
	}
}

func TestDirtyAdoptsCompletedTurnAndStaysOnThread(t *testing.T) {
	// Interrupted turn whose answer completed: one bounded adopt commits
	// it and the new request continues delta on the SAME thread — no
	// replay, no new chat. Nothing is resubmitted either way.
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/OLD"}
	fake.readTurn = func(url, prompt string) (gemini.TurnResult, error) {
		return gemini.TurnResult{Answer: "adopted answer"}, nil
	}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "followup answer"
	}
	pending := []message{{Role: "user", Content: "q0"}, {Role: "user", Content: "q-pending"}}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"s": {Messages: []message{{Role: "user", Content: "q0"}},
			Pending: true, PendingPrompt: "q-pending", PendingMessages: pending,
			URL: "https://gemini.google.com/app/OLD"}}}
	h.gate <- struct{}{}
	incoming := append(append([]message{}, pending...),
		message{Role: "assistant", Content: "adopted answer"},
		message{Role: "user", Content: "followup"})
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "followup answer") {
		t.Fatalf("adopted continuation: %d %s", w.Code, w.Body.String())
	}
	if fake.reads != 1 {
		t.Fatalf("exactly one bounded adopt attempt: %d", fake.reads)
	}
	if fake.asks != 1 || len(fake.freshFlags) != 1 || fake.freshFlags[0] {
		t.Fatalf("one delta submit on the same thread: asks=%d fresh=%v", fake.asks, fake.freshFlags)
	}
	s := h.sessions["s"]
	if s.Pending || s.URL != "https://gemini.google.com/app/OLD" {
		t.Fatalf("adopted session must be clean on its thread: %+v", s)
	}
	if s.ReseedEvidence != "" || s.ReseedFrom != "" {
		t.Fatalf("adoption must not reseed: %+v", s)
	}
	if n := len(s.Messages); n != len(incoming)+1 || s.Messages[n-1].Content != "followup answer" {
		t.Fatalf("transcript must extend through the followup: %+v", s.Messages)
	}
}

func compactionBase() (saved, incoming []message) {
	saved = []message{{Role: "system", Content: "Bot instructions. Prior summarized context follows."}}
	for i := 1; i <= 5; i++ {
		saved = append(saved,
			message{Role: "user", Content: "question"},
			message{Role: "assistant", Content: "answer"})
	}
	saved = append(saved, message{Role: "user", Content: "latest question"})
	// Rewrite only the tail (index 10 of 12): the stored thread already
	// holds everything before the summary.
	incoming = append(append([]message{}, saved[:10]...),
		message{Role: "assistant", Content: "compacted summary of the tail"},
		message{Role: "user", Content: "followup after compaction"})
	return saved, incoming
}

func TestCompactionAppendStaysOnThread(t *testing.T) {
	saved, incoming := compactionBase()
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/OLD"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "appended answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"s": {Messages: saved, URL: "https://gemini.google.com/app/OLD"}}}
	h.gate <- struct{}{}
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/OLD",
		PromptHash: promptHash("prior"), Bytes: 5, Chunk: 1, Chunks: 1, AnswerHash: "abc"})
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "appended answer") {
		t.Fatalf("append continuation: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) != 2 || fake.freshFlags[0] || fake.freshFlags[1] {
		t.Fatalf("tail must append on the stored thread, no new chat: fresh=%v asks=%d", fake.freshFlags, fake.asks)
	}
	s := h.sessions["s"]
	if s.URL != "https://gemini.google.com/app/OLD" {
		t.Fatalf("thread must be kept: %q", s.URL)
	}
	if !strings.Contains(s.ReseedEvidence, "compaction-append") || !strings.Contains(s.ReseedEvidence, "same-thread") {
		t.Fatalf("append evidence missing: %q", s.ReseedEvidence)
	}
	if n := len(s.Messages); n != len(incoming)+1 || s.Messages[n-1].Content != "appended answer" {
		t.Fatalf("journal must follow the compacted history: %+v", s.Messages)
	}
	if fake.reads != 0 || fake.latests != 0 {
		t.Fatalf("append performs no recovery reads: exact=%d latest=%d", fake.reads, fake.latests)
	}
}

func TestCompactionEarlyMismatchStillReseeds(t *testing.T) {
	saved, _ := compactionBase()
	incoming := append([]message{{Role: "system", Content: "Bot instructions. Prior summarized context follows."},
		{Role: "user", Content: "rewritten from the start"},
		{Role: "assistant", Content: "compacted summary of the tail"},
		{Role: "user", Content: "followup after compaction"}}, saved[4:]...)
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"s": {Messages: saved, URL: "https://gemini.google.com/app/OLD"}}}
	h.gate <- struct{}{}
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/OLD",
		PromptHash: promptHash("prior"), Bytes: 5, Chunk: 1, Chunks: 1, AnswerHash: "abc"})
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("early rewrite must reseed: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("early divergence must mint a new chat: fresh=%v", fake.freshFlags)
	}
	if h.sessions["s"].URL == "https://gemini.google.com/app/OLD" {
		t.Fatal("reseed must abandon the old thread")
	}
}

func TestCompactionWithoutSentProofStillReseeds(t *testing.T) {
	saved, incoming := compactionBase()
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, gate: make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions:  map[string]*session{"s": {Messages: saved, URL: "https://gemini.google.com/app/OLD"}}}
	h.gate <- struct{}{}
	// No sent-log entry: nothing proves this session drove the stored
	// thread, so the tail must not be appended to it.
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("unproven thread must reseed: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("unproven thread must mint a new chat: fresh=%v", fake.freshFlags)
	}
}

func TestCompactionOnDirtySessionStillReseeds(t *testing.T) {
	saved, incoming := compactionBase()
	fake := &fakeRecoveryBackend{urlNow: "https://gemini.google.com/app/NEW"}
	fake.readTurn = func(url, prompt string) (gemini.TurnResult, error) {
		return gemini.TurnResult{}, structuralMismatch()
	}
	fake.answerFor = func(prompt string) string {
		if strings.Contains(prompt, "HISTORY IMPORT") || strings.Contains(prompt, "PRIOR EXECUTION RECORD") {
			return "ACK"
		}
		return "fresh answer"
	}
	h := &handler{model: "gemini-web", client: fake, maxChars: 200000, adoptWait: time.Millisecond,
		gate:      make(chan struct{}, 1),
		statePath: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]*session{"s": {Messages: saved, URL: "https://gemini.google.com/app/OLD",
			Pending: true, PendingPrompt: "uncertain prompt",
			PendingMessages: append(append([]message{}, saved...), message{Role: "user", Content: "uncertain prompt"})}}}
	h.gate <- struct{}{}
	h.appendSent(sentEntry{Event: "completed", Session: "s", URL: "https://gemini.google.com/app/OLD",
		PromptHash: promptHash("prior"), Bytes: 5, Chunk: 1, Chunks: 1, AnswerHash: "abc"})
	w := postJSON(t, h, "s", chatRequest{Model: "gemini-web", Messages: incoming})
	if w.Code != http.StatusOK {
		t.Fatalf("dirty compaction must reseed: %d %s", w.Code, w.Body.String())
	}
	if len(fake.freshFlags) == 0 || !fake.freshFlags[0] {
		t.Fatalf("uncertain thread must never be appended to: fresh=%v", fake.freshFlags)
	}
	if h.sessions["s"].URL == "https://gemini.google.com/app/OLD" {
		t.Fatal("dirty reseed must abandon the old thread")
	}
}

func recoverTestHandler(fake *fakeRecoveryBackend, s *session) *handler {
	h := &handler{model: "gemini-web", client: fake, gate: make(chan struct{}, 1),
		statePath: filepath.Join(os.TempDir(), "gemini-recover-test-state.json"),
		sessions:  map[string]*session{}}
	if s != nil {
		h.sessions["s"] = s
	}
	h.gate <- struct{}{}
	return h
}

func postRecover(t *testing.T, h *handler, sid string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/sessions/recover", nil)
	if sid != "" {
		r.Header.Set("X-Session-Id", sid)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}

func TestRecoverAdoptsPendingTurn(t *testing.T) {
	req := chatRequest{Model: "gemini-web", Messages: []message{{Role: "user", Content: "hello"}}}
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			if url != "https://gemini.google.com/app/abc" || prompt != "SUBMITTED-PROMPT" {
				return gemini.TurnResult{}, structuralMismatch()
			}
			return gemini.TurnResult{Answer: "adopted answer", URL: url}, nil
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "", gemini.TurnResult{}, errors.New("unused on the adoption path")
		},
	}
	h := recoverTestHandler(fake, &session{Pending: true, PendingPrompt: "SUBMITTED-PROMPT",
		URL:             "https://gemini.google.com/app/abc",
		PendingMessages: req.Messages, PendingReqHash: reqHash(&req)})
	w := postRecover(t, h, "s", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("adoptable pending must recover: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("recover body must be JSON: %v", err)
	}
	if body["recovered"] != true || body["session"] != "s" {
		t.Fatalf("recover body must confirm the session: %v", body)
	}
	if _, ok := body["cached_tool_calls"].(float64); !ok {
		t.Fatalf("recover body must carry cached_tool_calls for the shared report: %v", body)
	}
	if _, ok := body["response_bytes"].(float64); !ok {
		t.Fatalf("recover body must carry response_bytes for the shared report: %v", body)
	}
	s := h.sessions["s"]
	if s.Pending || s.LastAnswer != "adopted answer" {
		t.Fatalf("recover must commit the adopted turn: %+v", s)
	}
	if fake.asks != 0 {
		t.Fatalf("recovery must never submit: asks=%d", fake.asks)
	}
}

func TestRecoverRefusesWithoutAdoptableAnswer(t *testing.T) {
	fake := &fakeRecoveryBackend{
		readTurn: func(url, prompt string) (gemini.TurnResult, error) {
			return gemini.TurnResult{}, errors.New("stored thread answer is not a finished rendering")
		},
		readLatest: func(url string) (string, gemini.TurnResult, error) {
			return "", gemini.TurnResult{}, errors.New("stored thread holds no complete assistant answer to adopt")
		},
	}
	h := recoverTestHandler(fake, &session{Pending: true, PendingPrompt: "SUBMITTED-PROMPT",
		URL:             "https://gemini.google.com/app/abc",
		PendingMessages: []message{{Role: "user", Content: "hello"}}})
	h.adoptWait = time.Millisecond
	w := postRecover(t, h, "s", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("unadoptable pending must refuse 409: %d %s", w.Code, w.Body.String())
	}
	if !h.sessions["s"].Pending {
		t.Fatal("refused recovery must keep the uncertainty")
	}
}

func TestRecoverGuards(t *testing.T) {
	fake := &fakeRecoveryBackend{}
	newHandler := func() *handler { return recoverTestHandler(fake, nil) }

	w := postRecover(t, newHandler(), "unknown-session", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown session must refuse 409: %d", w.Code)
	}

	h := newHandler()
	h.sessions["clean"] = &session{Messages: []message{{Role: "user", Content: "hi"}}}
	w = postRecover(t, h, "clean", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no uncertain pending turn") {
		t.Fatalf("clean session must refuse 409: %d %s", w.Code, w.Body.String())
	}

	r := httptest.NewRequest("GET", "/v1/sessions/recover", nil)
	r.Header.Set("X-Session-Id", "s")
	w = httptest.NewRecorder()
	newHandler().routes().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("recover requires POST: %d", w.Code)
	}

	w = postRecover(t, newHandler(), "s", map[string]string{"Origin": "https://gemini.google.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("browser-originated recovery must be forbidden: %d", w.Code)
	}

	w = postRecover(t, newHandler(), "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing session must refuse 400: %d", w.Code)
	}

	w = postRecover(t, newHandler(), "all", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("wildcard session must refuse 400: %d", w.Code)
	}

	busy := newHandler()
	<-busy.gate // drain: adapter busy
	busy.sessions["s"] = &session{Pending: true, PendingPrompt: "x", URL: "https://gemini.google.com/app/abc",
		PendingMessages: []message{{Role: "user", Content: "x"}}}
	w = postRecover(t, busy, "s", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy adapter must refuse 503: %d", w.Code)
	}
	if !busy.sessions["s"].Pending {
		t.Fatal("busy refusal must change nothing")
	}
}
