package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Captured from installed OpenCode 1.18.35 through an isolated local endpoint,
// not handwritten SDK guesses. Text values are stable substitution tokens;
// the harmless fixture path is relocated. Roles, ordering and encodings remain.
const capturedSDKRequests = `[
 {"lane":"title","messages":[{"role":"system","content":"sanitized-text-0"},{"role":"user","content":"sanitized-text-1"},{"role":"user","content":"sanitized-text-2"}]},
 {"lane":"main","messages":[{"role":"system","content":"sanitized-text-3"},{"role":"user","content":"sanitized-text-4"}]},
 {"lane":"main","messages":[{"role":"system","content":"sanitized-text-3"},{"role":"user","content":"sanitized-text-4"},{"role":"assistant","content":"","tool_calls":[{"id":"call_fixture_read","type":"function","function":{"name":"read","arguments":"{\"filePath\":\"/isolated/fixture.txt\"}"}}]},{"role":"tool","tool_call_id":"call_fixture_read","content":"sanitized-text-6"}]}
]`

func TestCapturedSDKTitleAndToolRepresentations(t *testing.T) {
	t.Run("installed v2 compact DCP ordinary followup and restart", func(t *testing.T) {
		// Exact 637-byte metadata suffix from the affected v2 wire transcript.
		const suffix = "\n\n<dcp-system-reminder>\nEvaluate the conversation for compressible ranges.\n\nIf any messages are cleanly closed and unlikely to be needed again, use the compress tool on them.\nIf direction has shifted, compress earlier ranges that are now less relevant.\n\nThe goal is to filter noise and distill key information so context accumulation stays under control.\nKeep active context uncompressed.\n\nCompressed block context:\n- Active compressed blocks in this session: 0 (none)\n- If your selected compression range includes any listed block, include each required placeholder exactly once in the summary using `@b1@`.\n</dcp-system-reminder>\n\n@33@"
		if len(suffix) != 637 {
			t.Fatal("captured suffix shape changed")
		}
		f := &fakeBackend{answers: []string{"first answer", "second answer", "third answer"}}
		h := testHandler(f)
		h.statePath = t.TempDir() + "/state.json"
		req := completionsRequest{Messages: []message{{Role: "user", Content: "first question"}}, Tools: weatherReq().Tools, ToolChoice: "auto"}
		a := assistant(t, request(h, "compact-v2", req))
		url := h.sessions["compact-v2"].URL
		a.Content = contentText(a) + suffix
		req.Messages = append(req.Messages, a, message{Role: "user", Content: "second question"})
		b := assistant(t, request(h, "compact-v2", req))
		reloaded := testHandler(f)
		reloaded.statePath = h.statePath
		if err := reloaded.load(); err != nil {
			t.Fatal(err)
		}
		b.Content = contentText(b) + suffix
		req.Messages = append(req.Messages, b, message{Role: "user", Content: "third question"})
		assistant(t, request(reloaded, "compact-v2", req))
		if reloaded.sessions["compact-v2"].URL != url || len(f.prompts) != 3 || f.urls[1] != url || f.urls[2] != url || strings.Contains(f.prompts[1], "first question") || strings.Contains(f.prompts[2], "second question") || strings.Contains(f.prompts[1], toolPreamble) || strings.Contains(f.prompts[2], toolPreamble) {
			t.Fatal("ordinary compact-metadata followup/reopen rotated or replayed")
		}
		for _, changed := range []message{
			{Role: "assistant", Content: "first answer edited" + suffix},
			{Role: "assistant", Content: "first answer\n\n@33@"},
			{Role: "assistant", Content: "first answer" + strings.Replace(suffix, "\n\n@33@", "\n\n@b1@", 1)},
			{Role: "assistant", Content: "first answer" + suffix + " intentional tail"},
			{Role: "user", Content: "first answer" + suffix},
		} {
			if prefix([]message{changed}, []message{{Role: "assistant", Content: "first answer"}}) {
				t.Fatal("genuine edit or unrecognized metadata normalized")
			}
		}
	})
	var captured []struct {
		Lane     string    `json:"lane"`
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(capturedSDKRequests), &captured); err != nil {
		t.Fatal(err)
	}
	// Without lane isolation the real title request differs at the very first
	// system message; neither tool count nor the shared session ID identifies it.
	if got := firstMismatch(captured[1].Messages, captured[0].Messages); got == nil || got.Index != 0 || got.Field != "content" {
		t.Fatalf("title mismatch %+v", got)
	}
	expected := append([]message{}, captured[2].Messages[:3]...)
	expected[2].ToolCalls = append([]toolCall(nil), expected[2].ToolCalls...)
	// Fixture endpoint returned spaced JSON; real SDK serialized minified JSON.
	expected[2].ToolCalls[0].Function.Arguments = `{"filePath": "/isolated/fixture.txt"}`
	if got := firstMismatch(captured[2].Messages, expected); got != nil {
		t.Fatalf("SDK JSON formatting %+v", got)
	}
	// Same shared header, separate lane: auxiliary cannot become main's prefix.
	f := &fakeBackend{}
	h := testHandler(f)
	assistant(t, laneRequest(h, "captured-session", captured[0].Lane, completionsRequest{Messages: captured[0].Messages}))
	a := assistant(t, laneRequest(h, "captured-session", captured[1].Lane, completionsRequest{Messages: captured[1].Messages}))
	url := h.sessions["captured-session"].URL
	msgs := append(append([]message{}, captured[1].Messages...), a, message{Role: "user", Content: "followup"})
	assistant(t, laneRequest(h, "captured-session", "title", completionsRequest{Messages: captured[0].Messages}))
	assistant(t, laneRequest(h, "captured-session", "main", completionsRequest{Messages: msgs}))
	if h.sessions["captured-session"].URL != url {
		t.Fatal("captured title request lost main URL")
	}
}
