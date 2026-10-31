package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatgpt-proxy/internal/chatgpt"
	"chatgpt-proxy/internal/mcp"
)

// fakeCG is a scripted Chatgpt implementation for dispatch tests.
type fakeCG struct {
	askErr    error
	askText   string
	askPrompt string
	status    *chatgpt.Status
}

func (f *fakeCG) Ask(_ context.Context, prompt string, _ bool, _ time.Duration) (string, error) {
	f.askPrompt = prompt
	return f.askText, f.askErr
}

func (f *fakeCG) Status(_ context.Context) (*chatgpt.Status, error) {
	return f.status, nil
}

func call(t *testing.T, h *Handler, body string) *mcp.Response {
	t.Helper()
	resp := h.Handle(context.Background(), []byte(body))
	if resp == nil {
		return nil
	}
	var out mcp.Response
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return &out
}

func TestInitialize(t *testing.T) {
	h := NewHandler(&fakeCG{}, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ServerInfo.Name != "chatgpt-proxy" {
		t.Errorf("server name = %q", res.ServerInfo.Name)
	}
	if res.Capabilities.Tools == nil {
		t.Error("expected tools capability")
	}
}

func TestToolsList(t *testing.T) {
	h := NewHandler(&fakeCG{}, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res mcp.ToolListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{ToolAsk, ToolStatus} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestAskChatgptSuccess(t *testing.T) {
	f := &fakeCG{askText: "hello from chatgpt"}
	h := NewHandler(f, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask_chatgpt","arguments":{"prompt":"say hi"}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("expected success result")
	}
	if len(res.Content) != 1 || res.Content[0].Text != "hello from chatgpt" {
		t.Errorf("content = %+v", res.Content)
	}
	if f.askPrompt != "say hi" {
		t.Errorf("prompt = %q", f.askPrompt)
	}
}

func TestAskChatgptEmptyPrompt(t *testing.T) {
	h := NewHandler(&fakeCG{}, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ask_chatgpt","arguments":{"prompt":"  "}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	var res mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected tool-level error for empty prompt")
	}
	if !strings.Contains(res.Content[0].Text, "prompt") {
		t.Errorf("error text = %q", res.Content[0].Text)
	}
}

func TestAskChatgptNotLoggedIn(t *testing.T) {
	f := &fakeCG{askErr: chatgpt.ErrNotLoggedIn}
	h := NewHandler(f, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"ask_chatgpt","arguments":{"prompt":"hi"}}}`)
	var res mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error result")
	}
	if !strings.Contains(res.Content[0].Text, "log in") {
		t.Errorf("error text = %q", res.Content[0].Text)
	}
}

func TestStatus(t *testing.T) {
	f := &fakeCG{status: &chatgpt.Status{LoggedIn: true, Composer: true}}
	h := NewHandler(f, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"chatgpt_status","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content[0].Text, `"logged_in":true`) {
		t.Errorf("status text = %q", res.Content[0].Text)
	}
}

func TestUnknownMethod(t *testing.T) {
	h := NewHandler(&fakeCG{}, t.TempDir())
	resp := call(t, h, `{"jsonrpc":"2.0","id":7,"method":"bogus/method"}`)
	if resp.Error == nil {
		t.Fatal("expected rpc error")
	}
	if resp.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("code = %d", resp.Error.Code)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	h := NewHandler(&fakeCG{}, t.TempDir())
	out := h.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if out != nil {
		t.Errorf("expected nil for notification, got %s", out)
	}
}

func TestHTTPPostSSE(t *testing.T) {
	f := &fakeCG{status: &chatgpt.Status{LoggedIn: true}}
	h := NewHandler(f, t.TempDir())
	srv := httptest.NewServer(h.NewHTTPHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chatgpt_status","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Error("missing Mcp-Session-Id header")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: message") {
		t.Errorf("body = %q", body)
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(&fakeCG{}, t.TempDir())
	srv := httptest.NewServer(h.NewHTTPHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("healthz = %q", body)
	}
}
