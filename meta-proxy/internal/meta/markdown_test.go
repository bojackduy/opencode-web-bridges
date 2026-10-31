package meta

import (
	"strings"
	"testing"
)

func TestAnswerToMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		html, text string
		wantSub    []string
	}{
		{
			"prose plus structure",
			`<div class="text-response"><p>Hello <strong>Duy</strong>, try:</p><ul><li>first <code>go build</code></li><li>second <a href="https://x.test">docs</a></li></ul></div>`,
			"Hello Duy, try:\nfirst go build\nsecond docs",
			[]string{"**Duy**", "- first `go build`", "[docs](https://x.test)"},
		},
		{
			"headings and code fence",
			`<div><h2>Deploy</h2><pre><code>go build ./...</code></pre></div>`,
			"Deploy\ngo build ./...",
			[]string{"## Deploy", "```\ngo build ./...\n```"},
		},
	} {
		got := answerToMarkdown(tc.html, tc.text)
		for _, sub := range tc.wantSub {
			if !strings.Contains(got, sub) {
				t.Fatalf("%s: want %q in %q", tc.name, sub, got)
			}
		}
	}
}

func TestAnswerToMarkdownBypassesWrappers(t *testing.T) {
	raw := `<opencode_bridge_call>{"name":"shell","arguments":{"command":"uname -a"}}</opencode_bridge_call>`
	htmlFrag := `<div class="text-response"><p>` + raw + `</p></div>`
	if got := answerToMarkdown(htmlFrag, raw); got != raw {
		t.Fatalf("wrapper-bearing answers must pass through untouched, got %q", got)
	}
}

func TestAnswerToMarkdownFallsBackToPlain(t *testing.T) {
	if got := answerToMarkdown("", "just text"); got != "just text" {
		t.Fatalf("empty HTML must fall back to plain text, got %q", got)
	}
}

func TestIsMetaErrorState(t *testing.T) {
	for _, s := range []string{
		"Something went wrong. Please try again.",
		"something went wrong",
		"  PLEASE TRY AGAIN  ",
	} {
		if !isMetaErrorState(s) {
			t.Fatalf("must detect error state: %q", s)
		}
	}
	for _, s := range []string{
		"", "Greeting", "Something went right", "try again later with a real question answered",
	} {
		if isMetaErrorState(s) {
			t.Fatalf("must not flag model output: %q", s)
		}
	}
}
