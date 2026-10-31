package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Exercise the production DOM probe through the adapter's actual validation,
// SSE, cache and persisted recovery paths, rather than a second converter.
func TestRenderedMarkdownProtocolAndPersistence(t *testing.T) {
	js, err := os.ReadFile("../../internal/chatgpt/response.js")
	if err != nil {
		t.Fatal(err)
	}
	alloc, stop := chromedp.NewExecAllocator(context.Background(), chromedp.DefaultExecAllocatorOptions[:]...)
	defer stop()
	tab, stopTab := chromedp.NewContext(alloc)
	defer stopTab()
	tab, cancel := context.WithTimeout(tab, 45*time.Second)
	defer cancel()
	extract := func(body string) string {
		t.Helper()
		html, _ := json.Marshal(`<div data-message-author-role="user">request</div><div data-message-author-role="assistant" data-message-id="answer"><div class="markdown">` + body + `</div></div>`)
		var st struct {
			Answer string `json:"lastAssistant"`
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(`document.body.innerHTML = `+string(html), nil), chromedp.Evaluate(string(js)+`([])`, &st)); err != nil {
			t.Fatal(err)
		}
		return st.Answer
	}
	raw := `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Ha\\noi_*"}}</opencode_bridge_call>`
	answer := extract(`<p>` + strings.ReplaceAll(strings.ReplaceAll(raw, "<", "&lt;"), ">", "&gt;") + `</p>`)
	if answer != raw {
		t.Fatalf("literal wrapper changed: %q", answer)
	}
	for _, stream := range []bool{false, true} {
		req := weatherReq()
		req.Stream = stream
		h := testHandler(&fakeBackend{answers: []string{answer}})
		w := request(h, "wire", req)
		if w.Code != 200 || strings.Contains(w.Body.String(), "opencode_bridge_call") || !strings.Contains(w.Body.String(), "get_weather") {
			t.Fatalf("protocol validation/SSE: %d %s", w.Code, w.Body.String())
		}
	}
	if extract(`<p>ACK</p>`) != "ACK" {
		t.Fatal("history acknowledgement changed")
	}
	markdown := extract(`<h2>Result</h2><p>Use <code>x</code>.</p><table><tr><th>A</th></tr><tr><td>B</td></tr></table>`)
	if markdown != "## Result\n\nUse `x`.\n\n| A |\n| --- |\n| B |" {
		t.Fatalf("rich body changed: %q", markdown)
	}
	req := weatherReq()
	req.Tools, req.ToolChoice = nil, nil
	f := &inspectBackend{fakeBackend: &fakeBackend{}, answer: markdown}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	turns1, err1 := h.buildTurns(req, req.Messages, toolReminder(req))
	if err1 != nil {
		t.Fatalf("buildTurns: %v", err1)
	}
	f.expected = turns1[0].prompt
	h.sessions["recover"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: f.expected, ToolsFP: toolsFingerprint(req.Tools)}
	if w := recoveryRequest(h, &req); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	got := assistant(t, request(loaded, "recover", req))
	if got.Content != markdown || len(f.prompts) != 0 {
		t.Fatalf("recovery/persisted cache Markdown mismatch: %#v", got)
	}
}

func TestRenderPreambleMermaidSubset(t *testing.T) {
	for _, want := range []string{
		"```mermaid", "flowchart", "sequenceDiagram", "stateDiagram-v2",
		"gantt", "gitGraph", "timeline", "classDiagram", "mindmap",
		"markdown table or bullet list", "ASCII/Unicode art",
	} {
		if !strings.Contains(renderPreamble, want) {
			t.Fatalf("preamble missing %q", want)
		}
	}
	for _, banned := range []string{"SVG", "HTML", "CSS", "canvas"} {
		if !strings.Contains(renderPreamble, banned) {
			t.Fatalf("preamble must still ban %q", banned)
		}
	}
	if strings.Contains(renderPreamble, "Never output") && strings.Contains(renderPreamble, "Mermaid markup") {
		t.Fatal("preamble still bans Mermaid outright")
	}
}
