package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Attachment is a file (image or video) uploaded into the ChatGPT composer.
type Attachment struct {
	Name string
	MIME string
	Data []byte
}

// TurnOptions configures one ChatGPT turn.
type TurnOptions struct {
	Prompt      string
	URL         string
	Attachments []Attachment
	Thinking    bool
	// ProjectURL starts a new chat inside an already-known ChatGPT project
	// (a /g/g-p-.../project URL) instead of the shared bridge project. Only
	// used when URL is empty. Empty preserves legacy behavior.
	ProjectURL string
	// ProjectName mints (or reuses) the named ChatGPT project for a new chat
	// when URL and ProjectURL are both empty. Empty preserves legacy behavior.
	ProjectName string
}

// TurnResult is the assistant output plus captured reasoning and thread URL.
type TurnResult struct {
	Answer    string
	Reasoning string
	URL       string
	// Images holds model SVGs rasterized bridge-side to PNG data URLs. Empty
	// on text-only answers and on any rasterization failure (the "[SVG: ...]"
	// text fallback in Answer still stands).
	Images []RasterImage
	// ProjectURL echoes the project a new chat was started in (empty when an
	// existing thread was continued or the shared project was used).
	ProjectURL string
	// BubbleID is the DOM id of the user bubble this submitted turn became,
	// captured at send observation. Empty when the turn never reached the
	// thread or capture failed. Recovery journals it as PendingBubbleID so
	// read-back verifies against the recorded bubble instead of assuming
	// pending == last bubble.
	BubbleID string
}

func mimeExt(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/png"):
		return ".png"
	case strings.HasPrefix(mime, "image/jpeg"), strings.HasPrefix(mime, "image/jpg"):
		return ".jpg"
	case strings.HasPrefix(mime, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mime, "image/gif"):
		return ".gif"
	case strings.HasPrefix(mime, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(mime, "video/webm"):
		return ".webm"
	case strings.HasPrefix(mime, "video/quicktime"):
		return ".mov"
	}
	return ".bin"
}

// attachFiles writes the payloads to temp files and sets them on the composer's
// file input. It returns the temp paths for the caller to remove once the turn
// is submitted (the browser reads them during/just after the change event).
func (c *Client) attachFiles(ctx context.Context, tab context.Context, atts []Attachment) ([]string, error) {
	var paths []string
	for _, a := range atts {
		f, err := os.CreateTemp("", "chatgpt-attach-*"+mimeExt(a.MIME))
		if err != nil {
			return paths, err
		}
		if _, err := f.Write(a.Data); err != nil {
			f.Close()
			return paths, err
		}
		f.Close()
		paths = append(paths, f.Name())
	}
	sel := `input[type=file][accept="image/*,video/*"]`
	upCtx, cancel := context.WithTimeout(tab, 150*time.Second)
	defer cancel()
	if err := chromedp.Run(upCtx, chromedp.SetUploadFiles(sel, paths, chromedp.ByQuery)); err != nil {
		return paths, fmt.Errorf("upload attachments: %w", err)
	}
	// Wait until the composer shows an attachment chip/preview and Send is
	// usable, stable for a few probes so an in-progress upload cannot be sent.
	deadline := time.Now().Add(150 * time.Second)
	stable := 0
	for {
		var state struct {
			Chip bool `json:"chip"`
			Send bool `json:"send"`
		}
		probe, probeCancel := context.WithTimeout(tab, 3*time.Second)
		err := chromedp.Run(probe, chromedp.Evaluate(`(() => {
		  const chip = document.querySelector('button[aria-label*="Remove"]')
		    || document.querySelector('img[alt][src^="blob:"]')
		    || document.querySelector('[data-testid*="attachment"]')
		    || document.querySelector('[aria-label*="attachment"]');
		  const send = document.querySelector('button[aria-label="Send"]')
		    || document.querySelector('[data-testid="send-button"]');
		  return { chip: !!chip, send: !!(send && !send.disabled) };
		})()`, &state))
		probeCancel()
		if err == nil && state.Chip && state.Send {
			stable++
			if stable >= 3 {
				return paths, nil
			}
		} else {
			stable = 0
		}
		if time.Now().After(deadline) {
			return paths, fmt.Errorf("attachment did not register in composer (chip=%v send=%v); REPAIR: inspect file input + attachment chip DOM", state.Chip, state.Send)
		}
		if err := pause(tab, 700*time.Millisecond); err != nil {
			return paths, err
		}
	}
}

// selectThinking switches the model power control to Thinking so the answer
// includes a reasoning trace. The current UI exposes a role=slider "power"
// control (Instant/Thinking/Pro) adjusted with the arrow keys, rather than a
// plain "Thinking" menu item.
func (c *Client) selectThinking(ctx context.Context, tab context.Context) error {
	openCtx, cancel := context.WithTimeout(tab, 10*time.Second)
	var opened bool
	err := chromedp.Run(openCtx, chromedp.Evaluate(`(() => {
	  const b = document.querySelector('button[aria-label="Select ChatGPT model"]')
	    || [...document.querySelectorAll('button')].find(e => /select.*model/i.test(e.getAttribute('aria-label') || ''));
	  if (!b) return false; b.click(); return true;
	})()`, &opened))
	cancel()
	if err != nil {
		return err
	}
	if !opened {
		return errors.New("model selector button not found; REPAIR: inspect the model dropdown DOM")
	}
	if err := pause(tab, 800*time.Millisecond); err != nil {
		return err
	}
	// The current UI exposes a "Thinking effort" power slider
	// ([role=slider] with aria-valuenow/aria-valuemax; levels read like
	// "Instant, 1 of 3" ... "High, 3 of 3") rather than a named Thinking item.
	// Raise it to the maximum effort, which is the reasoning level.
	level := func() (int, int) {
		var raw string
		probe, pc := context.WithTimeout(tab, 3*time.Second)
		_ = chromedp.Run(probe, chromedp.Evaluate(`(() => {
		  const s = document.querySelector('[role="slider"][aria-valuenow]');
		  if (!s) return '';
		  return s.getAttribute('aria-valuenow') + '/' + s.getAttribute('aria-valuemax');
		})()`, &raw))
		pc()
		var now, max int
		fmt.Sscanf(raw, "%d/%d", &now, &max)
		return now, max
	}
	now, max := level()
	for i := 0; i < 4 && max > 0 && now < max; i++ {
		adjust, ac := context.WithTimeout(tab, 5*time.Second)
		err := chromedp.Run(adjust,
			chromedp.Focus(`[role="slider"]`, chromedp.ByQuery),
			chromedp.KeyEvent(kb.ArrowRight))
		ac()
		if err != nil {
			return fmt.Errorf("adjust power slider: %w (REPAIR: inspect the model dropdown power control)", err)
		}
		if err := pause(tab, 600*time.Millisecond); err != nil {
			return err
		}
		now, max = level()
	}
	if max == 0 || now < max {
		return fmt.Errorf("could not raise thinking effort to max (now %d of %d); REPAIR: inspect the model dropdown power control", now, max)
	}
	closeCtx, cc := context.WithTimeout(tab, 5*time.Second)
	_ = chromedp.Run(closeCtx, chromedp.KeyEvent(kb.Escape))
	cc()
	return pause(tab, 300*time.Millisecond)
}
