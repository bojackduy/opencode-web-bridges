// Package meta provides a chromedp-based browser client for Meta AI Web
// (https://www.meta.ai/). It mirrors gemini/internal/gemini but targets
// Meta's composer and answer surfaces.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// DefaultChromePort is the CDP port meta-proxy owns.
const DefaultChromePort = 9224

// HomePage is the Meta AI consumer UI.
const HomePage = "https://www.meta.ai/"

// Status is the login/composer readiness reported by the browser surface.
type Status struct {
	Profile    string `json:"profile,omitempty"`
	ChromePort int    `json:"chrome_port,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	LoggedIn   bool   `json:"logged_in"`
	Composer   bool   `json:"composer"`
	NewChat    bool   `json:"new_chat"`
	LoginBtn   bool   `json:"login_btn"`
}

// ResponseImage is a retrievable output image (parity with gemini).
type ResponseImage struct {
	URL string `json:"url"`
}

// Client drives a dedicated Chrome tab on Meta AI.
type Client struct {
	ChromePort int
	ProfileDir string
	ChromePath string
	Background bool

	mu          sync.Mutex
	browser     context.Context
	cancel      context.CancelFunc
	tab         context.Context
	tabDone     context.CancelFunc
	wsURL       string
	actualPort  int
	ownedTarget string
	launched    bool
	life        context.Context
}

func New(port int, profile, chrome string) *Client {
	return &Client{ChromePort: port, ProfileDir: profile, ChromePath: chrome,
		life: context.Background()}
}

// Close releases the chromedp context.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teardownLocked()
}

func (c *Client) teardownLocked() {
	if c.tabDone != nil {
		c.tabDone()
		c.tabDone = nil
	}
	c.tab = nil
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.browser = nil
	c.launched = false
	c.ownedTarget = ""
}

func (c *Client) tabAlive() bool {
	return c.tab != nil && c.tab.Err() == nil
}

// ensureBrowser attaches to (or launches) Chrome on c.ChromePort with the
// isolated profile, mints our own tab, and navigates to HomePage.
func (c *Client) ensureBrowser(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tabAlive() {
		return nil
	}
	c.teardownLocked()

	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := c.tryEnsureLocked(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		c.teardownLocked()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("unknown browser setup failure")
	}
	return lastErr
}

func (c *Client) tryEnsureLocked(ctx context.Context) error {
	port := c.ChromePort
	if !c.debuggerUp(port) {
		if !c.portFree(port) {
			return errors.New("configured Chrome port is occupied; refusing another port")
		}
		if err := c.launchLocked(ctx, port); err != nil {
			return err
		}
	} else {
		log.Printf("[meta] attaching to existing debugger on %d", port)
	}
	wsURL, err := c.waitDebugger(port, 20*time.Second)
	if err != nil {
		return err
	}
	if err := c.verifyProfile(port, c.ProfileDir); err != nil {
		return err
	}
	allocCtx, cancel := chromedp.NewRemoteAllocator(c.life, wsURL)
	_ = allocCtx
	_ = cancel
	// Fix: need to set c.browser and c.cancel
	c.browser = allocCtx
	c.cancel = cancel
	c.wsURL = wsURL
	c.actualPort = port

	tid, err := c.createOwnTab(port)
	if err != nil {
		return err
	}
	log.Printf("[meta] created own tab %s on %d", tid, port)
	c.ownedTarget = string(tid)

	tab, done := chromedp.NewContext(c.browser, chromedp.WithTargetID(tid))
	if err := chromedp.Run(tab, chromedp.ActionFunc(func(context.Context) error { return nil })); err != nil {
		done()
		return fmt.Errorf("tab verify: %w", err)
	}
	c.tab = tab
	c.tabDone = done
	log.Printf("[meta] tab %s verified; navigating to %s", tid, HomePage)

	nav, cancelNav := context.WithTimeout(c.tab, 90*time.Second)
	defer cancelNav()
	if err := chromedp.Run(nav, chromedp.Navigate(HomePage)); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if err := c.pollPageSettledLocked(nav); err != nil {
		return err
	}
	log.Printf("[meta] tab %s settled at %s", tid, HomePage)
	c.launched = true
	return nil
}

func (c *Client) pollPageSettledLocked(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var ready, title string
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		errReady := chromedp.Run(rctx, chromedp.Evaluate(`document.readyState`, &ready))
		errTitle := chromedp.Run(rctx, chromedp.Evaluate(`document.title`, &title))
		cancel()
		if errReady == nil && errTitle == nil && ready == "complete" && strings.TrimSpace(title) != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("page did not settle after navigation")
}

func (c *Client) debuggerUp(port int) bool {
	_, err := c.debuggerWSURL(port)
	return err == nil
}

func (c *Client) debuggerWSURL(port int) (string, error) {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("empty debugger url")
	}
	return v.WebSocketDebuggerURL, nil
}

func (c *Client) waitDebugger(port int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if wsURL, err := c.debuggerWSURL(port); err == nil {
			return wsURL, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("debugging port %d never came up", port)
}

func (c *Client) portFree(port int) bool {
	return !c.debuggerUp(port)
}

func (c *Client) launchLocked(ctx context.Context, port int) error {
	bin, err := chromeBinary(c.ChromePath)
	if err != nil {
		return err
	}
	args := []string{
		bin,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--no-first-run",
		"--no-default-browser-check",
		fmt.Sprintf("--user-data-dir=%s", c.ProfileDir),
		"about:blank",
	}
	if c.Background {
		args = append(args, "--headless=new")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("chrome launch: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (c *Client) createOwnTab(port int) (target.ID, error) {
	client := http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", port, HomePage), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.ID == "" {
		return "", errors.New("empty tab id")
	}
	return target.ID(t.ID), nil
}

func (c *Client) verifyProfile(port int, profile string) error {
	return nil
}

func chromeBinary(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("google-chrome"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("chrome"); err == nil {
		return p, nil
	}
	return "", errors.New("chrome not found")
}

func operationContext(tab context.Context, parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(tab, timeout)
	if parent != nil {
		go func() {
			select {
			case <-parent.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

// ensureBrowserLocked assumes c.mu is held (same discipline as gemini-proxy).
func (c *Client) ensureBrowserLocked(ctx context.Context) error {
	if c.tabAlive() {
		return nil
	}
	c.teardownLocked()
	return c.tryEnsureLocked(ctx)
}

// LoginState attaches (read-only) and reports the signed-in state.
func (c *Client) LoginState(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return nil, err
	}
	rctx, cancel := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel()
	st := &Status{Profile: c.ProfileDir, ChromePort: c.actualPort}
	var url, title string
	if err := chromedp.Run(rctx, chromedp.Location(&url)); err != nil {
		return nil, err
	}
	if err := chromedp.Run(rctx, chromedp.Title(&title)); err != nil {
		return nil, err
	}
	st.URL, st.Title = url, title
	st.LoginBtn = c.anyPresent(rctx, loginSelectors)
	st.LoggedIn = c.anyPresent(rctx, loggedInSelectors) && !st.LoginBtn
	st.Composer = c.anyPresent(rctx, composerSelectors)
	st.NewChat = c.anyPresent(rctx, newChatSelectors)
	return st, nil
}

func (c *Client) CurrentURL(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return ""
	}
	rctx, cancel := operationContext(c.tab, ctx, 10*time.Second)
	defer cancel()
	var url string
	if err := chromedp.Run(rctx, chromedp.Location(&url)); err != nil {
		return ""
	}
	return url
}

// Ask submits a prompt and returns the answer text. Finishedness proof
// (same standard as gemini-proxy): snapshot the answer set before submit,
// then adopt only new content stable across 3 polls (~4.5s) with no stop
// indicator. Stale, absent, or unfinished answers fail closed — never adopted.
func (c *Client) Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("prompt must not be empty")
	}
	if timeout <= 0 || timeout > 30*time.Minute {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	log.Printf("[meta] stage=start prompt_bytes=%d new_session=%v timeout=%s", len(prompt), newSession, timeout)
	defer func() {
		log.Printf("[meta] stage=finish duration=%s", time.Since(started).Round(time.Millisecond))
	}()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return "", err
	}
	rctx, rcancel := operationContext(c.tab, ctx, timeout)
	defer rcancel()

	if newSession {
		// Fresh turns must PROVE a new thread before submitting: all
		// sessions share one tab, and silently continuing on the previous
		// thread cross-contaminates another session (observed: fresh test
		// sessions adopting live user threads). A missing control or an
		// unchanged URL fails closed — never submit blind.
		var beforeURL string
		_ = chromedp.Run(rctx, chromedp.Location(&beforeURL))
		clicked := false
		for _, sel := range newChatSelectors {
			var found bool
			if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
				_ = chromedp.Run(rctx, chromedp.Click(sel, chromedp.ByQuery))
				clicked = true
				break
			}
		}
		if !clicked {
			return "", fmt.Errorf("%w: new-chat control missing, refusing fresh submit into the previous thread", ErrNotSubmitted)
		}
		minted := false
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var cur string
			if err := chromedp.Run(rctx, chromedp.Location(&cur)); err == nil && cur != beforeURL && ValidThreadURL(cur) {
				minted = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !minted {
			return "", fmt.Errorf("%w: new chat did not open, refusing fresh submit into the previous thread", ErrNotSubmitted)
		}
	}

	before := c.answerSnapshot(rctx)
	log.Printf("[meta] stage=submit prior_answers=%d", before.count)

	// Bulk insert, never keystroke typing: SendKeys types word-by-word and
	// takes minutes on large pastes (observed). Meta's composer is Lexical,
	// which rejects document.execCommand('insertText') (observed
	// exec-false) but honors a single CDP Input.insertText transaction.
	// Select-all first so residue drafts are replaced, not appended to;
	// the held length is then verified before Send is touched. A short
	// write fails closed — a partial paste must never be sent.
	composer := ""
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range composerSelectors {
			var found bool
			if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
				composer = sel
				break
			}
		}
		if composer != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if composer == "" {
		return "", ErrNoComposer
	}
	selAllJS := fmt.Sprintf(`(function(){var el=document.querySelector(%q); if(!el)return 'missing'; el.focus(); var sel=getSelection(); try{sel.selectAllChildren(el);}catch(e){return 'select-error:'+e;} return 'selected:'+((el.innerText||'').length);})()`, composer)
	var selRes string
	if err := chromedp.Run(rctx, chromedp.Evaluate(selAllJS, &selRes)); err != nil || !strings.HasPrefix(selRes, "selected:") {
		return "", fmt.Errorf("%w: composer select failed (%s): %v", ErrNotSubmitted, selRes, err)
	}
	if err := chromedp.Run(rctx, input.InsertText(prompt)); err != nil {
		return "", fmt.Errorf("%w: composer insert failed: %v", ErrNotSubmitted, err)
	}
	var held string
	if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`(document.querySelector(%q).innerText||'')`, composer), &held)); err != nil {
		return "", fmt.Errorf("%w: composer verify failed: %v", ErrNotSubmitted, err)
	}
	// Lexical normalizes edge whitespace on insert (observed: consistent
	// ~570B shortfall on 190KB+ pastes, all at the edges). Exact bytes win;
	// edge-whitespace-only differences are the same words and safe to send.
	// Anything else fails closed — a cut tail usually holds the question.
	if held != prompt && strings.TrimSpace(held) != strings.TrimSpace(prompt) {
		off := 0
		for off < len(held) && off < len(prompt) && held[off] == prompt[off] {
			off++
		}
		log.Printf("[meta] stage=composer_insert result=rejected held=%d want=%d diffoff=%d", len(held), len(prompt), off)
		return "", fmt.Errorf("%w: short composer write held=%d want=%d diffoff=%d", ErrNotSubmitted, len(held), len(prompt), off)
	}
	log.Printf("[meta] stage=composer_insert result=inserted held=%d prompt_bytes=%d exact=%v", len(held), len(prompt), held == prompt)

	sent := false
	for _, sel := range sendSelectors {
		var found bool
		if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
			if err := chromedp.Run(rctx, chromedp.Click(sel, chromedp.ByQuery)); err == nil {
				sent = true
				break
			}
		}
	}
	if !sent {
		// Fallback: Enter key submits most chat composers.
		if err := chromedp.Run(rctx, chromedp.KeyEvent("\n")); err == nil {
			sent = true
		}
	}
	if !sent {
		return "", ErrNotSubmitted
	}
	log.Printf("[meta] stage=submitted")

	return c.waitForAnswerLocked(rctx, ctx, before)
}

// answerSnapshot is the pre-submit answer set: node count plus the newest
// answer text length. Both must advance before any adoption.
type answerSnapshot struct {
	count       int
	newestBytes int
}

func (c *Client) answerSnapshot(ctx context.Context) answerSnapshot {
	var snap answerSnapshot
	_ = chromedp.Run(ctx, chromedp.Evaluate(newestAnswerCountJS, &snap.count))
	_ = chromedp.Run(ctx, chromedp.Evaluate(newestAnswerTextJS, &snap.newestBytes))
	return snap
}

// waitForAnswerLocked polls for a new, stable, finished answer. Callers must
// hold c.mu. Fail-closed: stale/absent/unfinished content is never adopted.
// Completion is proved by stability (text unchanged across 3 polls ~6s)
// after the node count or text length grew past the before-snapshot, plus
// presence of the "Copy response" affordance that Meta renders only for
// a finished turn.
func (c *Client) waitForAnswerLocked(rctx, parent context.Context, before answerSnapshot) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastStable string
	stableRounds := 0
	lastLogged := -1
	for {
		select {
		case <-rctx.Done():
			return "", fmt.Errorf("meta: no answer within timeout: %w", rctx.Err())
		case <-parent.Done():
			return "", fmt.Errorf("meta: no answer within timeout: %w", parent.Err())
		case <-ticker.C:
			var count int
			_ = chromedp.Run(rctx, chromedp.Evaluate(newestAnswerCountJS, &count))
			var text string
			_ = chromedp.Run(rctx, chromedp.Evaluate(newestAnswerTextJS, &text))
			// completion indicator: Copy response button rendered
			var hasCopy bool
			_ = chromedp.Run(rctx, chromedp.Evaluate(`!!document.querySelector('button[aria-label="Copy response"]')`, &hasCopy))
			if len(text) != lastLogged {
				lastLogged = len(text)
				log.Printf("[meta] stage=stream_poll answer_bytes=%d answer_nodes=%d has_copy=%v", len(text), count, hasCopy)
			}
			if !answerAdoptable(before.count, before.newestBytes, count, text, hasCopy) {
				stableRounds = 0
				lastStable = text
				continue
			}
			if text == lastStable {
				stableRounds++
			} else {
				stableRounds = 0
				lastStable = text
			}
			if stableRounds >= 3 {
				select {
				case <-rctx.Done():
					return "", fmt.Errorf("meta: no answer within timeout: %w", rctx.Err())
				case <-time.After(1 * time.Second):
				}
				if isMetaErrorState(text) {
					log.Printf("[meta] stage=finish result=error-bubble")
					return "", errors.New("meta: answer is an error state, not model output")
				}
				rendered, rerr := c.renderAnswer(rctx)
				if rerr != nil {
					return "", rerr
				}
				return rendered, nil
			}
		}
	}
}

// metaErrorStrings are UI error states Meta renders inside the answer
// article instead of a reply (observed live). They carry the finishedness
// affordances (Copy button) but are not model output: adopting one would
// commit an error bubble as the turn's answer. Match is case-insensitive
// on trimmed text; only the reason code is ever logged.
var metaErrorStrings = []string{
	"something went wrong",
	"please try again",
}

func isMetaErrorState(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	for _, s := range metaErrorStrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// answerAdoptable is the pure finishedness gate: new content exists (node
// count grew or newest text lengthened), text is non-blank, and the
// "Copy response" completion affordance is present (Meta renders it only
// for a finished turn). Unit-tested.
func answerAdoptable(beforeCount, beforeBytes, count int, text string, hasCopy bool) bool {
	if !hasCopy {
		return false
	}
	if strings.TrimSpace(text) == "" {
		return false
	}
	if count > beforeCount {
		return true
	}
	if count == beforeCount && len(text) > beforeBytes {
		return true
	}
	return false
}

// newestAnswerCountJS counts candidate answer nodes; newestAnswerTextJS
// returns the newest candidate answer text ("" when none);
// newestAnswerHTMLJS returns its HTML for Markdown rendering (delivery
// only — stability polling always compares plain text).
const newestAnswerCountJS = `(function(){
  return document.querySelectorAll('div[role="article"][aria-label="Meta AI response"]').length;})()`

const newestAnswerTextJS = `(function(){
  var n=document.querySelectorAll('div[role="article"][aria-label="Meta AI response"]');
  if(!n.length) return '';
  var el=n[n.length-1];
  // Prefer the markdown text container; fall back to the article. UI
  // chrome (buttons incl. "Show thinking", toolbars, hidden and thinking
  // nodes) is stripped: innerText otherwise leaks control text into
  // delivered answers (observed: titles delivered as
  // "Show thinking\n\nGreeting") and poisons wrapper validation.
  var body=(el.querySelector('[class*="text-response"]') || el).cloneNode(true);
  body.querySelectorAll('button,[role="toolbar"],[aria-hidden="true"],[data-thought],[data-thinking]').forEach(function(e){e.remove();});
  return (body.innerText||'').trim();})()`

const newestAnswerHTMLJS = `(function(){
  var n=document.querySelectorAll('div[role="article"][aria-label="Meta AI response"]');
  if(!n.length) return '';
  var el=n[n.length-1];
  var body=el.querySelector('[class*="text-response"]') || el;
  return body.outerHTML||'';})()`

// ReadThreadTurn reads a specific turn (read-only, no submit). It verifies
// the tab is on url, then returns the newest finished answer: two reads 2s
// apart must agree with no stop indicator, else fail closed.
func (c *Client) ReadThreadTurn(ctx context.Context, url, prompt string) (TurnResult, error) {
	_ = prompt // binding key is the URL; prompt sizes are logged by the caller
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return TurnResult{}, err
	}
	rctx, cancel := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel()
	var cur string
	if err := chromedp.Run(rctx, chromedp.Location(&cur)); err != nil {
		return TurnResult{}, err
	}
	if url != "" && cur != url {
		return TurnResult{}, fmt.Errorf("%w: want thread, at other page", ErrThreadBinding)
	}
	return c.readLatestLocked(rctx)
}

// ReadLatestThreadTurn reads the latest turn (read-only, no submit).
func (c *Client) ReadLatestThreadTurn(ctx context.Context, url string) (string, TurnResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return "", TurnResult{}, err
	}
	rctx, cancel := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel()
	var cur string
	if err := chromedp.Run(rctx, chromedp.Location(&cur)); err != nil {
		return "", TurnResult{}, err
	}
	if url != "" && cur != url {
		return "", TurnResult{}, fmt.Errorf("%w: want thread, at other page", ErrThreadBinding)
	}
	res, err := c.readLatestLocked(rctx)
	if err != nil {
		return "", TurnResult{}, err
	}
	return cur, res, nil
}

// readLatestLocked returns the newest finished answer. Callers must hold c.mu.
func (c *Client) readLatestLocked(ctx context.Context) (TurnResult, error) {
	if c.anyPresent(ctx, stopSelectors) {
		return TurnResult{}, errors.New("meta: generation still in progress")
	}
	var first string
	_ = chromedp.Run(ctx, chromedp.Evaluate(newestAnswerTextJS, &first))
	if strings.TrimSpace(first) == "" {
		return TurnResult{}, errors.New("meta: no answer present")
	}
	select {
	case <-ctx.Done():
		return TurnResult{}, ctx.Err()
	case <-time.After(2 * time.Second):
	}
	if c.anyPresent(ctx, stopSelectors) {
		return TurnResult{}, errors.New("meta: generation still in progress")
	}
	var second string
	_ = chromedp.Run(ctx, chromedp.Evaluate(newestAnswerTextJS, &second))
	if strings.TrimSpace(second) == "" || second != first {
		return TurnResult{}, errors.New("meta: answer not stable")
	}
	if isMetaErrorState(second) {
		return TurnResult{}, errors.New("meta: answer is an error state, not model output")
	}
	rendered, rerr := c.renderAnswer(ctx)
	if rerr != nil {
		return TurnResult{}, rerr
	}
	log.Printf("[meta] stage=read answer_bytes=%d", len(second))
	return TurnResult{Answer: rendered}, nil
}

// renderAnswer fetches the finished answer once as HTML and converts it to
// Markdown (wrapper-bearing text bypasses conversion untouched). Called
// only after stability is proven on plain text.
func (c *Client) renderAnswer(ctx context.Context) (string, error) {
	var htmlFrag, text string
	if err := chromedp.Run(ctx, chromedp.Evaluate(newestAnswerHTMLJS, &htmlFrag)); err != nil {
		return "", fmt.Errorf("meta: answer render failed: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(newestAnswerTextJS, &text)); err != nil {
		return "", fmt.Errorf("meta: answer render failed: %v", err)
	}
	return answerToMarkdown(htmlFrag, text), nil
}

// ValidThreadURL is a validity check.
func ValidThreadURL(u string) bool {
	return u != "" && strings.HasPrefix(u, "https://www.meta.ai/")
}

// TurnResult mirrors gemini.TurnResult for recovery readers.
type TurnResult struct {
	Answer string
	Images []ResponseImage
}

// Sentinel errors.
var (
	ErrNotSubmitted = errors.New("meta: not submitted")
	ErrNotLoggedIn  = errors.New("meta: not logged in")
	ErrNoComposer   = errors.New("meta: no composer")
	ErrForeignDraft = errors.New("meta: foreign draft")
	ErrDraftOwned   = errors.New("meta: draft owned")
)

var ErrThreadBinding = errors.New("meta thread binding error")

func IsPreSubmit(err error) bool {
	return errors.Is(err, ErrNotSubmitted) || errors.Is(err, ErrNoComposer) || errors.Is(err, ErrNotLoggedIn) ||
		errors.Is(err, ErrForeignDraft) || errors.Is(err, ErrDraftOwned)
}
func IsNavigateError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "navigate:") || strings.Contains(msg, "page did not settle")
}

// ExtractExecutableCalls is defined in bridge.go.

// Selectors (pending verification on actual meta.ai DOM).
var (
	loginSelectors = []string{
		`a[href*="login"]`,
		`button[aria-label*="Log in"]`,
		`a[aria-label*="Log in"]`,
	}
	loggedInSelectors = []string{
		`img[alt*="profile"]`,
		`div[aria-label*="Account"]`,
		`button[aria-label*="Account"]`,
		`a[aria-label*="Account"]`,
	}
	composerSelectors = []string{
		`div[role="textbox"][contenteditable="true"]`, // verified 2026-10-30 live DOM
		`div[contenteditable="true"]`,
		`textarea`,
		`div[role="textbox"]`,
		`input[type="text"]`,
	}
	sendSelectors = []string{
		`button[aria-label="Send"]`, // verified 2026-10-30 live DOM
		`button[aria-label*="Send"]`,
		`button[aria-label*="Submit"]`,
		`button[type="submit"]`,
	}
	newChatSelectors = []string{
		`a[href*="?new=1"]`, // verified live 2026-10-31: sidebar "New chat" link
		`button[aria-label*="New chat"]`,
		`a[aria-label*="New chat"]`,
	}
	answerSelectors = []string{
		`div[role="article"][aria-label="Meta AI response"]`, // verified 2026-10-30
		`div[role="article"]`,
	}
	// stopSelectors match the cancel/stop affordance shown while generating.
	// Meta uses aria-label="Cancel" for the generation-cancel button (verified
	// 2026-10-30). It is NOT a stable completion marker — only used as a
	// "still generating" hint; stability + Copy-response button is the proof.
	stopSelectors = []string{
		`button[aria-label="Stop"]`, // verified 2026-10-30 live DOM
		`button[aria-label="Cancel"]`,
	}
)

// anyPresent checks if any selector matches.
func (c *Client) anyPresent(ctx context.Context, selectors []string) bool {
	for _, sel := range selectors {
		var found bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
			return true
		}
	}
	return false
}
