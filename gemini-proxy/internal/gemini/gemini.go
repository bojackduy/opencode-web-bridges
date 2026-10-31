// Package gemini drives gemini.google.com (consumer UI) in a dedicated
// Chrome profile via CDP so that AI agents can use a Gemini Web session
// (no API key, no cost) through a plain ask/answer interface.
//
// The client owns one Chrome instance (launched on demand with its own
// profile directory) and one tab. All operations are serialized through a
// mutex because they share that single tab.
//
// Isolation: default profile ~/.gemini-proxy and CDP port 9223 — never the
// chatgpt bridge's ~/.chatgpt-proxy / 9222. This package never navigates,
// closes, or inspects tabs owned by another bridge.
package gemini

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Defaults.
const (
	DefaultChromePort = 9223
	DefaultTimeout    = 10 * time.Minute
	// HomePage is the Gemini consumer UI. /app is the direct chat route;
	// the bare domain redirects there when logged in, to a sign-in
	// interstitial when not.
	HomePage = "https://gemini.google.com/app"
)

// Errors returned by Ask/Status. Callers can use errors.Is to give agents
// actionable messages.
var (
	ErrChromeNotFound      = errors.New("chrome binary not found")
	ErrChromeLaunch        = errors.New("failed to launch chrome")
	ErrNotLoggedIn         = errors.New("not logged in to gemini.google.com (run gemini-proxy -wait-login and sign in with Google once)")
	ErrNoComposer          = errors.New("gemini composer not found")
	ErrNoResponse          = errors.New("gemini did not produce a response in time")
	ErrMissingAnswer       = errors.New("Gemini generation ended without an extractable new model answer; inspect the tab and retry; no automatic resubmission")
	ErrNotSubmitted        = errors.New("turn failed before any submission action")
	ErrSubmissionUncertain = errors.New("submission action outcome is uncertain; no automatic resubmission")
	// ErrForeignDraft marks a refusal that retrying cannot fix: the composer
	// holds text this bridge cannot prove it wrote. It deliberately does not
	// wrap ErrNotSubmitted, so callers cannot downgrade it to a safe retry.
	ErrForeignDraft = errors.New("composer holds a draft this bridge cannot verify; refusing to overwrite it")
	ErrDraftOwned   = errors.New("composer holds this bridge's own draft from a turn whose submission may have been dispatched; refusing to touch it")
	// ErrThreadBinding marks a refusal to submit anywhere except the
	// session's saved thread URL: restore failed, the tab sits on a
	// different conversation, or auth/challenge/redirect blocks the
	// restore. It always wraps ErrNotSubmitted too (see threadFail), so
	// both errors.Is(err, ErrNotSubmitted) and IsPreSubmit stay true:
	// provably nothing was typed or sent.
	ErrThreadBinding = errors.New("saved thread unavailable; refusing to submit elsewhere")
)

// ValidThreadURL reports whether u is an exact Gemini conversation URL the
// bridge may submit to: HomePage plus a non-empty conversation id. The bare
// HomePage (fresh-chat landing, no thread) is never a valid binding target,
// so a rebuilt tab sitting on it can never satisfy a delta submit.
func ValidThreadURL(u string) bool {
	const prefix = "https://gemini.google.com/app/"
	if !strings.HasPrefix(u, prefix) {
		return false
	}
	rest := strings.TrimPrefix(u, prefix)
	rest = strings.TrimSuffix(rest, "/")
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '=' {
			continue
		}
		return false
	}
	return true
}

// threadFail builds the typed pre-submit binding failure: provably nothing
// was typed or sent. reason is a fixed code (counts/flags only, never URLs
// or content): missing-or-invalid-target, wrong-conversation,
// restore-redirect, restore-auth, restore-challenge, restore-navigate,
// binding-unsupported.
func threadFail(reason string) error {
	return fmt.Errorf("thread binding %s: %w", reason, errors.Join(ErrNotSubmitted, ErrThreadBinding))
}

// IsPreSubmit reports a failure proven to have happened before any submission
// action, so the session stays clean instead of becoming uncertain. It does NOT
// imply retrying helps: a foreign draft is pre-submit yet needs a person.
func IsPreSubmit(err error) bool {
	return errors.Is(err, ErrNotSubmitted) || errors.Is(err, ErrForeignDraft) || errors.Is(err, ErrDraftOwned) || errors.Is(err, ErrThreadBinding)
}

// IsNavigateError reports a page-navigation failure. All navigate steps run
// before anything is typed or submitted, so a caller that sees only such an
// error knows provably that nothing was sent and may safely retry instead of
// treating the turn as uncertain.
func IsNavigateError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "navigate:")
}

// ---------------------------------------------------------------------------
// Selector inventory (Gemini consumer UI, October 2026).
//
// Signed-in Vietnamese DOM verified 2026-10-14: account SignOutOptions,
// Quill rich-textarea.__quill.getText(), localized send button, response
// message-content IDs, markdown aria-busy=false and response-footer.complete.
// English fallbacks remain provisional. Do not blind-retry on selector drift.
// ---------------------------------------------------------------------------

var (
	// loginSelectors match the signed-out state: the "Sign in" affordance
	// on the gemini.google.com interstitial. Must be valid CSS for
	// querySelector. Live-verified 2026-10-14 (signed-out DOM): both the
	// link and the button variants exist with aria-label="Sign in".
	loginSelectors = []string{
		`a[aria-label*="Sign in"]`,     // sign-in link (verified hit)
		`button[aria-label="Sign in"]`, // sign-in button (verified in button inventory)
	}
	// loginTextJS is the fallback text scan for locales where the sign-in
	// control carries no aria-label (querySelector can't match text).
	loginTextJS = `(function(){return Array.from(document.querySelectorAll('a,button')).some(function(el){return /sign in/i.test(el.textContent||'')&&el.offsetParent!==null;});})()`
	// loggedInSelectors match the signed-in state: the Google-account avatar
	// (account switcher) present on every logged-in Google surface.
	loggedInSelectors = []string{
		`a[href^="https://accounts.google.com/SignOutOptions"]`, // signed-in avatar, locale-independent; verified vi 2026-10-14
		`a[aria-label^="Google Account"]`,                       // account avatar link ("Google Account: Name <mail>")
		`img[aria-label*="Google Account"]`,                     // avatar image fallback
		`div[aria-label*="Google Account"]`,                     // avatar container fallback
	}
	// composerSelectors match the prompt box, in priority order. Gemini's
	// editor is a Quill instance inside the rich-textarea component.
	// Live-verified 2026-10-14 (signed-out DOM): the editable div carries
	// aria-label="Enter a prompt for Gemini" and data-placeholder="Ask
	// Gemini" (the placeholder is NOT the aria-label — first guess wrong).
	composerSelectors = []string{
		`div.ql-editor[role="textbox"][contenteditable="true"]`,                         // signed-in Vietnamese Quill, verified 2026-10-14
		`div.ql-editor[aria-label="Enter a prompt for Gemini"][contenteditable="true"]`, // primary prompt box (verified hit)
		`rich-textarea div[contenteditable="true"]`,                                     // component-scoped fallback (verified hit)
		`div.ql-editor[contenteditable="true"]`,                                         // Quill editor fallback (verified hit)
	}
	// sendSelectors match the submit button, in priority order.
	// The localized control appears after a user-source Quill document update.
	// Signed-out composer presence is not evidence of a functional submit.
	sendSelectors = []string{
		`button[aria-label="Gửi tin nhắn"]`, // enabled signed-in vi control, verified after native input 2026-10-14
		`button[aria-label="Send message"]`, // guessed primary (UNVERIFIED)
		`button.send-button`,                // class-based fallback (UNVERIFIED)
	}
	// newChatSelectors match the "New chat" affordance. Live-verified
	// 2026-10-14: the LINK variant hits on the signed-out DOM.
	newChatSelectors = []string{
		`a[aria-label="New chat"]`,      // link variant (verified hit)
		`button[aria-label="New chat"]`, // side-nav button variant (unseen signed-out)
	}
	// stopSelectors match the "Stop" button shown while generating; its
	// disappearance marks generation end.
	stopSelectors = []string{
		`button[aria-label="Dừng phản hồi"]`,
		`button[aria-label="Dừng"]`,
		`button[aria-label="Stop"]`, // stop-generation button
	}
	// answerSelectors scope the newest model answer, in priority order.
	// model-response is Gemini's custom element per turn.
	answerSelectors = []string{
		`model-response`,                // per-turn custom element (primary)
		`div.model-response-text`,       // response text container
		`div.markdown-container`,        // rendered Markdown container
		`div[data-message-id*="model"]`, // message-id fallback
	}
)

// Status describes the current state of the Gemini Web tab.
type Status struct {
	LoggedIn   bool   `json:"logged_in"`
	Composer   bool   `json:"composer"`
	NewChat    bool   `json:"new_chat_button"`
	LoginBtn   bool   `json:"login_button"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Profile    string `json:"chrome_profile"`
	ChromePort int    `json:"chrome_port"`
}

// Client owns the Chrome instance and the shared tab.
type Client struct {
	ChromePort int
	ProfileDir string
	ChromePath string // optional override; auto-detected when empty
	Background bool   // run Chrome window in background (off-screen / minimized)

	mu      sync.Mutex
	life    context.Context // long-lived parent for browser/tab contexts
	browser context.Context
	cancel  context.CancelFunc
	// wsURL is the debugger endpoint the allocator is bound to. A Chrome
	// restart mints a new endpoint UUID, so it identifies staleness.
	wsURL           string
	tab             context.Context
	tabDone         context.CancelFunc
	launchAttempted bool
	launchPort      int
	actualPort      int
	ownedTarget     target.ID
	draftPrompt     string // retained only after a provably unsubmitted insertion
	draftIdentity   string // adapter session + exact request; never cross-client reuse
	// draftHash fingerprints the logical document this bridge inserted. With
	// draftPrompt it proves ownership across a page reload, where Quill's
	// page-scoped marker is gone.
	draftHash string
	// draftDoc is the EXACT logical document this bridge left, draftTarget is
	// the CDP target it was written on and draftThread the conversation URL at
	// that moment. Ownership requires all three to match: a marker or a hash
	// alone on a different tab or thread is not proof.
	draftDoc    string
	draftTarget string
	draftThread string
	// draftSubmitted fences the draft permanently once a Send may have fired:
	// the composer still holding it is never evidence that the turn did not go.
	draftSubmitted bool
}

// New builds a client with defaults for anything zero-valued.
func New(port int, profileDir, chromePath string) *Client {
	if port == 0 {
		port = DefaultChromePort
	}
	if profileDir == "" {
		dir, err := os.UserHomeDir()
		if err == nil && dir != "" {
			profileDir = filepath.Join(dir, ".gemini-proxy")
		} else {
			profileDir = ".gemini-proxy"
		}
	}
	return &Client{ChromePort: port, ProfileDir: profileDir, ChromePath: chromePath,
		life: context.Background()}
}

// Close shuts down the CDP connection (not the Chrome process — the user's
// session stays alive in the profile for the next run).
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teardownLocked()
}

// teardownLocked drops the tab and allocator contexts. Callers must hold c.mu.
// Nil-safe: double teardown is a no-op.
func (c *Client) teardownLocked() {
	if c.ownedTarget != "" {
		// Only explicit newly-created targets are ours to close. Context
		// cancellation of an attached target merely detaches its CDP session.
		client := http.Client{Timeout: 2 * time.Second}
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", c.actualPort, c.ownedTarget)); err == nil {
			resp.Body.Close()
		}
		c.ownedTarget = ""
	}
	if c.tabDone != nil {
		c.tabDone()
		c.tabDone = nil
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.tab = nil
	c.browser = nil
	c.wsURL = ""
	c.draftPrompt = ""
	c.draftIdentity = ""
	c.draftDoc = ""
	c.draftTarget = ""
	c.draftThread = ""
}

// tabAlive exercises the full CDP chain (tab session -> allocator websocket)
// with a trivial read, so a silently dropped connection is rebuilt instead
// of failing every later Run.
func (c *Client) tabAlive() bool {
	if c.tab == nil || c.browser == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(c.tab, 5*time.Second)
	defer cancel()
	var url string
	if err := chromedp.Run(ctx, chromedp.Location(&url)); err != nil {
		return false
	}
	return true
}

// portFree reports whether localhost:port accepts no TCP connection.
func portFree(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return true
	}
	conn.Close()
	return false
}

// chromeBinary resolves the Chrome executable: explicit override, then the
// usual macOS / Linux / Windows locations, then PATH.
func chromeBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
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
	if p, err := exec.LookPath("chromium"); err == nil {
		return p, nil
	}
	return "", ErrChromeNotFound
}

// ensureBrowserLocked attaches to (or launches) Chrome and prepares OUR OWN
// dedicated tab on the Gemini page. Callers must hold c.mu.
//
// Lifecycle (mirrors chatgpt-proxy's proven shape):
//  1. attach to an already-running debugger on the isolated port, else launch
//     headless/visible Chrome with the isolated profile;
//  2. create our tab through the DevTools HTTP API (/json/new) — reliable
//     even when no window exists yet, unlike Target.createTarget over the
//     websocket ("no browser is open");
//  3. attach the CDP session to that explicit target ID and verify it with a
//     no-op Run before trusting it.
//
// A connection blip during a fresh launch makes chromedp cancel the whole tab
// context (its LostConnection handler cancels the tab root, so every later
// Run fails instantly with "context canceled" while the browser stays alive).
// Hence the verify step plus retry-until-deadline: a dead first build is
// torn down and rebuilt, never cached.
func (c *Client) ensureBrowserLocked(ctx context.Context) error {
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

// tryEnsureLocked performs one attach-or-launch + own-tab + verify pass.
// Callers must hold c.mu. On any failure the caller tears down and retries.
func (c *Client) tryEnsureLocked(ctx context.Context) error {
	// 1. Debugger endpoint: attach to ours if already up, else launch.
	port := c.ChromePort
	if !c.debuggerUp(port) {
		if !portFree(port) {
			return errors.New("configured Chrome port is occupied; refusing another port")
		}
		if err := c.launchLocked(ctx, port); err != nil {
			return err
		}
	} else {
		log.Printf("[gemini] attaching to existing debugger on %d", port)
	}
	wsURL, err := c.waitDebugger(port, 20*time.Second)
	if err != nil {
		return err
	}
	if err := verifyProfile(port, c.ProfileDir); err != nil {
		return err
	}
	allocCtx, cancel := chromedp.NewRemoteAllocator(c.life, wsURL)
	c.browser = allocCtx
	c.cancel = cancel
	c.wsURL = wsURL
	c.actualPort = port

	// 2. Our OWN tab via DevTools HTTP (never adopt another bridge's tab:
	//    Phase 1 always mints a dedicated target on OUR debugger).
	tid, err := c.createOwnTab(port)
	if err != nil {
		return err
	}
	log.Printf("[gemini] created own tab %s on %d", tid, port)
	c.ownedTarget = tid

	// 3. Attach to the explicit target and verify the session is live.
	// CRITICAL: the FIRST Run on a new tab context must use the tab context
	// itself, never a timeout child. chromedp's allocator binds its lifetime
	// watcher to the first Run's context: cancelling that context (e.g. via
	// defer on a timeout child at function return) makes the watcher call
	// Cancel on the SHARED tab root — every later Run then fails instantly
	// with "context canceled" and the target is closed. The tab root lives
	// until teardown, so verifying on it is safe; the caller's context still
	// bounds the public call.
	tab, done := chromedp.NewContext(c.browser, chromedp.WithTargetID(tid))
	if err := chromedp.Run(tab, chromedp.ActionFunc(func(context.Context) error { return nil })); err != nil {
		done()
		return fmt.Errorf("tab verify: %w", err)
	}
	c.tab = tab
	c.tabDone = done
	log.Printf("[gemini] tab %s verified; navigating to %s", tid, HomePage)

	// 4. Load the Gemini page in OUR tab (navigating our own target only),
	// then actively poll for settle. A blind Sleep leaves the tab's
	// websocket idle while headless software rendering pegs the renderer;
	// chromedp reads that as a lost connection and cancels the whole tab
	// context (every later Run then fails with "context canceled" and the
	// target is closed). Polling keeps the websocket warm and adapts the
	// wait to the real load.
	nav, cancelNav := context.WithTimeout(c.tab, 90*time.Second)
	defer cancelNav()
	if err := chromedp.Run(nav, chromedp.Navigate(HomePage)); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if err := c.pollPageSettledLocked(nav); err != nil {
		return err
	}
	log.Printf("[gemini] tab %s settled at %s (tab.Err=%v)", tid, HomePage, c.tab.Err())
	return nil
}

// pollPageSettledLocked waits (actively) until the page reports
// readyState==complete with a non-empty title, up to ~30s. Each poll is a
// real CDP round-trip, keeping the tab websocket warm while heavy JS loads.
// Callers must hold c.mu.
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

// debuggerUp reports whether the DevTools /json/version endpoint answers.
func (c *Client) debuggerUp(port int) bool {
	_, err := c.debuggerWSURL(port)
	return err == nil
}

// debuggerWSURL fetches the browser websocket URL from /json/version.
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

// waitDebugger polls /json/version until it answers or timeout.
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

// launchLocked starts Chrome with the isolated profile on the given free
// port. Callers must hold c.mu.
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
		return fmt.Errorf("%w: %v", ErrChromeLaunch, err)
	}
	go func() { _ = cmd.Wait() }()
	c.launchAttempted = true
	c.launchPort = port
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// createOwnTab mints a dedicated page target via the DevTools HTTP API.
func (c *Client) createOwnTab(port int) (target.ID, error) {
	client := http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", port, HomePage), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create tab: %w", err)
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("create tab: %w", err)
	}
	if created.ID == "" {
		return "", errors.New("empty target id from /json/new")
	}
	return target.ID(created.ID), nil
}

// jsBool runs a JS expression expected to yield a boolean.
func (c *Client) jsBool(ctx context.Context, expr string) bool {
	var out bool
	_ = chromedp.Run(ctx, chromedp.Evaluate(expr, &out))
	return out
}

// jsString runs a JS expression expected to yield a string.
func (c *Client) jsString(ctx context.Context, expr string) string {
	var out string
	_ = chromedp.Run(ctx, chromedp.Evaluate(expr, &out))
	return out
}

// signInPresent reports whether a visible sign-in affordance exists
// (aria-label selector hit OR visible link/button text scan).
func (c *Client) signInPresent(ctx context.Context) bool {
	if c.anyPresent(ctx, loginSelectors) {
		return true
	}
	return c.jsBool(ctx, loginTextJS)
}

// anyPresent reports whether any selector in the list matches at least one
// node in the page.
func (c *Client) anyPresent(ctx context.Context, selectors []string) bool {
	for _, sel := range selectors {
		if c.jsBool(ctx, fmt.Sprintf(`!!document.querySelector(%q)`, sel)) {
			return true
		}
	}
	return false
}

// isStaleErr reports transport-level failures that mean the tab session is
// dead (not a page problem): the caller should rebuild and retry. It must
// NEVER match page-level states (not-logged-in, no-composer, no-answer) —
// those are answers, not transport faults.
func isStaleErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "websocket") ||
		strings.Contains(msg, "lost connection") ||
		strings.Contains(msg, "target closed") ||
		strings.Contains(msg, "no such target")
}

// LoginState attaches (read-only) and reports the signed-in state without
// typing or navigating away from the current page. Transport blips rebuild
// the tab transparently (bounded); page states are returned as-is.
func (c *Client) LoginState(ctx context.Context) (*Status, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		st, err := c.loginStateOnce(ctx)
		if err == nil {
			return st, nil
		}
		lastErr = err
		if !isStaleErr(err) {
			return nil, err
		}
		c.mu.Lock()
		c.teardownLocked()
		c.mu.Unlock()
		log.Printf("[gemini] stale tab during LoginState (attempt %d): %v; rebuilding", attempt+1, err)
	}
	return nil, lastErr
}

func (c *Client) loginStateOnce(ctx context.Context) (*Status, error) {
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
	st.LoginBtn = c.signInPresent(rctx)
	st.LoggedIn = c.anyPresent(rctx, loggedInSelectors) && !st.LoginBtn
	// A generic Google-account avatar also counts (some locales render the
	// switcher without the exact "Google Account" prefix).
	if !st.LoggedIn {
		st.LoggedIn = c.jsBool(rctx, `!!document.querySelector('a[aria-label*="Account"]') && !document.querySelector('a[aria-label*="Sign in"]')`)
	}
	st.Composer = c.anyPresent(rctx, composerSelectors)
	st.NewChat = c.anyPresent(rctx, newChatSelectors)
	return st, nil
}

// CurrentURL reports the owned tab's current URL without typing,
// submitting, or navigating. Read-only probe for the adapter's sent-log
// thread linkage. "" means the browser was unreachable, never evidence
// about thread state.
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

// Status attaches (or launches) and reports the full tab state.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	return c.LoginState(ctx)
}

// Ask sends prompt to Gemini Web and returns the answer as Markdown.
// When newSession is true it starts a fresh chat first; otherwise it
// continues the current conversation.
func (c *Client) Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error) {
	text, _, err := c.askRich(ctx, prompt, newSession, timeout, false)
	return text, err
}

// AskWithImages sends prompt to Gemini Web and returns the answer as
// Markdown plus the retrievable output-image parts (inline data: payloads
// within the documented caps). The text always carries the ChatGPT-consistent
// fixtures and omit-notes, so callers that ignore parts still see explained
// placeholders instead of bare "[Image 1]" stubs. Image collection is
// read-only DOM/CDP (fetch of same-document blob: payloads and canvas
// readback only) — no clicks, typing, submission, navigation, or tab churn
// beyond the single submitted turn itself.
func (c *Client) AskWithImages(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, []ResponseImage, error) {
	return c.askRich(ctx, prompt, newSession, timeout, true)
}

// AskThread sends prompt bound to threadURL: for continuations the tab is
// restored to and verified on the saved thread after prepare and again
// immediately before typing, inside the same tab lock that submits. A fresh
// chat (newSession) ignores the binding and mints its thread; the caller
// binds later chunks to the minted URL. Every binding failure is a typed
// pre-submit refusal with zero typing and zero sends.
func (c *Client) AskThread(ctx context.Context, prompt, threadURL string, newSession bool, timeout time.Duration) (string, error) {
	text, _, err := c.askRichThread(ctx, prompt, threadURL, newSession, timeout, false)
	return text, err
}

// AskWithImagesThread is AskThread with retrievable output-image parts.
func (c *Client) AskWithImagesThread(ctx context.Context, prompt, threadURL string, newSession bool, timeout time.Duration) (string, []ResponseImage, error) {
	return c.askRichThread(ctx, prompt, threadURL, newSession, timeout, true)
}

// askRichThread is the thread-bound submit-and-wait flow. Fresh turns behave
// exactly like askRich; continuation turns verify the saved thread under the
// submit lock so a reconnect between prepare and typing cannot strand the
// suffix on a rebuilt homepage tab.
func (c *Client) askRichThread(ctx context.Context, prompt, threadURL string, newSession bool, timeout time.Duration, collectImages bool) (text string, images []ResponseImage, err error) {
	if strings.TrimSpace(prompt) == "" {
		return "", nil, errors.New("prompt must not be empty")
	}
	if !newSession && !ValidThreadURL(threadURL) {
		return "", nil, threadFail("missing-or-invalid-target")
	}
	if timeout <= 0 || timeout > 30*time.Minute {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := flowID(ctx)
	if id == "" {
		id = NewFlowID()
		ctx = WithFlowID(ctx, id)
	}
	started := time.Now()
	log.Printf("[gemini-flow] id=%s stage=start prompt_bytes=%d new_session=%v timeout=%s", id, len(prompt), newSession, timeout)
	defer func() {
		log.Printf("[gemini-flow] id=%s stage=finish err=%v answer_bytes=%d images=%d duration=%s",
			id, err, len(text), len(images), time.Since(started).Round(time.Millisecond))
	}()

	prepStart := time.Now()
	log.Printf("[gemini-flow] id=%s stage=prepare start new_session=%v", id, newSession)
	beforeIDs, err := c.prepareSubmission(ctx, prompt, newSession)
	if err != nil {
		log.Printf("[gemini-flow] id=%s stage=prepare result=error duration=%s err=%v", id, time.Since(prepStart).Round(time.Millisecond), err)
		return "", nil, err
	}
	log.Printf("[gemini-flow] id=%s stage=prepare result=ok prior_answers=%d duration=%s", id, len(beforeIDs), time.Since(prepStart).Round(time.Millisecond))

	c.mu.Lock()
	defer c.mu.Unlock()
	if !newSession {
		if verr := c.verifyThreadLocked(ctx, threadURL); verr != nil {
			return "", nil, verr
		}
	}
	if err := c.typeAndSubmitLocked(ctx, prompt); err != nil {
		return "", nil, err
	}
	html, err := c.waitForAnswerLocked(ctx, beforeIDs)
	if err != nil {
		return "", nil, err
	}
	if !collectImages {
		text := ConvertResponseHTML(html)
		if strings.TrimSpace(text) == "" {
			return "", nil, ErrMissingAnswer
		}
		return text, nil, nil
	}
	live := c.collectLiveImagesLocked(ctx)
	text, parts := ConvertResponseWithLiveImages(html, live.Imgs, live.Canvas)
	if strings.TrimSpace(text) == "" && len(parts) == 0 {
		return "", nil, ErrMissingAnswer
	}
	// An image-only answer still needs text beside the parts: the fixtures
	// above guarantee non-empty text whenever nodes were seen; an empty
	// fragment with live pixels alone keeps the parts with a short note.
	if strings.TrimSpace(text) == "" {
		text = "[Image: model-generated visual content]"
	}
	return text, parts, nil
}

// askRich is the shared submit-and-wait flow for Ask/AskWithImages.
// With collectImages it additionally resolves image bytes read-only after
// the answer stabilizes; without it the behavior is exactly Ask's.
func (c *Client) askRich(ctx context.Context, prompt string, newSession bool, timeout time.Duration, collectImages bool) (text string, images []ResponseImage, err error) {
	if strings.TrimSpace(prompt) == "" {
		return "", nil, errors.New("prompt must not be empty")
	}
	if timeout <= 0 || timeout > 30*time.Minute {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := flowID(ctx)
	if id == "" {
		id = NewFlowID()
		ctx = WithFlowID(ctx, id)
	}
	started := time.Now()
	log.Printf("[gemini-flow] id=%s stage=start prompt_bytes=%d new_session=%v timeout=%s", id, len(prompt), newSession, timeout)
	defer func() {
		log.Printf("[gemini-flow] id=%s stage=finish err=%v answer_bytes=%d images=%d duration=%s",
			id, err, len(text), len(images), time.Since(started).Round(time.Millisecond))
	}()

	prepStart := time.Now()
	log.Printf("[gemini-flow] id=%s stage=prepare start new_session=%v", id, newSession)
	beforeIDs, err := c.prepareSubmission(ctx, prompt, newSession)
	if err != nil {
		log.Printf("[gemini-flow] id=%s stage=prepare result=error duration=%s err=%v", id, time.Since(prepStart).Round(time.Millisecond), err)
		return "", nil, err
	}
	log.Printf("[gemini-flow] id=%s stage=prepare result=ok prior_answers=%d duration=%s", id, len(beforeIDs), time.Since(prepStart).Round(time.Millisecond))

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.typeAndSubmitLocked(ctx, prompt); err != nil {
		return "", nil, err
	}
	html, err := c.waitForAnswerLocked(ctx, beforeIDs)
	if err != nil {
		return "", nil, err
	}
	if !collectImages {
		text := ConvertResponseHTML(html)
		if strings.TrimSpace(text) == "" {
			return "", nil, ErrMissingAnswer
		}
		return text, nil, nil
	}
	live := c.collectLiveImagesLocked(ctx)
	text, parts := ConvertResponseWithLiveImages(html, live.Imgs, live.Canvas)
	if strings.TrimSpace(text) == "" && len(parts) == 0 {
		return "", nil, ErrMissingAnswer
	}
	// An image-only answer still needs text beside the parts: the fixtures
	// above guarantee non-empty text whenever nodes were seen; an empty
	// fragment with live pixels alone keeps the parts with a short note.
	if strings.TrimSpace(text) == "" {
		text = "[Image: model-generated visual content]"
	}
	return text, parts, nil
}

// prepareSubmission runs the bounded pre-submission phase: draft-identity
// guard, auth-gated turn preparation, and the before-set snapshot. It
// provably types and submits nothing, so transport blips rebuild instead of
// risking a double submit. Callers must not hold c.mu.
func (c *Client) prepareSubmission(ctx context.Context, prompt string, newSession bool) (map[string]bool, error) {
	// Pre-submission phase (bounded retry): ensure, auth gate, new chat.
	// Retries here are provably safe — nothing is typed or submitted yet,
	// so a transport blip rebuilds instead of risking a double submit.
	var beforeIDs map[string]bool
	submissionReady := false
	var lastErr error
	for attempt := 0; attempt < 2 && !submissionReady; attempt++ {
		if err := c.prepareTurn(ctx, newSession); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: prepare canceled: %v", ErrNotSubmitted, ctx.Err())
			}
			if errors.Is(err, ErrNotLoggedIn) {
				return nil, err // page state, not transport: never churn on this
			}
			if !isStaleErr(err) && !IsNavigateError(err) && !errors.Is(err, ErrNotSubmitted) {
				return nil, fmt.Errorf("%w: prepare: %v", ErrNotSubmitted, err)
			}
			c.mu.Lock()
			c.teardownLocked()
			c.mu.Unlock()
			log.Printf("[gemini] pre-submission blip (attempt %d): %v; rebuilding", attempt+1, err)
			continue
		}
		c.mu.Lock()
		var err error
		beforeIDs, err = c.modelMessageIDsLocked(ctx)
		c.mu.Unlock()
		if err != nil {
			lastErr = err
			continue
		}
		submissionReady = true
	}
	if !submissionReady {
		if lastErr == nil {
			lastErr = errors.New("could not prepare turn")
		}
		return nil, fmt.Errorf("%w: prepare: %v", ErrNotSubmitted, lastErr)
	}
	return beforeIDs, nil
}

// prepareTurn ensures the browser, enforces the auth gate, and starts
// a fresh chat when asked. It never types or submits: safe to retry.
// It locks internally per step; callers must not hold c.mu.
func (c *Client) prepareTurn(ctx context.Context, newSession bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: prepare canceled: %v", ErrNotSubmitted, err)
	}
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return err
	}

	// Fail closed on auth: never attempt to bypass Google sign-in.
	loggedIn, err := c.checkLoggedInLocked(ctx)
	if err != nil {
		return err
	}
	if !loggedIn {
		return ErrNotLoggedIn
	}

	if newSession {
		if err := c.newChatLocked(ctx); err != nil {
			return err
		}
	}
	return nil
}

// checkLoggedInLocked reports the signed-in state. Callers must hold c.mu.
func (c *Client) checkLoggedInLocked(ctx context.Context) (bool, error) {
	rctx, cancel := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel()
	if c.signInPresent(rctx) {
		return false, nil
	}
	if c.anyPresent(rctx, loggedInSelectors) {
		return true, nil
	}
	if err := rctx.Err(); err != nil {
		return false, fmt.Errorf("%w: login check interrupted: %v", ErrNotSubmitted, err)
	}
	// No sign-in evidence above, but a composer alone does NOT prove login:
	// the signed-out page renders one too (live-verified 2026-10-14). Only
	// the account avatar proves it — otherwise fail closed.
	return false, nil
}

// newChatLocked starts a fresh Gemini conversation. Callers must hold c.mu.
func (c *Client) newChatLocked(ctx context.Context) error {
	rctx, cancel := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel()
	for _, sel := range newChatSelectors {
		clicked := false
		_ = chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(
			`(function(){var el=document.querySelector(%q);if(!el)return false;el.click();return true;})()`, sel), &clicked))
		if clicked {
			_ = chromedp.Run(rctx, chromedp.Sleep(2*time.Second))
			return nil
		}
	}
	// Fallback: direct navigation to a fresh /app URL starts a new chat.
	// This navigates OUR OWN dedicated tab only.
	nctx, cancel2 := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel2()
	if err := chromedp.Run(nctx,
		chromedp.Navigate(HomePage),
		chromedp.Sleep(3*time.Second),
	); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	return nil
}

// currentURLLocked reports the owned tab's URL. Callers must hold c.mu.
func (c *Client) currentURLLocked(ctx context.Context) (string, error) {
	rctx, cancel := operationContext(c.tab, ctx, 10*time.Second)
	defer cancel()
	var url string
	if err := chromedp.Run(rctx, chromedp.Location(&url)); err != nil {
		return "", fmt.Errorf("navigate: thread location unreadable: %v", err)
	}
	return url, nil
}

// verifyThreadLocked restores and verifies the owned tab sits on threadURL.
// Callers must hold c.mu. It types and submits nothing: every failure is a
// typed pre-submit binding refusal (see threadFail), never a fallback to a
// new chat. Lifecycle note: tryEnsureLocked always rebuilds a dead tab on
// the bare HomePage, so a tab recreated before or between chunks lands on
// the homepage by design; this verifier routes it back to the saved thread
// per transaction instead of submitting into the empty chat.
func (c *Client) verifyThreadLocked(ctx context.Context, threadURL string) error {
	if !ValidThreadURL(threadURL) {
		return threadFail("missing-or-invalid-target")
	}
	cur, err := c.currentURLLocked(ctx)
	if err != nil {
		return threadFail("restore-navigate")
	}
	if cur == threadURL {
		return nil
	}
	// The tab is elsewhere (rebuilt homepage, operator-moved tab, redirect
	// residue): navigate to the saved thread and re-verify exactly.
	nctx, cancel := operationContext(c.tab, ctx, 60*time.Second)
	defer cancel()
	if err := chromedp.Run(nctx, chromedp.Navigate(threadURL)); err != nil {
		return fmt.Errorf("navigate: thread restore: %w", err)
	}
	if err := c.pollPageSettledLocked(nctx); err != nil {
		return threadFail("restore-navigate")
	}
	if c.signInPresent(nctx) {
		return threadFail("restore-auth")
	}
	after, err := c.currentURLLocked(ctx)
	if err != nil {
		return threadFail("restore-navigate")
	}
	if after != threadURL {
		return threadFail("restore-redirect")
	}
	// Restored onto the thread, but only a signed-in composer proves the
	// conversation is usable; anything else (challenge, consent, wrong
	// conversation shell) fails closed.
	rctx, cancel2 := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel2()
	loggedIn := c.anyPresent(rctx, loggedInSelectors) && !c.signInPresent(rctx)
	if !loggedIn {
		return threadFail("restore-challenge")
	}
	return nil
}

// EnsureThread binds the owned tab to threadURL: attach/reconnect, auth
// gate, restore, and exact verification. It never types or submits: safe to
// call before every chunk. Callers must not hold c.mu.
func (c *Client) EnsureThread(ctx context.Context, threadURL string) error {
	if !ValidThreadURL(threadURL) {
		return threadFail("missing-or-invalid-target")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: ensure canceled: %v", ErrNotSubmitted, err)
	}
	if err := c.ensureBrowserLocked(ctx); err != nil {
		return err
	}
	loggedIn, err := c.checkLoggedInLocked(ctx)
	if err != nil {
		return err
	}
	if !loggedIn {
		return ErrNotLoggedIn
	}
	return c.verifyThreadLocked(ctx, threadURL)
}

// modelMessageIDsLocked snapshots the current set of model-response nodes so
// the poller can identify the NEW answer. Callers must hold c.mu.
func (c *Client) modelMessageIDsLocked(ctx context.Context) (map[string]bool, error) {
	rctx, cancel := operationContext(c.tab, ctx, 15*time.Second)
	defer cancel()
	var values []string
	if err := chromedp.Run(rctx, chromedp.Evaluate(
		`Array.from(document.querySelectorAll('model-response message-content[id]')).map(e=>e.id)`, &values)); err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(values))
	for _, id := range values {
		ids[id] = true
	}
	return ids, nil
}

// operationContext cancels a tab-scoped operation on either the caller's
// cancellation or the local stage budget, without canceling the tab root.
func operationContext(tab, caller context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(tab, budget)
	stop := context.AfterFunc(caller, cancel)
	if caller.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

type turnIdentityKey struct{}

type flowIDKey struct{}

// flowID names one browser turn in the flow log so a whole request can be read
// as a timeline. Empty when the caller is not an ask.
func flowID(ctx context.Context) string {
	id, _ := ctx.Value(flowIDKey{}).(string)
	return id
}

// NewFlowID names one browser turn in the flow log. The adapter creates it so
// its own request line and every browser stage share a single id.
func NewFlowID() string {
	return fmt.Sprintf("gemini-%x", time.Now().UnixNano())
}

// WithFlowID pins a caller-created flow id. Plain MCP calls omit it and askRich
// makes one.
func WithFlowID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, flowIDKey{}, id)
}

// jsStringArray renders a Go slice as a JavaScript string array literal.
func jsStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = jsQuote(item)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// WithTurnIdentity binds an adapter retry to both its session and exact request.
// Plain MCP Ask calls retain their existing single-consumer behavior.
func WithTurnIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, turnIdentityKey{}, identity)
}

// geminiDraftState is one read of the LOGICAL Quill document. `Text` excludes
// the structural trailing newline Quill always keeps. `Rendered` is a hint from
// the visual DOM and must never decide ownership or exactness.
type geminiDraftState struct {
	OK     bool   `json:"ok"`
	Text   string `json:"text"`
	Marked bool   `json:"marked"`
	// The in-page token is structured on purpose: a bare boolean would let a
	// marker on another tab or conversation authorize an overwrite here.
	MarkedText   string `json:"markedText"`
	MarkedTarget string `json:"markedTarget"`
	MarkedThread string `json:"markedThread"`
	Rendered     int    `json:"rendered"`
}

// readComposerJS reads the Quill document through the same accessor the fill
// and verification steps use, so no decision compares two different readings.
func readComposerJS(composer string) string {
	return fmt.Sprintf(`(function(){
var el=document.querySelector(%q), host=el&&el.closest('rich-textarea'), q=host&&host.__quill;
if(!q||typeof q.getText!=='function')return {ok:false,text:'',marked:false,markedText:'',markedTarget:'',markedThread:'',rendered:0};
var raw=q.getText(), text=raw.endsWith('\n')?raw.slice(0,-1):raw;
var m=q.__geminiProxyDraft, mo=(m&&typeof m==='object')?m:null;
return {ok:true,text:text,marked:!!mo,markedText:mo&&typeof mo.text==='string'?mo.text:'',markedTarget:mo&&typeof mo.target==='string'?mo.target:'',markedThread:mo&&typeof mo.thread==='string'?mo.thread:'',rendered:(el.textContent||'').trim().length};
})()`, composer)
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// draftHash is a content fingerprint used ONLY as a diagnostic and as an
// integrity check on the recorded snapshot. Ownership compares the exact
// logical document; a truncated digest must never authorize an overwrite.
func draftHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:])
}

// draftHashShort is the log-sized form. It is never used for a decision.
func draftHashShort(s string) string {
	return draftHash(s)[:8]
}

// geminiDraftRecord is the Go-side provenance of the document this bridge last
// left in the composer. Ownership needs all of it: the exact text, its full
// digest, the adapter identity that asked for it, and the tab and conversation
// it was written on.
type geminiDraftRecord struct {
	Prompt    string
	Doc       string
	Hash      string
	Identity  string
	Target    string
	Thread    string
	Submitted bool
}

// decideGeminiDraft is the whole ownership rule. `owned` permits replacing the
// current document; `reuse` means it already is exactly this turn's text.
// Ownership requires the recorded tab and conversation to match and the
// document to equal the exact recorded snapshot. The page-scoped marker is
// corroboration only: on its own, or on a different target/thread, it proves
// nothing. A user edit, a missing record and an unreadable editor all preserve
// the draft.
func decideGeminiDraft(s geminiDraftState, identity, target, thread, prompt string, rec geminiDraftRecord) (owned, reuse bool, why string) {
	if !s.OK {
		return false, false, "no-quill"
	}
	if s.Text == "" {
		return true, false, "empty"
	}
	if rec.Submitted {
		return false, false, "submission-attempted"
	}
	if rec.Target != "" && rec.Thread != "" && rec.Doc != "" {
		if rec.Target != target {
			return false, false, "other-target"
		}
		if rec.Thread != thread {
			return false, false, "other-thread"
		}
		if s.Text != rec.Doc || draftHash(rec.Doc) != rec.Hash {
			return false, false, "unverified-document"
		}
	} else {
		// No Go record: a lost CDP reply leaves a document this bridge did write.
		// The in-page token is acceptable only because it binds the exact text,
		// the exact target and the exact conversation; it is never a bare flag.
		if s.MarkedText == "" || s.MarkedTarget == "" || s.MarkedThread == "" {
			return false, false, "unverified-document"
		}
		if s.MarkedTarget != target {
			return false, false, "other-target"
		}
		if s.MarkedThread != thread {
			return false, false, "other-thread"
		}
		if s.MarkedText != s.Text {
			return false, false, "unverified-document"
		}
	}
	if rec.Identity != "" && rec.Identity == identity && rec.Prompt == prompt {
		return true, true, "same-turn"
	}
	return true, false, "superseded-owned-draft"
}

// fillComposerJS updates the genuine Quill document, not DOM textContent.
// Native Input.insertText triggers extremely slow per-line editor mutation on
// large catalogs. Quill's user-source update notifies the app in one batch.
// Always append a structural newline, even when the prompt already ends in one.
//
// Fire and forget: the document is written and nothing is verified afterwards.
// Whatever the editor holds is what Send will submit, so a stale draft is
// replaced and a partial write may be sent. That tradeoff is deliberate.
func fillComposerJS(composer, prompt string) string {
	return fmt.Sprintf(`(function(){
var el=document.querySelector(%q), host=el&&el.closest('rich-textarea'), q=host&&host.__quill;
if(!q||typeof q.setText!=='function'||typeof q.getText!=='function')return 'no-quill';
var expected=%s+'\n';
if(q.getText()===expected)return 'reused';
q.setText(expected,'user');
return 'inserted';
})()`, composer, jsQuote(prompt))
}

// typeAndSubmitLocked fills the composer and clicks Send. Nothing is verified:
// the document is never compared with the prompt, the click never re-checks the
// editor, and readiness only asks whether the Send control is enabled. The read
// and the click are separate calls so a slow renderer cannot turn a click that
// never happened into an uncertain turn.
func (c *Client) typeAndSubmitLocked(ctx context.Context, prompt string) error {
	id := flowID(ctx)
	rctx, cancel := operationContext(c.tab, ctx, 60*time.Second)
	defer cancel()
	// Gemini code-block reveal animations can remain pending in a hidden tab.
	// Emulate focus only on our target: do not steal OS keyboard focus from
	// the user's terminal (which could otherwise contaminate the composer).
	if err := chromedp.Run(rctx, emulation.SetFocusEmulationEnabled(true)); err != nil {
		return fmt.Errorf("%w: emulate owned-tab focus: %v", ErrNotSubmitted, err)
	}

	log.Printf("[gemini-flow] id=%s stage=composer_wait start prompt_bytes=%d", id, len(prompt))
	composer := ""
	for _, sel := range composerSelectors {
		if c.jsBool(rctx, fmt.Sprintf(`!!document.querySelector(%q)`, sel)) {
			composer = sel
			break
		}
	}
	if composer == "" {
		log.Printf("[gemini-flow] id=%s stage=composer_focus result=missing", id)
		return ErrNoComposer
	}
	log.Printf("[gemini-flow] id=%s stage=composer_focus result=ok", id)

	started := time.Now()
	var filled string
	if err := chromedp.Run(rctx, chromedp.Evaluate(fillComposerJS(composer, prompt), &filled)); err != nil {
		log.Printf("[gemini-flow] id=%s stage=composer_insert result=error err=%v", id, err)
		return fmt.Errorf("%w: Quill input failed (%d bytes, %s): %v", ErrNotSubmitted, len(prompt), time.Since(started).Round(time.Millisecond), err)
	}
	log.Printf("[gemini-flow] id=%s stage=composer_insert result=%s bytes=%d duration=%s", id, filled, len(prompt), time.Since(started).Round(time.Millisecond))
	if filled == "no-quill" {
		return fmt.Errorf("%w: Quill editor unavailable, nothing typed", ErrNotSubmitted)
	}

	// Phase 1 only READS whether a Send control is enabled. A large Quill
	// update can block the renderer for seconds, so this gets its own budget.
	// Nothing is dispatched here: failing it is provably pre-send.
	readyJS := fmt.Sprintf(`(function(){var sels=%s;for(var i=0;i<sels.length;i++){var e=document.querySelector(sels[i]);if(e&&!e.disabled&&e.getAttribute('aria-disabled')!=='true')return true;}return false;})()`, jsStringArray(sendSelectors))
	sendJS := func(sel string) string {
		return fmt.Sprintf(`(function(){var e=document.querySelector(%q);if(!e||e.disabled||e.getAttribute('aria-disabled')==='true')return false;e.click();return true;})()`, sel)
	}
	log.Printf("[gemini-flow] id=%s stage=send_wait start", id)
	readyStart := time.Now()
	readyDeadline := time.Now().Add(30 * time.Second)
	var lastRead error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: canceled before Send: %v", ErrNotSubmitted, err)
		}
		probe, cancelProbe := operationContext(c.tab, ctx, 10*time.Second)
		var enabled bool
		err := chromedp.Run(probe, chromedp.Evaluate(readyJS, &enabled))
		cancelProbe()
		if err == nil && enabled {
			log.Printf("[gemini-flow] id=%s stage=send_ready enabled=true waited=%s", id, time.Since(readyStart).Round(time.Millisecond))
			break
		}
		lastRead = err
		if time.Now().After(readyDeadline) {
			log.Printf("[gemini-flow] id=%s stage=send_ready enabled=false waited=%s last_err=%v", id, time.Since(readyStart).Round(time.Millisecond), lastRead)
			return fmt.Errorf("%w: Send control never became enabled after %s with %d bytes inserted; nothing was submitted",
				ErrNotSubmitted, time.Since(readyStart).Round(time.Millisecond), len(prompt))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: canceled before Send: %v", ErrNotSubmitted, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}

	// Phase 2: dispatch exactly once. Only a failure here can be uncertain.
	for _, sel := range sendSelectors {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: canceled before Send: %v", ErrNotSubmitted, err)
		}
		if !c.jsBool(rctx, fmt.Sprintf(`(function(){var e=document.querySelector(%q);return !!e&&!e.disabled&&e.getAttribute('aria-disabled')!=='true';})()`, sel)) {
			continue
		}
		clickCtx, cancelClick := operationContext(c.tab, ctx, 15*time.Second)
		var clicked bool
		err := chromedp.Run(clickCtx, chromedp.Evaluate(sendJS(sel), &clicked))
		cancelClick()
		log.Printf("[gemini-flow] id=%s stage=send_click selector=%s clicked=%v err=%v", id, sel, clicked, err)
		if err != nil {
			// The click may have happened: keep the turn uncertain so nothing
			// resends automatically.
			return fmt.Errorf("%w: %v", ErrSubmissionUncertain, err)
		}
		if clicked {
			log.Printf("[gemini-flow] id=%s stage=submitted", id)
			// Phase 3: observe only. The composer clearing or a generating
			// indicator is proof the turn reached the thread. Nothing is
			// refused on a miss: the answer wait stays the authority, and this
			// is a breadcrumb for a click that never became a turn.
			observeStart := time.Now()
			observeDeadline := time.Now().Add(10 * time.Second)
			retries := 0
			for time.Now().Before(observeDeadline) {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(700 * time.Millisecond):
				}
				retries++
				octx, ocancel := operationContext(c.tab, ctx, 3*time.Second)
				empty := c.jsBool(octx, fmt.Sprintf(`(function(){var el=document.querySelector(%q),h=el&&el.closest('rich-textarea'),q=h&&h.__quill;return !!q&&q.getText().trim()==='';})()`, composer))
				generating := c.anyPresent(octx, stopSelectors)
				ocancel()
				if empty || generating {
					log.Printf("[gemini-flow] id=%s stage=send_observed result=submitted retries=%d waited=%s",
						id, retries, time.Since(observeStart).Round(time.Millisecond))
					return nil
				}
			}
			log.Printf("[gemini-flow] id=%s stage=send_observed result=unobserved retries=%d waited=%s composer_still_holds_text=true",
				id, retries, time.Since(observeStart).Round(time.Millisecond))
			return nil
		}
	}
	return fmt.Errorf("%w: no enabled send control", ErrNotSubmitted)
}

// waitForAnswerLocked polls until generation finishes and returns the newest
// model-response outerHTML. Callers must hold c.mu.
func (c *Client) waitForAnswerLocked(ctx context.Context, before map[string]bool) (string, error) {
	id := flowID(ctx)
	log.Printf("[gemini-flow] id=%s stage=stream_wait start", id)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	var lastStable string
	stableRounds := 0
	lastLogged := -1
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %v", ErrNoResponse, ctx.Err())
		case <-ticker.C:
			rctx, cancel := operationContext(c.tab, ctx, 15*time.Second)
			id := flowID(ctx)
			generating := c.anyPresent(rctx, stopSelectors)
			msgID := c.jsString(rctx, `(function(){var n=document.querySelectorAll('model-response message-content[id]');return n.length?n[n.length-1].id:'';})()`)
			html := c.jsString(rctx, newestModelResponseJS)
			cancel()
			if len(html) != lastLogged {
				lastLogged = len(html)
				log.Printf("[gemini-flow] id=%s stage=stream_poll answer_bytes=%d generating=%v", id, len(html), generating)
			}
			if html == "" || generating || msgID == "" || before[msgID] {
				stableRounds = 0
				lastStable = html
				continue
			}
			if html == lastStable {
				stableRounds++
			} else {
				stableRounds = 0
				lastStable = html
			}
			// New content, stable across 3 polls (~4.5s) with no stop
			// button: generation is done. The before-set guards against
			// adopting a stale answer when nothing new rendered.
			if stableRounds >= 3 {
				// Rendering grace: one extra beat for late Markdown paint.
				select {
				case <-ctx.Done():
					return "", fmt.Errorf("%w: %v", ErrNoResponse, ctx.Err())
				case <-time.After(2 * time.Second):
				}
				return html, nil
			}
		}
	}
}

// newestModelResponseJS returns the outerHTML of the last model-response
// element (or its fallback containers), or "" when none exists.
const newestModelResponseJS = `(function(){
  var nodes=document.querySelectorAll('model-response');
  if(nodes.length){var n=nodes[nodes.length-1];var body=n.querySelector('message-content .markdown');
    if(body && body.getAttribute('aria-busy')!=='true' && n.querySelector('.response-footer.complete')){
      var clean=body.cloneNode(true);
      clean.querySelectorAll('.code-block-decoration,button,[role="toolbar"]').forEach(e=>e.remove());
      return clean.outerHTML;}}
  return '';
})()`

// responseImagesJS is a READ-ONLY probe of the last model-response's
// image-bearing nodes: current <img> sources plus same-document blob: payload
// resolution, and <canvas> readback. It performs no clicks, typing,
// submission, navigation, reload, or tab churn — attribute reads,
// canvas-to-dataURL, and GET fetches of same-document blob: URLs only.
// Callers evaluate it with await-promise semantics (blob resolution is
// async) under a bounded budget; any failure degrades to empty slots and the
// pure outerHTML path still carries the fixtures.
const responseImagesJS = `(async function(){
  var out={imgs:[],canvas:[]};
  try{
    var nodes=document.querySelectorAll('model-response');
    var scope=document;
    if(nodes.length){scope=nodes[nodes.length-1];}
    var toDataURL=function(blob){return new Promise(function(res){
      try{var fr=new FileReader();
        fr.onload=function(){try{res(fr.result||'');}catch(_){res('');}};
        fr.onerror=function(){res('');};
        fr.readAsDataURL(blob);
      }catch(_){res('');}
    });};
    var imgs=scope.querySelectorAll('img');
    var n=Math.min(imgs.length,8);
    for(var i=0;i<n;i++){
      var el=imgs[i];
      var alt=el.getAttribute('alt')||el.getAttribute('aria-label')||el.getAttribute('title')||'';
      var src=el.currentSrc||el.getAttribute('src')||el.getAttribute('data-src')||'';
      out.imgs.push({alt:alt,src:src,dataURL:''});
    }
    for(var k=0;k<out.imgs.length;k++){
      var rec=out.imgs[k];
      if(rec.src&&rec.src.indexOf('blob:')===0){
        try{
          var r=await fetch(rec.src);
          if(r.ok){var b=await r.blob();rec.dataURL=await toDataURL(b);}
        }catch(_){rec.dataURL='';}
      }
    }
    var cv=scope.querySelectorAll('canvas');
    var m=Math.min(cv.length,4);
    for(var j=0;j<m;j++){
      var c=cv[j];
      var ca=c.getAttribute('alt')||c.getAttribute('aria-label')||'';
      var durl='';
      try{
        if(c.width>0&&c.height>0&&c.width*c.height<=16777216){durl=c.toDataURL('image/png')||'';}
      }catch(_){durl='';}
      out.canvas.push({alt:ca,src:'',dataURL:durl});
    }
  }catch(_){}
  return out;
})()`

// liveImageSlot is one read-only collected image slot, aligned per kind in
// document order with parseImageNodes' within-kind sequences.
type liveImageSlot struct {
	Alt     string `json:"alt"`
	Src     string `json:"src"`
	DataURL string `json:"dataURL"`
}

// liveImageSnapshot is the read-only collector result.
type liveImageSnapshot struct {
	Imgs   []liveImageSlot `json:"imgs"`
	Canvas []liveImageSlot `json:"canvas"`
}

// collectLiveImagesLocked evaluates the read-only image probe against the
// owned tab. Callers must hold c.mu (same discipline as the answer wait that
// precedes it). Any failure returns an empty snapshot: the pure outerHTML
// fixtures still carry the turn.
func (c *Client) collectLiveImagesLocked(ctx context.Context) liveImageSnapshot {
	var snap liveImageSnapshot
	if c.tab == nil {
		return snap
	}
	rctx, cancel := operationContext(c.tab, ctx, 30*time.Second)
	defer cancel()
	_ = chromedp.Run(rctx, chromedp.Evaluate(responseImagesJS, &snap,
		chromedp.EvalAsValue,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	return snap
}

// jsQuote renders s as a JS string literal (JSON encoding is valid JS).
func jsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
