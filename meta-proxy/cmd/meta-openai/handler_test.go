package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerRoutes(t *testing.T) {
	// Smoke test: handler routes register without panic.
	h := &handler{model: "meta-web", gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	_ = h.routes()
}

func TestHealthz(t *testing.T) {
	h := &handler{model: "meta-web", gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.sessions = map[string]*session{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rr.Code)
	}
}

func TestSessionStateUnknown(t *testing.T) {
	h := &handler{model: "meta-web", gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.sessions = map[string]*session{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/state", nil)
	req.Header.Set("X-Session-Id", "test-sid")
	h.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("state status = %d, want 200", rr.Code)
	}
}

func TestSessionStateMethodNotAllowed(t *testing.T) {
	h := &handler{model: "meta-web", gate: make(chan struct{}, 1)}
	h.gate <- struct{}{}
	h.sessions = map[string]*session{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/state", nil)
	req.Header.Set("X-Session-Id", "test-sid")
	h.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("state POST status = %d, want 405", rr.Code)
	}
}