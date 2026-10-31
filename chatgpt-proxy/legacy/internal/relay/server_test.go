package relay_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatgpt-proxy/legacy/internal/agent"
	"chatgpt-proxy/legacy/internal/relay"
	"chatgpt-proxy/legacy/internal/tools"
)

func startTestRelay(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s := relay.New(relay.Config{
		Addr:         ":0",
		AgentToken:   "agent-secret",
		ClientToken:  "client-secret",
		AgentTimeout: 5 * time.Second,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

func startTestAgent(t *testing.T, relayURL string) {
	t.Helper()
	wsURL := strings.Replace(relayURL, "http://", "ws://", 1) + "/ws/agent?token=agent-secret"
	a := &agent.Agent{
		RelayURL: wsURL,
		Token:    "agent-secret",
		Name:     "test-agent",
		Tools:    tools.NewTools(nil),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run()
	}()
	t.Cleanup(func() {
		a.Stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	waitForAgents(t, relayURL, 1)
}

func waitForAgents(t *testing.T, relayURL string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(relayURL + "/healthz")
		if err == nil {
			var body struct {
				Agents int `json:"agents"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if body.Agents >= want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d agent(s) to connect", want)
}

func postJSON(t *testing.T, url, token string, req map[string]any, accept string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	return resp, buf.String()
}

func TestInitialize(t *testing.T) {
	ts, _ := startTestRelay(t)
	startTestAgent(t, ts.URL)

	resp, body := postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "test", "version": "1"}},
	}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Error("expected Mcp-Session-Id header on initialize")
	}
	var out struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("bad JSON %s: %v", body, err)
	}
	if out.Result.ServerInfo.Name != "chatgpt-proxy" {
		t.Errorf("server name = %q", out.Result.ServerInfo.Name)
	}
}

func TestToolsListAndCall(t *testing.T) {
	ts, _ := startTestRelay(t)
	startTestAgent(t, ts.URL)

	// tools/list forwards to the agent.
	_, body := postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}, "")
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("bad JSON %s: %v", body, err)
	}
	names := map[string]bool{}
	for _, tl := range list.Result.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"read_file", "search_text", "apply_patch", "shell", "system_info"} {
		if !names[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}

	// tools/call read_file on a temp file.
	dir := t.TempDir()
	f := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, body = postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": f},
		},
	}, "")
	var call struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &call); err != nil {
		t.Fatalf("bad JSON %s: %v", body, err)
	}
	if call.Result.IsError {
		t.Fatalf("tool returned error: %s", body)
	}
	if len(call.Result.Content) == 0 || !strings.Contains(call.Result.Content[0].Text, "line2") {
		t.Errorf("unexpected content: %s", body)
	}
}

func TestPolicyBlocksDestructiveShell(t *testing.T) {
	ts, _ := startTestRelay(t)
	startTestAgent(t, ts.URL)

	_, body := postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "shell",
			"arguments": map[string]any{"command": "rm -rf /tmp/whatever"},
		},
	}, "")
	var call struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &call); err != nil {
		t.Fatalf("bad JSON %s: %v", body, err)
	}
	if !call.Result.IsError {
		t.Fatalf("expected destructive command to be blocked, got: %s", body)
	}
	if len(call.Result.Content) == 0 || !strings.Contains(call.Result.Content[0].Text, "blocked by policy") {
		t.Errorf("expected policy message, got: %s", body)
	}

	// A safe command goes through.
	_, body = postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name":      "shell",
			"arguments": map[string]any{"command": "echo hello-agent"},
		},
	}, "")
	var okCall struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &okCall); err != nil {
		t.Fatalf("bad JSON %s: %v", body, err)
	}
	if okCall.Result.IsError {
		t.Fatalf("safe command should succeed: %s", body)
	}
	if len(okCall.Result.Content) == 0 || !strings.Contains(okCall.Result.Content[0].Text, "hello-agent") {
		t.Errorf("expected echo output, got: %s", body)
	}
}

func TestSSEResponse(t *testing.T) {
	ts, _ := startTestRelay(t)
	startTestAgent(t, ts.URL)

	resp, body := postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "ping",
	}, "text/event-stream")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(body, "event: message") || !strings.Contains(body, `"result":{}`) {
		t.Errorf("unexpected SSE body: %s", body)
	}
}

func TestNoAgentConnected(t *testing.T) {
	ts, _ := startTestRelay(t) // no agent started

	_, body := postJSON(t, ts.URL+"/mcp", "client-secret", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}, "")
	if !strings.Contains(body, "no agent is connected") {
		t.Errorf("expected no-agent error, got: %s", body)
	}
}
