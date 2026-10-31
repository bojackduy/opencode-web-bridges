package gemini

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Fire-and-forget: a leftover draft from an earlier attempt is overwritten,
// never refused. The ownership table below is retained only as dead reference
// for the removed gate; it is no longer consulted by the ask path.
func TestDecideGeminiDraftOwnership(t *testing.T) {
	const prompt = "the prompt we typed"
	const target = "T1"
	const thread = "https://gemini.google.com/app/abc"
	rec := geminiDraftRecord{Prompt: prompt, Doc: prompt, Hash: draftHash(prompt), Identity: "id1", Target: target, Thread: thread}
	own := geminiDraftState{OK: true, Text: prompt, Marked: true}
	cases := []struct {
		name       string
		state      geminiDraftState
		identity   string
		target     string
		thread     string
		prompt     string
		rec        geminiDraftRecord
		wantOwned  bool
		wantReuse  bool
		wantReason string
	}{
		{"unreadable editor refuses", geminiDraftState{}, "id1", target, thread, prompt, rec, false, false, "no-quill"},
		{"empty composer types", geminiDraftState{OK: true}, "id1", target, thread, prompt, rec, true, false, "empty"},
		{"submission attempted refuses", own, "id2", target, thread, prompt, geminiDraftRecord{Prompt: prompt, Doc: prompt, Hash: draftHash(prompt), Identity: "id1", Target: target, Thread: thread, Submitted: true}, false, false, "submission-attempted"},
		{"same turn reuses", own, "id1", target, thread, prompt, rec, true, true, "same-turn"},
		{"superseded same tab and thread replaces", own, "id2", target, thread, prompt, rec, true, false, "superseded-owned-draft"},
		// The page marker alone is NOT ownership: without a matching Go record
		// the document cannot be attributed to this bridge.
		{"marker without a record refuses", geminiDraftState{OK: true, Text: prompt, Marked: true}, "id1", target, thread, prompt, geminiDraftRecord{}, false, false, "unverified-document"},
		// A matching document on a different tab or conversation is not proof.
		{"different target refuses", own, "id2", "T2", thread, prompt, rec, false, false, "other-target"},
		{"different thread refuses", own, "id2", target, "https://gemini.google.com/app/other", prompt, rec, false, false, "other-thread"},
		{"record without a target refuses", own, "id2", target, thread, prompt, geminiDraftRecord{Prompt: prompt, Doc: prompt, Hash: draftHash(prompt), Identity: "id1"}, false, false, "unverified-document"},
		// Identity reuse: the same adapter request asking for DIFFERENT text must
		// not be treated as an exact reuse.
		{"same identity different request text is superseded", own, "id1", target, thread, "a newer request", rec, true, false, "superseded-owned-draft"},
		{"same document different identity is superseded", own, "id9", target, thread, prompt, rec, true, false, "superseded-owned-draft"},
		{"user edit refuses", geminiDraftState{OK: true, Text: prompt + " plus"}, "id2", target, thread, prompt, rec, false, false, "unverified-document"},
		{"user shortened to a prefix refuses", geminiDraftState{OK: true, Text: "the prompt"}, "id2", target, thread, prompt, rec, false, false, "unverified-document"},
		{"stale record for other text refuses", geminiDraftState{OK: true, Text: "other text"}, "id2", target, thread, prompt, geminiDraftRecord{Prompt: prompt, Doc: "other different text", Hash: draftHash("other different text"), Identity: "id1", Target: target, Thread: thread}, false, false, "unverified-document"},
		{"record whose digest disagrees with its document refuses", geminiDraftState{OK: true, Text: prompt}, "id2", target, thread, prompt, geminiDraftRecord{Prompt: prompt, Doc: prompt, Hash: draftHash("something else"), Identity: "id1", Target: target, Thread: thread}, false, false, "unverified-document"},
		// A lost CDP reply leaves no Go record. The page token is sound only
		// because it names this target, this conversation and this exact text.
		{"bound page token proves ownership after a lost reply", geminiDraftState{OK: true, Text: prompt, Marked: true, MarkedText: prompt, MarkedTarget: target, MarkedThread: thread}, "id1", target, thread, prompt, geminiDraftRecord{}, true, false, "superseded-owned-draft"},
		{"bound page token reuses the same request", geminiDraftState{OK: true, Text: prompt, Marked: true, MarkedText: prompt, MarkedTarget: target, MarkedThread: thread}, "id1", target, thread, prompt, geminiDraftRecord{Prompt: prompt, Identity: "id1"}, true, true, "same-turn"},
		{"page token on another target refuses", geminiDraftState{OK: true, Text: prompt, Marked: true, MarkedText: prompt, MarkedTarget: "T2", MarkedThread: thread}, "id1", target, thread, prompt, geminiDraftRecord{}, false, false, "other-target"},
		{"page token on another thread refuses", geminiDraftState{OK: true, Text: prompt, Marked: true, MarkedText: prompt, MarkedTarget: target, MarkedThread: "https://gemini.google.com/app/other"}, "id1", target, thread, prompt, geminiDraftRecord{}, false, false, "other-thread"},
		{"page token whose text no longer matches refuses", geminiDraftState{OK: true, Text: prompt + " plus", Marked: true, MarkedText: prompt, MarkedTarget: target, MarkedThread: thread}, "id1", target, thread, prompt, geminiDraftRecord{}, false, false, "unverified-document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owned, reuse, why := decideGeminiDraft(tc.state, tc.identity, tc.target, tc.thread, tc.prompt, tc.rec)
			if owned != tc.wantOwned || reuse != tc.wantReuse || why != tc.wantReason {
				t.Fatalf("decideGeminiDraft = (%v,%v,%q), want (%v,%v,%q)", owned, reuse, why, tc.wantOwned, tc.wantReuse, tc.wantReason)
			}
		})
	}
}

// A truncated digest must never authorize an overwrite.
func TestDraftDigestsAreNotTruncated(t *testing.T) {
	doc := "some document text"
	if got := draftHash(doc); len(got) != 64 {
		t.Fatalf("ownership digest is %d hex chars, want 64", len(got))
	}
	if got := draftHashShort(doc); len(got) != 8 {
		t.Fatalf("log digest is %d hex chars, want 8", len(got))
	}
	if draftHashShort(doc) == draftHash(doc) {
		t.Fatal("short and ownership digests must not be interchangeable")
	}
}

func TestOperationContext(t *testing.T) {
	for _, cause := range []string{"caller", "tab", "budget", "already-canceled"} {
		t.Run(cause, func(t *testing.T) {
			tab, closeTab := context.WithCancel(context.Background())
			defer closeTab()
			caller, closeCaller := context.WithCancel(context.Background())
			defer closeCaller()
			budget := time.Minute
			if cause == "budget" {
				budget = time.Millisecond
			}
			if cause == "already-canceled" {
				closeCaller()
			}
			ctx, cancel := operationContext(tab, caller, budget)
			defer cancel()
			if cause == "caller" {
				closeCaller()
			}
			if cause == "tab" {
				closeTab()
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("operation ignored cancellation")
			}
			if cause != "tab" && tab.Err() != nil {
				t.Fatal("operation poisoned long-lived tab")
			}
		})
	}
}

func TestOperationCleanupPreservesRoots(t *testing.T) {
	tab := context.Background()
	caller := context.Background()
	ctx, cancel := operationContext(tab, caller, time.Minute)
	cancel()
	if ctx.Err() == nil || tab.Err() != nil || caller.Err() != nil {
		t.Fatal("incorrect context ownership")
	}
}

func TestFillComposerEncodesEntireLargePrompt(t *testing.T) {
	prompt := strings.Repeat("public catalog \"quoted\" 日本語\n", 5000) + "\n"
	script := fillComposerJS(`div.ql-editor`, prompt)
	if !strings.Contains(script, "var expected="+jsQuote(prompt)+"+'\\n'") {
		t.Fatal("prompt was truncated, improperly quoted, or lost its structural newline")
	}
	for _, required := range []string{"q.setText(expected,'user')", "q.getText()===expected"} {
		if !strings.Contains(script, required) {
			t.Fatalf("missing step %q", required)
		}
	}
	// Fire and forget: the script must not revalidate an earlier document, refuse
	// a foreign draft, or verify the write afterwards.
	for _, forbidden := range []string{"prev", "foreign-draft", "identity-mismatch", "changed", "__geminiProxyDraft",
		".click(", "textContent=", "innerHTML=", "setTimeout", "clipboard"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("fire-and-forget fill still contains %q: %s", forbidden, script)
		}
	}
}
