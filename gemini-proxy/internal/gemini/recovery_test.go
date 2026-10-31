package gemini

import (
	"strings"
	"testing"
)

func TestPromptKeyAcceptsRenderingWhitespace(t *testing.T) {
	base := "Reminder: available functions\nshell, read, write.\nReply with ONLY wrappers."
	expanded := "Reminder:   available   functions\n\n\nshell,   read,  write.\n\nReply\twith ONLY   wrappers.  \n"
	if PromptKey(expanded) != PromptKey(base) {
		t.Fatalf("whitespace rendering must identify: %q vs %q", PromptKey(expanded), PromptKey(base))
	}
	for _, other := range []string{
		strings.Replace(base, "shell", "exec", 1),                                               // reworded
		"Reply with ONLY wrappers. Reminder: available functions\nshell, read, write.",          // reordered
		"Reminder: available functions\nshell, read.",                                           // dropped words
		"Reminder: available functions\nshell, read, write.\nReply with ONLY wrappers, please.", // added words
	} {
		if PromptKey(other) == PromptKey(base) {
			t.Fatalf("different message must not identify: %q", other)
		}
	}
	if PromptKey("") != "" || PromptKey("   \n\t ") != "" {
		t.Fatal("blank input must key empty so adoption refuses it")
	}
}

func TestPromptWindowParity(t *testing.T) {
	edge := strings.Repeat("e", 200)
	mk := func(mid string) string { return edge + " " + mid + " " + edge }
	long := mk(strings.Repeat("m ", 300))
	rewritten := mk(strings.Repeat("x ", 300))
	if !promptWindow(rewritten, long) {
		t.Fatal("interior rewrite of a long prompt must identify")
	}
	if promptWindow(mk(strings.Repeat("m ", 300))+" tail-change", long) {
		t.Fatal("changed trailing edge must not identify")
	}
	if promptWindow("tail-change "+mk(strings.Repeat("m ", 300)), long) {
		t.Fatal("changed leading edge must not identify")
	}
	far := mk(strings.Repeat("m ", 600))
	if promptWindow(far, long) {
		t.Fatal("length beyond an eighth must not identify")
	}
	short := edge + " hi " + edge
	if promptWindow(short+"!", short) {
		t.Fatal("short prompts must not use the window tier")
	}
}
