// Shared recovery primitives for the Gemini bridge.
//
// NormalizePrompt is the canonical normalization under which a submitted
// prompt is hashed for the sent-log AND under which the interrupt adoption
// comparison reads the thread-side user turn. Both sides must use it, so hash lineage compares equal. Gemini-appropriate rules only:
// newline canonicalization plus surrounding-whitespace trim. Renderer
// transforms beyond this (if the Gemini DOM shows any) belong to an
// explicitly-enumerated bounded class in the adoption layer — never to
// silent extensions here.
package gemini

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// NormalizePrompt canonicalizes a submitted prompt for hashing and for
// thread-side adoption comparison. Newlines become LF; surrounding
// whitespace is insignificant.
func NormalizePrompt(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"))
}

// PromptHash identifies a submitted prompt exactly the way adoption compares
// it: normalized, then SHA256 hex. The sent-log and any hash-lineage gate
// both use it, so a thread reading that went through the same normalization
// compares equal to the logged submission.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(NormalizePrompt(prompt)))
	return fmt.Sprintf("%x", sum)
}

// AnswerHash identifies a received answer for sent-log outcome records.
// Answers are hashed raw (byte-exact): the hash is evidence, never a
// comparison key, so no normalization applies.
func AnswerHash(answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return fmt.Sprintf("%x", sum)
}

// ---------------------------------------------------------------------------
// Interrupt adoption + auto-resync thread reading.
//
// The adapter owns ONE tab and never navigates it for recovery: after an
// uncertain submission that tab IS the thread. Both readers verify the tab
// is still on the stored thread URL (fail closed otherwise) and evaluate
// read-only DOM probes — no clicks, no typing, no navigation, no tab
// churn. Anything unverifiable fails closed: the caller keeps 409.
//
// Selector provenance (do not assume ChatGPT structure):
//   - answer side: VERIFIED 2026-10-14 (model-response, message-content[id],
//     .markdown aria-busy, .response-footer.complete, localized stop
//     buttons) — the same finishedness proof as the submit path.
//   - user side: the renderer expands submitted whitespace (observed
//     2026-10-29: 354B paste, 475B bubble), so identity is tiered:
//     exact bytes, then whitespace-key equality (same words, same order),
//     then the long-prompt edge window. A wrong or absent node still
//     mismatches under every tier (safe), never falsely adopts.
// ---------------------------------------------------------------------------

// TurnResult is one verifiably-completed thread answer.
type TurnResult struct {
	Answer    string
	Reasoning string // Gemini renders no separate reasoning recap; always "".
	URL       string
	// Images holds the retrievable output-image parts for the same answer
	// (inline data: payloads within caps). Empty for text-only turns; the
	// Answer text always carries the fixtures and notes regardless.
	Images []ResponseImage
}

// PromptKey is the prompt identity a textarea round trip preserves: words
// and their order, with whitespace runs treated as a single separator.
// Adoption's second tier: a rendered bubble that matches the pending prompt
// under this key differs by rendering whitespace only (observed 2026-10-29:
// a 354B paste rendered as a 475B bubble). Equality here proves the same
// submission; a reworded, reordered or different message still mismatches.
func PromptKey(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// promptWindow accepts a rendered bubble whose leading and trailing runes
// match the pending prompt while the length stays within an eighth.
// Interior re-rendering is tolerated; a different message is not. Ported
// from the ChatGPT bridge's samePromptWindow for long-prompt parity.
func promptWindow(have, want string) bool {
	h := []rune(PromptKey(have))
	w := []rune(PromptKey(want))
	const edge = 200
	if len(w) < 2*edge || len(h) < 2*edge {
		return false
	}
	if d := len(h) - len(w); d > len(w)/8 || d < -len(w)/8 {
		return false
	}
	return string(h[:edge]) == string(w[:edge]) && string(h[len(h)-edge:]) == string(w[len(w)-edge:])
}

// UserBubbleMismatchError is the structural adoption failure: the thread's
// latest user turn provably is not the pending prompt. Spans carries the
// size-only divergence map (see DiffSpans), so the recovery layer can tell a
// small bounded difference from multi-KB structural desync. Use errors.As
// for the structured evidence.
type UserBubbleMismatchError struct {
	Path     string // "user-query": which DOM-bound source diverged
	Rendered int
	Want     int
	DiffOff  int
	Spans    string
}

func (e *UserBubbleMismatchError) Error() string {
	return fmt.Sprintf("stored user prompt does not exactly match original request (rendered %dB want %dB)", e.Rendered, e.Want)
}

// FirstDiffByte returns the first byte offset at which a and b differ, or
// min(len) when one is a prefix of the other. Evidence only.
func FirstDiffByte(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// DiffSpans maps ALL divergent regions between rendered and wanted text as
// "aOff-aEnd/bOff-bEnd,..." (offsets and lengths only, never content), so one
// log line distinguishes a small bounded difference (one span, then
// re-convergence) from structural desync (scattered spans or no
// re-convergence). Bounded: at most 6 spans, anchor search capped, so a
// 170KB pair stays cheap.
func DiffSpans(a, b string) string {
	const anchorLen = 32
	const maxSpans = 6
	const scanWindow = 8192
	var spans []string
	i, j := 0, 0
	for len(spans) < maxSpans {
		for i < len(a) && j < len(b) && a[i] == b[j] {
			i++
			j++
		}
		if i >= len(a) && j >= len(b) {
			break
		}
		ai, bj := i, j
		found := false
		aiEnd := ai + scanWindow
		if aiEnd > len(a) {
			aiEnd = len(a)
		}
	outer:
		for p := ai; p+anchorLen <= aiEnd; p++ {
			needle := a[p : p+anchorLen]
			q := j
			for q+anchorLen <= len(b) && q < j+scanWindow {
				if b[q:q+anchorLen] == needle {
					ai, bj = p, q
					found = true
					break outer
				}
				q++
			}
		}
		if !found {
			spans = append(spans, itoa(ai)+"+rest/"+itoa(bj)+"+rest")
			break
		}
		spans = append(spans, itoa(i)+"-"+itoa(ai)+"/"+itoa(j)+"-"+itoa(bj))
		i, j = ai, bj
	}
	return strings.Join(spans, ",")
}

func itoa(n int) string {
	if n < 0 {
		return "0"
	}
	return strconv.Itoa(n)
}

// recoverySnapshotJS returns one read-only look at the owned tab's latest
// turn: the last user-query text plus the newest model answer state. Lengths
// and booleans only cross into Go via struct fields; user text crosses for
// the equality comparison (compared, never logged).
const recoverySnapshotJS = `(function(){
  var uq = document.querySelectorAll('user-query');
  var user = '', userFound = false;
  if (uq.length) {
    var n = uq[uq.length-1];
    var clone = n.cloneNode(true);
    clone.querySelectorAll('button,[role="toolbar"],[aria-hidden="true"]').forEach(function(e){e.remove();});
    user = ((clone.innerText||'').replace(/\r\n?/g,'\n')).trim();
    userFound = true;
  }
  var nodes = document.querySelectorAll('model-response');
  var answerHTML = '', complete = false;
  if (nodes.length) {
    var m = nodes[nodes.length-1];
    var body = m.querySelector('message-content .markdown');
    var footer = m.querySelector('.response-footer.complete');
    var busy = body && body.getAttribute('aria-busy')==='true';
    var stale = uq.length>0 && (m.compareDocumentPosition(uq[uq.length-1]) & 2);
    if (body && footer && !busy && !stale) {
      var clean = body.cloneNode(true);
      clean.querySelectorAll('.code-block-decoration,button,[role="toolbar"]').forEach(function(e){e.remove();});
      answerHTML = clean.outerHTML; complete = true;
    }
  }
  var generating = !!document.querySelector('button[aria-label="Dừng phản hồi"],button[aria-label="Dừng"],button[aria-label="Stop"]');
  return {user:user, userFound:userFound, answerHTML:answerHTML, complete:complete, generating:generating};
})()`

// threadSnapshot is one read-only look at the owned tab's latest turn.
type threadSnapshot struct {
	userText   string
	userFound  bool
	answerHTML string
	complete   bool
	generating bool
	tabURL     string
}

// snapshotThreadLocked captures the latest turn from the OWN tab after
// proving the tab is still on url. Read-only: one Evaluate, no clicks,
// no navigation. Callers must hold c.mu.
func (c *Client) snapshotThreadLocked(ctx context.Context, url string) (threadSnapshot, error) {
	var snap threadSnapshot
	if strings.TrimSpace(url) == "" {
		return snap, errors.New("recovery requires a stored thread and exact original prompt")
	}
	rctx, cancel := operationContext(c.tab, ctx, 15*time.Second)
	defer cancel()
	var tabURL string
	if err := chromedp.Run(rctx, chromedp.Location(&tabURL)); err != nil {
		return snap, err
	}
	snap.tabURL = tabURL
	if strings.TrimSuffix(tabURL, "/") != strings.TrimSuffix(url, "/") {
		return snap, errors.New("owned tab is not on the stored thread; inspect the owned Gemini chat manually")
	}
	var corr struct {
		User       string `json:"user"`
		UserFound  bool   `json:"userFound"`
		AnswerHTML string `json:"answerHTML"`
		Complete   bool   `json:"complete"`
		Generating bool   `json:"generating"`
	}
	if err := chromedp.Run(rctx, chromedp.Evaluate(recoverySnapshotJS, &corr)); err != nil {
		return snap, err
	}
	snap.userText = NormalizePrompt(corr.User)
	snap.userFound = corr.UserFound
	snap.answerHTML = corr.AnswerHTML
	snap.complete = corr.Complete
	snap.generating = corr.Generating
	return snap, nil
}

// ReadThreadTurn reads the owned thread's finished answer for the pending
// prompt. The latest user turn must identify as the normalized pending
// prompt under a tiered identity — exact bytes, then whitespace-key
// equality (rendering whitespace only), then the long-prompt edge window —
// and a verifiably complete assistant rendering must follow it. Anything
// else fails closed: typed mismatch for structural divergence (auto-resync
// may still recover), plain errors for unfinished/absent state.
func (c *Client) ReadThreadTurn(ctx context.Context, url, prompt string) (TurnResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return TurnResult{}, errors.New("recovery requires a stored thread and exact original prompt")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return TurnResult{}, err
	}
	snap, err := c.snapshotThreadLocked(ctx, url)
	if err != nil {
		return TurnResult{}, err
	}
	want := NormalizePrompt(prompt)
	identity := ""
	switch {
	case snap.userFound && snap.userText != "" && snap.userText == want:
		identity = "exact"
	case snap.userFound && snap.userText != "" && PromptKey(want) != "" && PromptKey(snap.userText) == PromptKey(want):
		identity = "key"
	case snap.userFound && snap.userText != "" && promptWindow(snap.userText, want):
		identity = "window"
	}
	if identity == "" {
		off := FirstDiffByte(snap.userText, want)
		spans := DiffSpans(snap.userText, want)
		log.Printf("[gemini] adoption user-query mismatch rendered=%dB want=%dB keyrendered=%dB keywant=%dB diffoff=%d spans=%s",
			len(snap.userText), len(want), len(PromptKey(snap.userText)), len(PromptKey(want)), off, spans)
		return TurnResult{}, &UserBubbleMismatchError{Path: "user-query", Rendered: len(snap.userText), Want: len(want), DiffOff: off, Spans: spans}
	}
	log.Printf("[gemini] adoption user-query identity=%s rendered=%dB want=%dB", identity, len(snap.userText), len(want))
	return gateThreadAnswer(url, snap)
}

// ReadLatestThreadTurn returns the owned thread's latest user turn text
// (normalized exactly as adoption normalizes it) plus its following
// assistant answer, under the same finishedness proof as adoption but with
// NO prompt comparison. It exists for hash-lineage auto-resync: the caller
// proves ownership with PromptHash(userText) against the sent-log. Every
// failure is a plain error that fails closed — never a mismatch.
func (c *Client) ReadLatestThreadTurn(ctx context.Context, url string) (string, TurnResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return "", TurnResult{}, err
	}
	snap, err := c.snapshotThreadLocked(ctx, url)
	if err != nil {
		return "", TurnResult{}, err
	}
	if !snap.userFound || snap.userText == "" {
		return "", TurnResult{}, errors.New("stored thread holds no readable latest user turn to transplant")
	}
	res, err := gateThreadAnswer(url, snap)
	if err != nil {
		return "", TurnResult{}, err
	}
	return snap.userText, res, nil
}

// gateThreadAnswer applies the finishedness proof to an already-gated
// snapshot: no generation in flight, a complete non-empty rendering.
func gateThreadAnswer(url string, snap threadSnapshot) (TurnResult, error) {
	if snap.generating || !snap.complete || strings.TrimSpace(snap.answerHTML) == "" {
		return TurnResult{}, errors.New("stored thread answer is not a finished rendering; inspect before recovery")
	}
	text, parts := ConvertResponseWithImages(snap.answerHTML)
	if strings.TrimSpace(text) == "" && len(parts) == 0 {
		return TurnResult{}, errors.New("stored thread holds no complete assistant answer to adopt")
	}
	if strings.TrimSpace(text) == "" {
		text = "[Image: model-generated visual content]"
	}
	return TurnResult{Answer: text, Images: parts, URL: url}, nil
}
