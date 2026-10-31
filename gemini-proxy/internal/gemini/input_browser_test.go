//go:build browser

package gemini

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Offline local fixture: no Gemini account, network, real tools or user profile.
// Run explicitly with go test -tags browser ./internal/gemini -run TestComposer.
func TestComposer(t *testing.T) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Flag("no-sandbox", true))
	if path := os.Getenv("GEMINI_TEST_CHROME"); path != "" {
		opts = append(opts, chromedp.ExecPath(path))
	}
	alloc, stop := chromedp.NewExecAllocator(context.Background(), opts...)
	defer stop()
	tab, closeTab := chromedp.NewContext(alloc)
	defer closeTab()
	if err := chromedp.Run(tab); err != nil {
		t.Fatal(err)
	}
	c := &Client{tab: tab}
	fixture := `document.body.innerHTML='<rich-textarea><div class="ql-editor" role="textbox" contenteditable="true"></div></rich-textarea><button class="send-button">Send</button>';
window.sends=0;window.documentText='\n';
document.querySelector('button').onclick=()=>{window.submittedText=window.documentText;window.sends++;window.documentText='\n';};
document.querySelector('rich-textarea').__quill={getText:()=>window.documentText,setText:(t,s)=>{if(s!=='user')throw Error('wrong source');window.documentText=t;}};`
	reset := func() {
		t.Helper()
		if err := chromedp.Run(tab, chromedp.Evaluate(fixture, nil)); err != nil {
			t.Fatal(err)
		}
	}
	read := func(expr string) int {
		t.Helper()
		var n int
		if err := chromedp.Run(tab, chromedp.Evaluate(expr, &n)); err != nil {
			t.Fatal(err)
		}
		return n
	}
	prompt := strings.Repeat("synthetic catalog line\n", 6000) + "\n"
	t.Run("large-exact-and-send", func(t *testing.T) {
		reset()
		if err := c.typeAndSubmitLocked(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
		if gotSends, gotLen := read("window.sends"), read("window.submittedText.length"); gotSends != 1 || gotLen != len(prompt)+1 {
			t.Fatalf("lost input or duplicate send: sends=%d submitted=%d want=%d", gotSends, gotLen, len(prompt)+1)
		}
	})
	t.Run("leftover-exact-draft-is-reused-and-sent-once", func(t *testing.T) {
		reset()
		// A previous attempt left exactly our text in the composer (lost CDP
		// reply). The retry must not append a second copy and must not double-send.
		var result string
		if err := chromedp.Run(tab, chromedp.Evaluate(fillComposerJS(".ql-editor", prompt), &result)); err != nil {
			t.Fatal(err)
		}
		if result != "inserted" {
			t.Fatalf("first fill = %q", result)
		}
		if err := c.typeAndSubmitLocked(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
		if read("window.sends") != 1 || read("window.submittedText.length") != len(prompt)+1 {
			t.Fatal("retry appended or double-submitted")
		}
	})
	t.Run("foreign-draft-is-overwritten-and-sent", func(t *testing.T) {
		reset()
		// Fire and forget: text the bridge did not write is replaced, not refused.
		if err := chromedp.Run(tab, chromedp.Evaluate("window.documentText="+jsQuote("text the user typed\n"), nil)); err != nil {
			t.Fatal(err)
		}
		if err := c.typeAndSubmitLocked(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
		if read("window.sends") != 1 || read("window.submittedText.length") != len(prompt)+1 {
			t.Fatal("foreign draft was not replaced and sent")
		}
	})
	t.Run("canceled-no-send-tab-survives", func(t *testing.T) {
		reset()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.typeAndSubmitLocked(ctx, prompt); !errors.Is(err, ErrNotSubmitted) {
			t.Fatalf("%v", err)
		}
		if read("window.sends") != 0 {
			t.Fatal("sent canceled turn")
		}
		if tab.Err() != nil {
			t.Fatal("poisoned tab")
		}
	})
	t.Run("input-timeout-no-send-late-draft-retry", func(t *testing.T) {
		reset()
		if err := chromedp.Run(tab, chromedp.Evaluate(`document.querySelector('rich-textarea').__quill.setText=(t)=>{let until=Date.now()+250;while(Date.now()<until){}window.documentText=t;}`, nil)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := c.typeAndSubmitLocked(ctx, prompt); !errors.Is(err, ErrNotSubmitted) {
			t.Fatalf("%v", err)
		}
		if read("window.sends") != 0 {
			t.Fatal("sent timed-out turn")
		}
		if err := c.typeAndSubmitLocked(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
		if read("window.sends") != 1 {
			t.Fatal("bad retry")
		}
	})
}
