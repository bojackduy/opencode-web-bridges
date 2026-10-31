package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func laneRequest(h *handler, id, lane string, req completionsRequest) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("X-Session-Id", id)
	r.Header.Set("X-OpenCode-Request-Lane", lane)
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}

func TestAuxiliaryLanesCannotContaminateMain(t *testing.T) {
	for _, lane := range []string{"title", "summary", "compaction", "auxiliary"} {
		t.Run(lane, func(t *testing.T) {
			f := &fakeBackend{}
			h := testHandler(f)
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			req := completionsRequest{Messages: []message{{Role: "user", Content: "main first"}}}
			a := assistant(t, laneRequest(h, "session", "main", req))
			url := h.sessions["session"].URL
			before, _ := json.Marshal(h.sessions["session"])
			f.err = errors.New("auxiliary uncertain submission")
			if w := laneRequest(h, "session", lane, completionsRequest{Messages: []message{{Role: "user", Content: "auxiliary prompt"}}}); w.Code != 502 {
				t.Fatalf("status=%d", w.Code)
			}
			after, _ := json.Marshal(h.sessions["session"])
			if !bytes.Equal(before, after) {
				t.Fatal("auxiliary changed main journal")
			}
			f.err = nil
			h2 := testHandler(f)
			h2.statePath = h.statePath
			if err := h2.load(); err != nil {
				t.Fatal(err)
			}
			req.Messages = append(req.Messages, a, message{Role: "user", Content: "main followup"})
			assistant(t, laneRequest(h2, "session", "main", req))
			if f.urls[len(f.urls)-1] != url || h2.sessions["session"].URL != url {
				t.Fatal("main thread lost across auxiliary/restart")
			}
		})
	}
}

func TestUntaggedTitleRequestCannotRotateMain(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := completionsRequest{Messages: []message{{Role: "user", Content: "main first"}}}
	a := assistant(t, request(h, "session", req))
	url := h.sessions["session"].URL
	before, _ := json.Marshal(h.sessions["session"])
	// Exact installed-host title shape: same session ID, no lane header, no
	// tools, title prefix as the first user message.
	title := completionsRequest{Messages: []message{{Role: "user", Content: "Generate a title for this conversation:\nmain first"}}}
	assistant(t, request(h, "session", title))
	after, _ := json.Marshal(h.sessions["session"])
	if !bytes.Equal(before, after) {
		t.Fatal("untagged title request changed main journal")
	}
	if len(h.sessions) != 2 {
		t.Fatal("untagged title had no isolated journal", len(h.sessions))
	}
	req.Messages = append(req.Messages, a, message{Role: "user", Content: "main followup"})
	assistant(t, request(h, "session", req))
	if h.sessions["session"].URL != url || f.urls[len(f.urls)-1] != url {
		t.Fatal("main thread lost across untagged title")
	}
	// Near-miss shapes stay on main: no exact prefix, multipart content, or
	// tool-carrying requests must not be diverted.
	for _, near := range []completionsRequest{
		{Messages: []message{{Role: "user", Content: "Generate a title for a story"}}},
		{Messages: []message{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "Generate a title for this conversation:\nmain first"}}}}},
		{Messages: []message{{Role: "user", Content: "Generate a title for this conversation:\nmain first"}}, Tools: weatherReq().Tools},
	} {
		if aux, ok := isolateUntaggedTitle("session", near, h.sessions["session"]); ok {
			t.Fatalf("near-miss title diverted to %s", aux)
		}
	}
	if aux, ok := isolateUntaggedTitle("session", title, nil); ok {
		t.Fatalf("title without established thread diverted to %s", aux)
	}
}

func TestLaneValidationAndLegacyIdentity(t *testing.T) {
	h := testHandler(&fakeBackend{})
	req := completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}}
	if laneRequest(h, "session", "unknown", req).Code != 400 || len(h.sessions) != 0 {
		t.Fatal("invalid lane accepted")
	}
	a := assistant(t, request(h, "session", req))
	req.Messages = append(req.Messages, a, message{Role: "user", Content: "followup"})
	assistant(t, laneRequest(h, "session", "main", req))
	if len(h.sessions) != 1 {
		t.Fatal("explicit main broke legacy identity")
	}
}

func TestDirtyMainDoesNotBlockAuxiliaryOrBecomeItsRecoveryTarget(t *testing.T) {
	f := &fakeBackend{err: errors.New("main uncertain"), errorURL: "https://chatgpt.com/c/uncertain"}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "main"}}}
	if laneRequest(h, "session", "main", req).Code != 502 {
		t.Fatal("expected uncertainty")
	}
	before, _ := json.Marshal(h.sessions["session"])
	f.err = nil
	assistant(t, laneRequest(h, "session", "title", req))
	for _, endpoint := range []string{"recover", "resync"} {
		r := httptest.NewRequest("POST", "/v1/sessions/"+endpoint, bytes.NewBufferString(`{"messages":[]}`))
		if endpoint == "recover" {
			r = httptest.NewRequest("POST", "/v1/sessions/"+endpoint, nil)
		}
		r.Header.Set("X-Session-Id", "session")
		r.Header.Set("X-OpenCode-Request-Lane", "title")
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatalf("%s clean auxiliary recovery status=%d", endpoint, w.Code)
		}
	}
	after, _ := json.Marshal(h.sessions["session"])
	if !bytes.Equal(before, after) {
		t.Fatal("auxiliary mutated dirty main")
	}
}
