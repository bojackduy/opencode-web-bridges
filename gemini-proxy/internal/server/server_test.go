package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bojackduy/gemini-proxy/internal/gemini"
	"github.com/bojackduy/gemini-proxy/internal/mcp"
)

type fakeGemini struct {
	answer string
	err    error
	status *gemini.Status
	stErr  error
}

func (f *fakeGemini) Ask(_ context.Context, _ string, _ bool, _ time.Duration) (string, error) {
	return f.answer, f.err
}

func (f *fakeGemini) Status(_ context.Context) (*gemini.Status, error) {
	return f.status, f.stErr
}

func rpc(t *testing.T, h *Handler, method string, params any, id int) mcp.Response {
	t.Helper()
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		rawParams = b
	}
	idRaw, _ := json.Marshal(id)
	req, _ := json.Marshal(mcp.Request{JSONRPC: "2.0", ID: idRaw, Method: method, Params: rawParams})
	out := h.Handle(context.Background(), req)
	var resp mcp.Response
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("bad response: %v (%s)", err, out)
	}
	return resp
}

func TestInitialize(t *testing.T) {
	h := NewHandler(&fakeGemini{}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: "2025-06-18",
		ClientInfo:      mcp.ClientInfo{Name: "test", Version: "1"},
	}, 1)
	if resp.Error != nil {
		t.Fatalf("initialize error: %v", resp.Error)
	}
	var init mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ServerInfo.Name != "gemini-proxy" {
		t.Fatalf("server name: %q", init.ServerInfo.Name)
	}
}

func TestToolsList(t *testing.T) {
	h := NewHandler(&fakeGemini{}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodToolsList, nil, 2)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %v", resp.Error)
	}
	var list mcp.ToolListResult
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	if !names[ToolAsk] || !names[ToolStatus] {
		t.Fatalf("missing tools: %+v", list.Tools)
	}
}

func TestAskGemini(t *testing.T) {
	h := NewHandler(&fakeGemini{answer: "hello from gemini"}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodToolsCall,
		mcp.ToolCallParams{Name: ToolAsk, Arguments: json.RawMessage(`{"prompt":"hi"}`)}, 3)
	if resp.Error != nil {
		t.Fatalf("rpc error: %v", resp.Error)
	}
	var result mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "hello from gemini" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAskGeminiRequiresPrompt(t *testing.T) {
	h := NewHandler(&fakeGemini{}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodToolsCall,
		mcp.ToolCallParams{Name: ToolAsk, Arguments: json.RawMessage(`{"prompt":""}`)}, 4)
	if resp.Error != nil {
		t.Fatalf("rpc error: %v", resp.Error)
	}
	var result mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty prompt")
	}
}

func TestAskGeminiNotLoggedIn(t *testing.T) {
	h := NewHandler(&fakeGemini{err: gemini.ErrNotLoggedIn}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodToolsCall,
		mcp.ToolCallParams{Name: ToolAsk, Arguments: json.RawMessage(`{"prompt":"hi"}`)}, 5)
	if resp.Error != nil {
		t.Fatalf("rpc error: %v", resp.Error)
	}
	var result mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !result.IsError || len(result.Content) == 0 {
		t.Fatal("expected actionable login error")
	}
}

func TestStatusTool(t *testing.T) {
	h := NewHandler(&fakeGemini{status: &gemini.Status{LoggedIn: true, Composer: true}}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodToolsCall,
		mcp.ToolCallParams{Name: ToolStatus, Arguments: json.RawMessage(`{}`)}, 6)
	if resp.Error != nil {
		t.Fatalf("rpc error: %v", resp.Error)
	}
	var result mcp.ToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("status error: %+v", result)
	}
}

func TestUnknownTool(t *testing.T) {
	h := NewHandler(&fakeGemini{}, "/tmp/x")
	resp := rpc(t, h, mcp.MethodToolsCall,
		mcp.ToolCallParams{Name: "nope", Arguments: json.RawMessage(`{}`)}, 7)
	if resp.Error == nil {
		t.Fatal("expected method-not-found for unknown tool")
	}
}

func TestParseError(t *testing.T) {
	h := NewHandler(&fakeGemini{}, "/tmp/x")
	out := h.Handle(context.Background(), []byte(`{bad json`))
	var resp mcp.Response
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != mcp.CodeParseError {
		t.Fatalf("expected parse error: %+v", resp)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	h := NewHandler(&fakeGemini{}, "/tmp/x")
	out := h.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"ping"}`))
	if out != nil {
		t.Fatalf("notification must yield no response: %s", out)
	}
}

var _ = errors.Is
