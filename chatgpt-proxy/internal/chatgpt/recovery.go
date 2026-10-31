package chatgpt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yuin/goldmark"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// autolinkSpaceRe canonicalizes only the observed optional URL-defusing space
// after '<', '(' or a double quote. The quote case is observed in JSON schema
// URLs. The submit-path identity check uses the same rule.
var autolinkSpaceRe = regexp.MustCompile(`([<("]) ?(https?://)`)

// Match the declared CommonMark/HTML newline serialization, not visual layout.
// Interior spaces, tabs and literal plain-text backslashes remain significant.
func normalizeRecoveryPrompt(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"))
	return autolinkSpaceRe.ReplaceAllString(s, "$1$2")
}

// bubbleOnlyDivergence reports a snapshot whose only user text came from the
// rendered bubble (no DOM message ID) and still differs from the expected
// prompt. Visible text of a long message can be collapsed or partially
// hydrated, so it cannot prove a mismatch: the caller retries until the
// ID-bound source is readable. An exact bubble match still passes.
func bubbleOnlyDivergence(srcPath, userText string, expected []string) bool {
	if srcPath != "bubble" || len(expected) != 1 {
		return false
	}
	return userText != normalizeRecoveryPrompt(expected[0])
}

// ErrCompletedEmpty is authoritative rejection, not a missing DOM extraction.
// It is returned only after full pending-prompt and final-source binding.
var ErrCompletedEmpty = errors.New("ChatGPT stored a completed empty final response; no new tool calls were delivered; continue with a distinct new ordinary user message")

// firstDiffByte returns the first byte offset at which a and b differ,
// or min(len) when one is a prefix of the other. Evidence-only helper for
// the adoption user-bubble comparison above.
func firstDiffByte(a, b string) int {
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

// diffSpans maps ALL divergent regions between rendered and wanted text as
// "aOff+aLen/bOff+bLen,..." (offsets and lengths only, never content), so one
// log line distinguishes a single bounded renderer transform (one span, then
// re-convergence) from structural desync such as compaction or a rebuilt
// request (scattered spans or no re-convergence). Bounded: at most 6 spans,
// anchor search capped, so a 170KB pair stays cheap.
func diffSpans(a, b string) string {
	const anchorLen = 32
	const maxSpans = 6
	const scanWindow = 8192
	var spans []string
	i, j := 0, 0
	for len(spans) < maxSpans {
		// Skip equal prefix.
		for i < len(a) && j < len(b) && a[i] == b[j] {
			i++
			j++
		}
		if i >= len(a) && j >= len(b) {
			break
		}
		// Divergent region: re-sync on the next shared 32-byte anchor.
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
			spans = append(spans, itoa2(ai)+"+rest/"+itoa2(bj)+"+rest")
			break
		}
		spans = append(spans, itoa2(i)+"-"+itoa2(ai)+"/"+itoa2(j)+"-"+itoa2(bj))
		i, j = ai, bj
	}
	return strings.Join(spans, ",")
}

func itoa2(n int) string {
	if n < 0 {
		return "0"
	}
	return strconv.Itoa(n)
}

// UserBubbleMismatchError is the structural adoption failure: the thread's
// stored user turn provably is not the pending prompt. Spans carries the
// size-only divergence map (see diffSpans), so the recovery layer can tell a
// bounded renderer transform (one small span, then re-convergence) from
// multi-KB interior desync such as compaction or a rebuilt request. The
// Error text preserves the historic message so string-matching callers keep
// working; use errors.As for the structured evidence.
type UserBubbleMismatchError struct {
	Path     string // "bubble" or "mapping": which DOM-bound source diverged
	Rendered int
	Want     int
	DiffOff  int
	Spans    string
}

func (e *UserBubbleMismatchError) Error() string {
	return fmt.Sprintf("stored user prompt does not exactly match original request (rendered %dB want %dB)", e.Rendered, e.Want)
}

// PromptHash identifies a submitted prompt exactly the way adoption compares
// it: normalized, then SHA256 hex. The sent-log and the hash-lineage gate
// both use it, so a thread reading that went through the same normalization
// compares equal to the logged submission.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryPrompt(prompt)))
	return fmt.Sprintf("%x", sum)
}

// threadSnapshot is one read-only look at a stored thread's latest turn: the
// last rendered user bubble's normalized text plus everything needed to gate
// its following assistant answer — without comparing to any pending prompt.
// ReadThreadTurn (exact adoption) and ReadLatestThreadTurn (hash-lineage
// auto-resync) share it, so their finishedness proofs cannot drift apart.
type threadSnapshot struct {
	userText    string
	userRaw     string
	userID      string
	srcPath     string
	answerNext  bool
	answers     int
	visible     responseState
	images      []RasterImage
	probeErr    error
	emptyProved bool
	emptyErr    error
	// boundRequested marks verification against a journaled submit bubble
	// (not last-bubble correlation). boundIsLast reports that bubble is
	// still the thread's last user bubble: the thread is unchanged since
	// submit, so a text difference proves the send never landed.
	boundRequested bool
	boundIsLast    bool
}

// matchUserSource decides an exact adoption against the whole normalized
// prompt. Either the backend's raw stored serialization or its decoded
// rendering must equal the prompt byte for byte. Empty raw (an unmapped
// bubble with no DOM message ID) falls back to the decoded text alone; empty
// decoded never matches. Returns the provenance ("raw" or "decoded") so the
// caller can report which ground truth bound the turn.
func matchUserSource(raw, decoded, want string) (bool, string) {
	if raw != "" && raw == want {
		return true, "raw"
	}
	if decoded != "" && decoded == want {
		return true, "decoded"
	}
	return false, ""
}

// ReadThreadTurn only reads an existing finished turn in an owned target. The
// last rendered user bubble (or its exact ID-bound original text node) must
// equal the full normalized pending prompt. Never walk the backend mapping to
// guess the latest branch; only decode declared serialization for that DOM ID.
// A completed assistant body must follow it. Anything else fails closed.
//
// bubbleID is the journaled submit linkage: the DOM id the pending turn
// became, captured at send observation. When present, the pending is verified
// against that recorded bubble instead of assuming pending == last bubble.
// When the thread is unchanged yet holds no such bubble, the precise
// ErrPendingNeverStored reports the send never landed; when the recorded
// bubble is gone, ErrBubbleGone reports the thread changed. Empty bubbleID
// keeps the legacy last-bubble correlation (legacy journals).
func (c *Client) ReadThreadTurn(ctx context.Context, url, prompt, bubbleID string) (TurnResult, error) {
	if prompt == "" {
		return TurnResult{}, errors.New("recovery requires a stored thread and exact original prompt")
	}
	snap, err := c.snapshotThread(ctx, url, bubbleID, prompt)
	if err != nil {
		return TurnResult{}, err
	}
	// The full DOM-bound source audit matched all 97,805 bytes after decoding
	// its declared Markdown and normalizing newlines. No extra slash decoding.
	want := normalizeRecoveryPrompt(prompt)
	// Either the backend's raw stored serialization or its decoded rendering
	// must equal the prompt byte for byte — never a window, a tolerance, or
	// whitespace collapsing. The decoded rendering provably drops one \n per
	// extra blank line, so the raw bytes are the binding ground truth.
	if ok, provenance := matchUserSource(snap.userRaw, snap.userText, want); !ok {
		// Bound verification against a journaled submit bubble: when the
		// thread is unchanged since submit (recorded bubble is still last)
		// yet its text is not the pending, the send never landed — report
		// that precisely instead of a generic mismatch. A moved-on thread
		// keeps the legacy mismatch (fail closed, no adoption either way).
		if snap.boundRequested && snap.boundIsLast {
			log.Printf("[chatgpt] adoption pending never stored rendered=%dB want=%dB", len(snap.userText), len(want))
			return TurnResult{}, ErrPendingNeverStored
		}
		off := firstDiffByte(snap.userText, want)
		spans := diffSpans(snap.userText, want)
		log.Printf("[chatgpt] adoption user-bubble mismatch path=%s rendered=%dB want=%dB diffoff=%d spans=%s", snap.srcPath, len(snap.userText), len(want), off, spans)
		return TurnResult{}, &UserBubbleMismatchError{Path: snap.srcPath, Rendered: len(snap.userText), Want: len(want), DiffOff: off, Spans: spans}
	} else if provenance == "raw" && snap.userText != want {
		// Sizes only: the decoded rendering lost bytes the stored
		// serialization kept. Same turn, proven by the raw bytes.
		log.Printf("[chatgpt] adoption bound via raw stored source rendered=%dB want=%dB", len(snap.userText), len(want))
	}
	if snap.probeErr != nil {
		return TurnResult{}, snap.probeErr
	}
	if snap.emptyErr != nil {
		return TurnResult{}, snap.emptyErr
	}
	if snap.emptyProved {
		return TurnResult{}, ErrCompletedEmpty
	}
	return gateThreadAnswer(url, snap)
}

// ReadLatestThreadTurn returns the stored thread's latest user turn text
// (normalized exactly as adoption normalizes it) plus its following
// assistant answer, under the same finishedness proof as adoption but with NO
// prompt comparison. It exists for the hash-lineage auto-resync: the caller
// proves ownership with PromptHash(userText) against the sent-log instead of
// an exact pending prompt. The proof is: one "ChatGPT said:" section follows
// the user bubble (author-ended), exactly one answer (unambiguous), and the
// probe reports a complete non-empty rendering with no stop/error/continue
// chrome. Reasoning recaps are split out of Last by the probe, so a
// reasoning-only render fails the non-empty gate (non-thinking). Every
// failure is a plain error that fails closed — never a mismatch or
// completed-empty adoption signal.
func (c *Client) ReadLatestThreadTurn(ctx context.Context, url string) (string, TurnResult, error) {
	snap, err := c.snapshotThread(ctx, url, "")
	if err != nil {
		return "", TurnResult{}, err
	}
	if snap.probeErr != nil {
		return "", TurnResult{}, snap.probeErr
	}
	if snap.emptyErr != nil {
		return "", TurnResult{}, snap.emptyErr
	}
	if snap.emptyProved || snap.visible.Last == "" {
		return "", TurnResult{}, errors.New("stored thread holds no complete assistant answer to transplant")
	}
	if !snap.answerNext {
		return "", TurnResult{}, errors.New("stored latest turn has no finished assistant answer following it")
	}
	if snap.answers != 1 {
		return "", TurnResult{}, errors.New("stored thread latest turn has no single following answer")
	}
	if snap.visible.StopBtn || snap.visible.ErrorText || snap.visible.ContinueBtn || !snap.visible.Complete {
		return "", TurnResult{}, errors.New("stored latest answer is not a finished rendering; inspect before recovery")
	}
	return snap.userText, TurnResult{Answer: snap.visible.Last, Reasoning: snap.visible.Reasoning, Images: snap.images, URL: url}, nil
}

// gateThreadAnswer applies the exact-adoption finishedness proof to an
// already prompt-matched snapshot.
func gateThreadAnswer(url string, snap threadSnapshot) (TurnResult, error) {
	if !snap.answerNext {
		return TurnResult{}, errors.New("current response is not finished and nonempty")
	}
	if snap.answers != 1 {
		return TurnResult{}, errors.New("ambiguous multiple assistant responses after stored user prompt")
	}
	visible := snap.visible
	if visible.Last == "" || visible.StopBtn || visible.ErrorText || visible.ContinueBtn || !visible.Complete {
		return TurnResult{}, errors.New("finished stored response is not identically rendered; inspect before recovery")
	}
	return TurnResult{Answer: visible.Last, Reasoning: visible.Reasoning, Images: snap.images, URL: url}, nil
}

// snapshotThread navigates a throwaway background target to url and captures
// the latest turn: normalized user-bubble text plus the answer probe state.
// Read-only: it clicks only "Show more" expanders inside the last user bubble
// and closes only its own target afterwards. Recovery must never reload the
// active adapter tab. Probe-phase failures are captured in the snapshot (not
// returned) so ReadThreadTurn still reports a prompt mismatch first,
// preserving adoption error order.
//
// boundID is the journaled submit linkage: when non-empty, the source is
// fetched for that recorded bubble instead of the correlated last bubble.
// The current-last correlation still runs for the answer finishedness probe
// and to detect a moved-on thread.
func (c *Client) snapshotThread(ctx context.Context, url, boundID string, expected ...string) (threadSnapshot, error) {
	var snap threadSnapshot
	if !ThreadPattern.MatchString(url) {
		return snap, errors.New("recovery requires a stored thread and exact original prompt")
	}
	ctx, cancelRead := context.WithTimeout(ctx, 90*time.Second)
	defer cancelRead()
	c.mu.Lock()
	defer c.mu.Unlock()
	base, err := c.tabCtx(ctx)
	if err != nil {
		return snap, fmt.Errorf("recovery base: %w", err)
	}
	// Recovery must never reload the active adapter tab. Own a background
	// target for navigation/readback and close only that target afterwards.
	var tid target.ID
	probe, cancelProbe := requestContext(ctx, base)
	defer cancelProbe()
	if err := chromedp.Run(probe, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		tid, err = target.CreateTarget("about:blank").WithBackground(true).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		return err
	})); err != nil {
		return snap, fmt.Errorf("recovery create target: %w", err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(base, 3*time.Second)
		defer done()
		_ = chromedp.Run(cleanup, chromedp.ActionFunc(func(ctx context.Context) error {
			return target.CloseTarget(tid).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		}))
	}()
	// Inherit the initialized browser executor, not the allocator. Initializing
	// a second websocket through a short-lived request context can cancel it.
	owned, closeOwned := chromedp.NewContext(base, chromedp.WithTargetID(tid))
	defer closeOwned()
	stopInit := context.AfterFunc(ctx, closeOwned)
	initErr := chromedp.Run(owned)
	stopInit()
	if initErr != nil {
		return snap, fmt.Errorf("recovery attach target: %w", initErr)
	}
	tab, cancel := requestContext(ctx, owned)
	defer cancel()
	nav, done := context.WithTimeout(tab, 45*time.Second)
	defer done()
	if err := chromedp.Run(nav, chromedp.Navigate(url)); err != nil {
		return snap, fmt.Errorf("recovery navigate: %w", err)
	}
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return snap, fmt.Errorf("recovery ready: %w", err)
	}
	// Expand every collapsed region of the last user bubble (a long prompt
	// renders several "Show more" sections, not one), then wait until its
	// rendered length stops growing. Lengths only, never content.
	var expanded bool
	if err := chromedp.Run(tab, chromedp.Evaluate(recoveryExpandJS, &expanded)); err != nil {
		return snap, fmt.Errorf("recovery expand: %w", err)
	}
	if expanded {
		deadline := time.Now().Add(10 * time.Second)
		lastLen := -1
		for {
			var l int
			lctx, lcancel := context.WithTimeout(tab, 5*time.Second)
			_ = chromedp.Run(lctx, chromedp.Evaluate(recoveryUserLenJS, &l))
			lcancel()
			if l == lastLen || time.Now().After(deadline) {
				break
			}
			lastLen = l
			if err := pause(tab, 500*time.Millisecond); err != nil {
				return snap, err
			}
		}
	}
	// Correlate in the rendered DOM: the last "You said:" bubble's bound source
	// and whether a "ChatGPT said:" section follows it, so a stale earlier
	// answer can never be adopted. The answer itself comes from responseProbe
	// — the same rendered representation as the normal path.
	var corr struct {
		User       string `json:"user"`
		UserID     string `json:"userID"`
		AnswerNext bool   `json:"answerNext"`
		Answers    int    `json:"answers"`
	}
	if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil {
		return snap, fmt.Errorf("recovery user boundary: %w", err)
	}
	snap.answerNext, snap.answers = corr.AnswerNext, corr.Answers
	// Journaled submit linkage overrides last-bubble correlation for the
	// source fetch: verify the pending against the bubble it became, not
	// whatever happens to be last. When the thread moved on (recorded
	// bubble is no longer last), keep exact legacy last-bubble semantics.
	fetchID := corr.UserID
	if boundID != "" {
		if corr.UserID == "" {
			// Nothing rendered to correlate: page not ready, not a
			// verdict. Same retryable shape as the bubble path below.
			return snap, errors.New("recovery user boundary not fully rendered yet; retry")
		}
		if corr.UserID == boundID {
			fetchID = boundID
		}
	}
	snap.userID = fetchID
	snap.boundRequested = boundID != ""
	snap.boundIsLast = boundID != "" && corr.UserID != "" && boundID == corr.UserID
	// The actual DOM-bound node, never a latest/branch mapping walk. The
	// composer stores plain paragraphs as escaped CommonMark. Decode that
	// declared format with a real parser, not guessed slash/prefix stripping.
	userText := normalizeRecoveryPrompt(corr.User)
	snap.srcPath = "bubble"
	if fetchID != "" {
		raw, markdown, err := fetchUserSource(tab, fetchID)
		if err != nil {
			var srcErr *UserSourceError
			if boundID != "" && errors.As(err, &srcErr) && srcErr.Reason == "message-missing" {
				return snap, ErrBubbleGone
			}
			return snap, fmt.Errorf("recovery user source: %w", err)
		}
		// The stored serialization is the binding ground truth: the Markdown
		// rendering below drops one \n per extra blank line, so the raw bytes
		// must be kept alongside the decoded text for exact comparison.
		snap.userRaw = normalizeRecoveryPrompt(raw)
		if !markdown {
			userText = snap.userRaw
		} else if decoded, err := decodeMarkdownUserSource(tab, raw, expected...); err != nil {
			return snap, fmt.Errorf("recovery user source: %w", err)
		} else {
			userText = normalizeRecoveryPrompt(decoded)
		}
		snap.srcPath = "mapping"
	}
	snap.userText = userText
	// A bubble without its DOM message ID is an unhydrated or collapsed render,
	// not the authoritative user source. Comparing it would report a structural
	// prompt mismatch for a page that simply has not finished rendering, and a
	// mismatch is terminal. Wait for the exact ID-bound source instead.
	if bubbleOnlyDivergence(snap.srcPath, userText, expected) {
		return snap, errors.New("recovery user boundary not fully rendered yet; retry")
	}
	visible, err := responseProbe(tab, nil)
	if err != nil {
		snap.probeErr = fmt.Errorf("recovery response probe: %w", err)
		return snap, nil
	}
	snap.visible = visible
	if visible.Last == "" && visible.Complete && corr.UserID != "" {
		var empty bool
		js := fmt.Sprintf("(async () => {const uid = %s, id = %s; const recheck = () => %s([]); %s})()", strconv.Quote(corr.UserID), strconv.Quote(visible.LastID), responseProbeJS, completedEmptyJS)
		if err := chromedp.Run(tab, chromedp.Evaluate(js, &empty, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			snap.emptyErr = fmt.Errorf("recovery empty source: %w", err)
			return snap, nil
		}
		snap.emptyProved = empty
	}
	// Recovered turns must carry the same image parts as a live turn: rasterize
	// the adopted answer's substantive SVGs from this owned background target.
	// Best-effort — a failure leaves the "[SVG: ...]" text fallback standing, so
	// recovery semantics never change.
	if visible.Last != "" {
		snap.images = c.rasterizeIDs(tab, visible.LastID)
	}
	return snap, nil
}

// Only the observed empty DIL root's exact final source may establish rejection.
// No latest-branch lookup, renderer execution, reasoning or prior call adoption.
const completedEmptyJS = `
 const valid = () => {
   const st = recheck();
   const h = [...document.querySelectorAll('h4')].filter(e => e.textContent.trim() === 'You said:').at(-1);
   const owner = h?.nextElementSibling?.closest('[data-chatgpt-search-message-ids], [data-message-id]') || h?.closest('[data-chatgpt-search-message-ids], [data-message-id]');
   const userID = (owner?.getAttribute('data-chatgpt-search-message-ids') || owner?.getAttribute('data-message-id') || '').trim().split(/\s+/).at(-1);
   const root = [...document.querySelectorAll('[data-dil-message-id]')].find(e => e.getAttribute('data-dil-message-id') === id);
   return userID === uid && st.lastID === id && st.fresh && st.complete && !st.lastAssistant && !st.stopBtn && !st.continueBtn && !st.errorText && root && root.childNodes.length === 0;
 };
 if (!/^[a-zA-Z0-9-]+$/.test(id) || !valid()) return false;
 const thread = location.pathname.split('/c/')[1]; if (!thread) return false;
 const auth = await (await fetch('/api/auth/session')).json(); if (!auth.accessToken) return false;
 const r = await fetch('/backend-api/conversation/' + encodeURIComponent(thread), {headers:{Authorization:'Bearer ' + auth.accessToken}});
 if (!r.ok) return false;
 const mapping = (await r.json()).mapping, node = mapping?.[id], m = node?.message;
 if (m?.id !== id || m.author?.role !== 'assistant' || m.channel !== 'final' || m.recipient !== 'all' ||
     m.status !== 'finished_successfully' || m.end_turn !== true || m.metadata?.is_complete !== true ||
     m.content?.content_type !== 'text' || m.content.parts?.length !== 1 || m.content.parts[0] !== '') return false;
 let parent = node.parent;
 for (let i = 0; parent && i < 32; i++) {
   const p = mapping[parent]; if (!p || p.message?.id !== parent) return false;
   if (p.message.author?.role === 'user') return parent === uid && valid();
   if (p.message.author?.role !== 'assistant' || p.message.content?.content_type !== 'reasoning_recap') return false;
   parent = p.parent;
 }
 return false;
`

const recoveryExpandJS = `(() => {
	const h = [...document.querySelectorAll('h4')].filter(e => (e.textContent || '').trim() === 'You said:').at(-1);
	const sib = h ? h.nextElementSibling : null;
	if (!sib) return false;
	let clicked = false;
	// A long prompt renders several collapsed regions; expand them all.
	// Only exact "Show more" labels inside this bubble — never send,
	// regenerate, edit, copy, or page-global buttons.
	for (let round = 0; round < 10; round++) {
		const bs = [...sib.querySelectorAll('button')].filter(b => (b.textContent || '').trim() === 'Show more');
		if (bs.length === 0) break;
		for (const b of bs) { try { b.click(); } catch (e) {} }
		clicked = true;
	}
	return clicked;
})()`

// recoveryUserLenJS reports the rendered length of the last user bubble.
const recoveryUserLenJS = `(() => {
	const h = [...document.querySelectorAll('h4')].filter(e => (e.textContent || '').trim() === 'You said:').at(-1);
	const sib = h ? h.nextElementSibling : null;
	return sib ? sib.innerText.length : -1;
})()`

const recoveryUserJS = `(() => {
	  const norm = s => (s || '').replace(/\r\n?/g, '\n').trim();
	  const h4s = [...document.querySelectorAll('h4')];
	  let ui = -1;
	  for (let i = 0; i < h4s.length; i++) {
	    if ((h4s[i].textContent || '').trim() === 'You said:') ui = i;
	  }
	  if (ui < 0) return {user: '', answerNext: false};
	  let el = h4s[ui].nextElementSibling, parts = [], next = false;
	  while (el && el.tagName !== 'H4') {
	    const clone = el.cloneNode(true);
	    for (const b of clone.querySelectorAll('button, [role="toolbar"], [aria-hidden="true"], [data-markdown-copy="exclude"]')) b.remove();
	    for (const b of clone.querySelectorAll('br')) b.replaceWith(document.createTextNode('\n'));
	    for (const b of clone.querySelectorAll('p')) b.append(document.createTextNode('\n'));
	    parts.push(clone.textContent || '');
	    el = el.nextElementSibling;
	  }
	  let answers = 0;
	  for (let j = ui + 1; j < h4s.length; j++) {
	    if ((h4s[j].textContent || '').trim() === 'ChatGPT said:') { next = true; answers++; }
	  }
	  const h = h4s[ui];
	  const owner = h.nextElementSibling?.closest('[data-message-id], [data-chatgpt-search-message-ids]') || h.closest('[data-message-id], [data-chatgpt-search-message-ids], [data-turn-key]');
	  const userID = (owner?.getAttribute('data-message-id') || owner?.getAttribute('data-chatgpt-search-message-ids') || owner?.getAttribute('data-turn-key') || '').trim().split(/\s+/).at(-1);
	  return {user: norm(parts.join('\n')), userID, answerNext: next, answers};
	})()`

var equalLabelURLLink = regexp.MustCompile(`\[(https?://[^\s\[\]()]+)\]\((https?://[^\s\[\]()]+)\)`)

// Return a source projection only if it binds the ENTIRE expected prompt.
// The source's declared Markdown serializer may wrap a bare URL with itself
// as label and target. Unequal labels/targets and other characters stay exact.
func projectSourceURLLinks(text, expected string) string {
	if normalizeRecoveryPrompt(text) == normalizeRecoveryPrompt(expected) {
		return text
	}
	projected := equalLabelURLLink.ReplaceAllStringFunc(text, func(link string) string {
		parts := equalLabelURLLink.FindStringSubmatch(link)
		if parts[1] == parts[2] {
			return parts[1]
		}
		return link
	})
	if normalizeRecoveryPrompt(projected) == normalizeRecoveryPrompt(expected) {
		return projected
	}
	return text
}

// UserSourceError classifies WHY the ID-bound stored user message could not
// be read, so a read-back refusal can name the failed lookup step instead of
// one opaque "unavailable". Reason is content-free (a step name only, never
// message content): no-auth, fetch-failed, message-missing, message-shape,
// or message-empty.
type UserSourceError struct{ Reason string }

func (e *UserSourceError) Error() string {
	return fmt.Sprintf("stored user message unreadable (source=%s)", e.Reason)
}

// ErrBubbleGone reports a journaled submit bubble that is no longer in the
// thread: the turn landed, then the thread changed (edit/branch/regenerate)
// before read-back. Fail closed; nothing is adopted. Deliberately absent
// from IsPreSubmit — the turn may have executed.
var ErrBubbleGone = errors.New("submitted turn is no longer stored in the thread (thread changed since submit); nothing was adopted")

// ErrPendingNeverStored reports a thread unchanged since submit that holds
// no bubble for the pending turn: the send never landed. Fail closed.
// Deliberately absent from IsPreSubmit — abandon, never blind-resend, is
// the honest next step (a resend of a landed-but-unseen turn would
// duplicate execution).
var ErrPendingNeverStored = errors.New("submitted turn never landed in the thread (no new message stored); nothing was adopted")

// lastUserBubbleID reads the DOM id of the thread's last rendered user
// bubble. Read-only, content-free (an id only, never text). Used at submit
// time to journal which bubble a pending turn became, so read-back verifies
// against that recorded bubble instead of assuming pending == last bubble.
// Returns "" when no user bubble is rendered (nothing stored yet).
func lastUserBubbleID(tab context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(tab, 10*time.Second)
	defer cancel()
	js := `(() => {
	  const h = [...document.querySelectorAll('h4')].filter(e => (e.textContent || '').trim() === 'You said:').at(-1);
	  if (!h) return '';
	  const owner = h.nextElementSibling?.closest('[data-message-id], [data-chatgpt-search-message-ids]') || h.closest('[data-message-id], [data-chatgpt-search-message-ids], [data-turn-key]');
	  return (owner?.getAttribute('data-message-id') || owner?.getAttribute('data-chatgpt-search-message-ids') || owner?.getAttribute('data-turn-key') || '').trim().split(/\s+/).at(-1) || '';
	})()`
	var id string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &id)); err != nil {
		return "", err
	}
	return id, nil
}

// fetchUserSource returns the backend mapping's declared serialization of one
// user turn: the stored text plus whether its declared render format is
// Markdown. No decoding, no comparison — these raw bytes are the exact-binding
// ground truth the lossy rendering below cannot supply.
func fetchUserSource(tab context.Context, id string) (raw string, markdown bool, err error) {
	var source struct {
		Text     string
		Markdown bool
		Fail     string
		Shape    string
	}
	js := fmt.Sprintf(`(async () => {
	  const id = %s;
	  if (!/^[a-zA-Z0-9-]+$/.test(id)) return {fail:'message-missing'};
	  const thread = location.pathname.split('/c/')[1];
	  if (!thread) return {fail:'message-missing'};
	  const auth = await (await fetch('/api/auth/session')).json();
	  if (!auth.accessToken) return {fail:'no-auth'};
	  const r = await fetch('/backend-api/conversation/' + encodeURIComponent(thread), {headers:{Authorization:'Bearer ' + auth.accessToken}});
	  if (!r.ok) return {fail:'fetch-failed'};
	  const m = (await r.json()).mapping?.[id]?.message;
	  if (!m || m.id !== id) return {fail:'message-missing'};
	  if (m.author?.role !== 'user') return {fail:'message-shape'};
	  // Turns carrying attachments store as multimodal_text with the prompt
	  // as exactly one string part beside non-text parts. The binding ground
	  // truth is that single text part: the whole prompt was submitted as
	  // one text, so byte equality against it is still exact. Zero or
	  // several string parts stays a refusal (ambiguous).
	  const parts = m.content?.parts;
	  if (!Array.isArray(parts)) return {fail:'message-shape'};
	  const texts = parts.filter(p => typeof p === 'string');
	  const nonText = parts.length - texts.length;
	  if (texts.length !== 1) return {fail:'message-shape'};
	  const ct = m.content?.content_type;
	  if (ct !== 'text' && !(ct === 'multimodal_text' && nonText >= 1)) return {fail:'message-shape'};
	  if (texts[0] === '') return {fail:'message-empty'};
	  return {Text:texts[0], Markdown:m.metadata?.serialization_metadata?.render_format === 'markdown', Shape:(ct || '?') + '/' + parts.length + 'p'};
	})()`, strconv.Quote(id))
	if err := chromedp.Run(tab, chromedp.Evaluate(js, &source, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		return "", false, err
	}
	if source.Fail != "" {
		return "", false, &UserSourceError{Reason: source.Fail}
	}
	if source.Shape != "" && source.Shape != "text/1p" {
		// Content-free shape note: a multimodal turn bound on its single
		// text part. Same turn, proven by the text bytes below.
		log.Printf("[chatgpt] recovery source shape=%s", source.Shape)
	}
	return source.Text, source.Markdown, nil
}

// decodeMarkdownUserSource renders stored Markdown through its declared
// CommonMark serialization and extracts the text. KNOWN LOSSY: a run of N>1
// blank lines renders as a single paragraph break and extracts as exactly two
// \n, so one \n per extra blank line is dropped. Never the binding basis on
// its own — the raw source is.
func decodeMarkdownUserSource(tab context.Context, raw string, expected ...string) (string, error) {
	var rendered bytes.Buffer
	if err := goldmark.New().Convert([]byte(raw), &rendered); err != nil {
		return "", err
	}
	if bytes.Contains(rendered.Bytes(), []byte("<img")) {
		return "", errors.New("serialized user source contains non-text image content")
	}
	var text string
	// Detached parse only: no HTML, scripts, links or resources enter the page.
	decode := fmt.Sprintf(`(() => {const d = new DOMParser().parseFromString(%s, 'text/html');for(const e of d.querySelectorAll('br'))e.replaceWith(d.createTextNode('\n'));for(const e of d.querySelectorAll('p'))e.append(d.createTextNode('\n'));return d.body.textContent || '';})()`, strconv.Quote(rendered.String()))
	err := chromedp.Run(tab, chromedp.Evaluate(decode, &text))
	if err == nil && len(expected) == 1 {
		text = projectSourceURLLinks(text, expected[0])
	}
	return text, err
}

func originalUserText(tab context.Context, id string, expected ...string) (string, error) {
	raw, markdown, err := fetchUserSource(tab, id)
	if err != nil {
		return "", err
	}
	if !markdown {
		return raw, nil
	}
	return decodeMarkdownUserSource(tab, raw, expected...)
}
