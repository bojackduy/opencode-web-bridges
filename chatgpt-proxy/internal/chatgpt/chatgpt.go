// Package chatgpt drives chatgpt.com in a dedicated Chrome profile via CDP
// so that AI agents can use a ChatGPT Web session (no API key, no cost)
// through a plain ask/answer interface.
//
// The client owns one Chrome instance (launched on demand with its own
// profile directory) and one tab. All operations are serialized through a
// mutex because they share that single tab.
package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/gorilla/websocket"
)

// Defaults.
const (
	DefaultChromePort = 9222
	DefaultTimeout    = 10 * time.Minute
	HomePage          = "https://chatgpt.com/"
)

// Errors returned by Ask/Status. Callers can use errors.Is to give agents
// actionable messages.
var (
	ErrChromeNotFound = errors.New("chrome binary not found")
	ErrChromeLaunch   = errors.New("failed to launch chrome")
	ErrChromeProfile  = errors.New("debugging endpoint cannot prove the configured Chrome profile; start the dedicated Chrome with --enable-automation on the configured port")
	ErrNotLoggedIn    = errors.New("not logged in to chatgpt.com")
	ErrNoComposer     = errors.New("chatgpt composer not found (no #prompt-textarea or ProseMirror contenteditable)")
	ErrNoNewChat      = errors.New("could not find the New chat button")
	ErrNoResponse     = errors.New("chatgpt did not produce a response in time")
	ErrMissingAnswer  = errors.New("ChatGPT generation ended without an extractable new assistant answer; inspect stored thread/rendering and recover the pending turn; no automatic resubmission")
	ErrProjectSetup   = errors.New("project setup failed before turn submission")
	ErrNotSubmitted   = errors.New("turn failed before any submission action")
	// ErrPageChallenge marks a human-verification gate on the page. Like the
	// draft refusals it is pre-submit yet needs a person, so it keeps its own
	// identity instead of looking like a safe resubmission.
	ErrPageChallenge = errors.New("chatgpt page shows a human-verification challenge; resolve it in the logged Chrome profile")
	// ErrBlankProjectPage marks a loaded-but-empty project page (document
	// ready, empty body, no composer, no login control, no challenge). It is
	// a transient render state, so it stays safe-retryable pre-submit.
	ErrBlankProjectPage = errors.New("chatgpt project page loaded blank (document ready, empty body, no composer and no login); REPAIR: reload the project page in the logged Chrome profile")
	// ErrChallengeMidTurn marks a human-verification gate that appeared AFTER
	// the turn was submitted, while the answer was awaited. Unlike
	// ErrPageChallenge (pre-submit, clean journal) this turn may have
	// executed: the journal stays dirty/uncertain and the caller must not
	// resend. Deliberately absent from IsPreSubmit so the pending binding
	// survives for read-only recovery or deliberate abandon.
	ErrChallengeMidTurn = errors.New("turn was submitted but the page now shows human-verification; outcome unknown — do not resend; solve it in the bridge profile window, then recover or abandon")
	// ErrForeignDraft marks a refusal that RETRYING CANNOT FIX: the composer
	// holds text this bridge cannot prove it wrote, or proves it wrote a turn
	// that may already have been dispatched. It deliberately does NOT wrap
	// ErrNotSubmitted so callers cannot classify it as a safe retry.
	ErrForeignDraft = errors.New("composer holds a draft this bridge cannot verify; refusing to overwrite it")
	ErrDraftOwned   = errors.New("composer holds this bridge's own draft from a turn whose submission may have been dispatched; refusing to touch it")
)

// IsPreSubmit reports a failure proven to have happened before any submission
// action, so the session stays clean instead of becoming uncertain. It does NOT
// imply that retrying helps: a foreign draft is pre-submit yet needs a person,
// which is why retryability is decided separately.
func IsPreSubmit(err error) bool {
	return errors.Is(err, ErrNotSubmitted) || errors.Is(err, ErrForeignDraft) || errors.Is(err, ErrDraftOwned) ||
		errors.Is(err, ErrNoComposer) || errors.Is(err, ErrNotLoggedIn) || errors.Is(err, ErrPageChallenge)
}

// IsNavigateError reports a page-navigation or explicit project-setup failure. All
// navigate steps run before anything is typed or submitted, so a caller that
// sees only such an error (and reached no conversation URL) knows provably
// that nothing was sent and may safely retry instead of treating the turn
// as uncertain.
func IsNavigateError(err error) bool {
	return err != nil && (errors.Is(err, ErrProjectSetup) || strings.Contains(err.Error(), "navigate:"))
}

// Status describes the current state of the ChatGPT Web tab.
type Status struct {
	LoggedIn   bool   `json:"logged_in"`
	Composer   bool   `json:"composer"`
	Challenge  bool   `json:"challenge"`
	BodyLen    int    `json:"body_len"`
	NewChat    bool   `json:"new_chat_button"`
	LoginBtn   bool   `json:"login_button"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Profile    string `json:"chrome_profile"`
	ChromePort int    `json:"chrome_port"`
}

// Client owns the Chrome instance and the shared tab.
type Client struct {
	ChromePort   int
	ProfileDir   string
	ChromePath   string // optional override; auto-detected when empty
	Background   bool   // run Chrome window in background (off-screen / minimized)
	DedicatedTab bool   // do not reuse the MCP/user's existing ChatGPT tab
	SetupLocks   bool   // coordinate only setup when using owned parallel targets

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
	pool            *Pool
	ownedTarget     target.ID

	// draftMu guards draftOwn: the record of what this bridge last typed into a
	// composer. Separate from mu on purpose — a later request must be able to
	// reason about the leftover draft without contending for browser lifecycle
	// state, and typing already runs under the caller's turn lock.
	draftMu  sync.Mutex
	draftOwn *draftRecord
}

// New builds a client with defaults for anything zero-valued.
func New(port int, profileDir, chromePath string) *Client {
	if port == 0 {
		port = DefaultChromePort
	}
	if profileDir == "" {
		dir, err := os.UserHomeDir()
		if err == nil && dir != "" {
			profileDir = filepath.Join(dir, ".chatgpt-proxy")
		} else {
			profileDir = ".chatgpt-proxy"
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
}

// tabAlive exercises the full CDP chain (tab session -> allocator websocket)
// with a trivial read. Comparing /json/version UUIDs cannot catch a silently
// dropped websocket to a still-live Chrome — every later Run then fails
// instantly with "context canceled" forever, because the cached tab path
// never re-verifies. So ping the real thing. Short timeout: a wedged
// connection must read as dead, never hang the turn.
func (c *Client) tabAlive() bool {
	if c.tab == nil || c.browser == nil {
		return false
	}
	probe, cancel := context.WithTimeout(c.tab, 5*time.Second)
	defer cancel()
	var ok bool
	if err := chromedp.Run(probe, chromedp.Evaluate(`true`, &ok)); err != nil {
		return false
	}
	return ok
}

// --- browser lifecycle -------------------------------------------------------

// ensureBrowser attaches to a running Chrome on the debug port, or launches
// one with the dedicated profile. Safe to call repeatedly — once launched,
// the same port is reused and never relaunched while this process lives.
func (c *Client) ensureBrowser(ctx context.Context) error {
	if c.pool != nil {
		return c.pool.connectClient(ctx, c)
	}
	if c.browser != nil {
		if c.browserAlive() {
			return nil
		}
		// Stale allocator: Chrome restarted out from under us, which cancels
		// the old connection. Every CDP call on it fails instantly with
		// "context canceled", so tear down and fall through to attach-or-
		// launch instead of failing every turn until process restart.
		log.Printf("[chatgpt] browser connection stale; re-attaching")
		c.teardownLocked()
	}
	// 1. Attach to an already-running DevTools server (e.g. from a previous
	//    run of this proxy) before launching anything.
	port := c.resolvePort()
	wsURL, err := c.attach(ctx, port)
	if err == nil {
		c.actualPort = port
		return c.connect(wsURL)
	}
	if errors.Is(err, ErrChromeProfile) || ctx.Err() != nil {
		return err
	}
	// 2. Configured endpoint absent — launch only on that exact port.
	if !c.launchAttempted {
		if err := c.launch(ctx, port); err != nil {
			return err
		}
		c.launchAttempted = true
		c.launchPort = port
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		wsURL, err := c.attach(ctx, c.launchPort)
		if err == nil {
			c.actualPort = c.launchPort
			return c.connect(wsURL)
		}
		if errors.Is(err, ErrChromeProfile) || ctx.Err() != nil {
			return err
		}
		if time.Now().After(deadline) {
			c.launchAttempted = false // allow a fresh launch on the next call
			return fmt.Errorf("chrome started but debugging port %d never came up: %w", c.launchPort, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// scanStart is the first port to probe for an existing DevTools server.
func (c *Client) scanStart() int {
	if c.ChromePort != 0 && c.ChromePort != DefaultChromePort {
		return c.ChromePort
	}
	return DefaultChromePort
}

// connect wires up the chromedp allocator for a found debugger URL. The
// allocator is rooted at c.life (not the per-call context) so the browser
// connection survives across individual MCP requests.
func (c *Client) connect(wsURL string) error {
	browserCtx, cancel := chromedp.NewRemoteAllocator(c.life, wsURL)
	c.browser = browserCtx
	c.cancel = cancel
	c.wsURL = wsURL
	return nil
}

// browserAlive reports whether the allocator still points at the live
// browser by comparing /json/version's endpoint with the one we connected
// to. A Chrome restart mints a new endpoint UUID, so any difference (or any
// fetch failure) means stale. A short client timeout keeps a wedged listener
// from hanging us: worst case we relaunch unnecessarily, which is safe.
func (c *Client) browserAlive() bool {
	if c.wsURL == "" {
		return false
	}
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	if port == 0 {
		port = DefaultChromePort
	}
	httpClient := http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false
	}
	return v.WebSocketDebuggerURL != "" && v.WebSocketDebuggerURL == c.wsURL
}

// resolvePort keeps browser identity on the configured endpoint. Adjacent
// listeners may belong to another provider/profile and must never be adopted.
func (c *Client) resolvePort() int {
	if c.ChromePort != 0 {
		return c.ChromePort
	}
	return DefaultChromePort
}

// attach returns the WebSocket debugger URL of an already-running Chrome.
// Bounded by a short timeout so a dead or foreign listener can't hang us.
func (c *Client) attach(ctx context.Context, port int) (string, error) {
	versionURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("%w: invalid version response", ErrChromeProfile)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%w: missing browser endpoint", ErrChromeProfile)
	}
	endpoint, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil || endpoint.Scheme != "ws" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost") || endpoint.Port() != strconv.Itoa(port) || !strings.HasPrefix(endpoint.Path, "/devtools/browser/") {
		return "", fmt.Errorf("%w: unexpected browser endpoint", ErrChromeProfile)
	}
	// This browser-level query is read-only: never create/adopt a target until
	// Chrome proves both its profile and port. Without --enable-automation the
	// command is unavailable; fail closed rather than trusting a static label.
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, v.WebSocketDebuggerURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: browser verification unavailable", ErrChromeProfile)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Browser.getBrowserCommandLine"}); err != nil {
		return "", fmt.Errorf("%w: browser verification unavailable", ErrChromeProfile)
	}
	var command struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Arguments []string `json:"arguments"`
		} `json:"result"`
	}
	for command.ID != 1 {
		if err := conn.ReadJSON(&command); err != nil {
			return "", fmt.Errorf("%w: browser verification unavailable", ErrChromeProfile)
		}
	}
	profileOK, portOK := false, false
	wantProfile, err := filepath.Abs(c.ProfileDir)
	if err != nil {
		return "", ErrChromeProfile
	}
	// Older dedicated Chrome may not expose command-line CDP without
	// --enable-automation. In that ONE case require independent OS proof:
	// exact TCP listener PID, exact executable/profile/port args, and the same
	// PID as this profile's singleton owner. No missing evidence is trusted.
	if command.Error.Code == -32000 && command.Error.Message == "Command line not returned because --enable-automation not set." {
		listeners, err := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
		if err != nil {
			return "", fmt.Errorf("%w: listener owner unavailable", ErrChromeProfile)
		}
		var pids []string
		for _, field := range strings.Fields(string(listeners)) {
			if strings.HasPrefix(field, "p") {
				pids = append(pids, field)
			}
		}
		if len(pids) != 1 || !regexp.MustCompile(`^p[1-9][0-9]*$`).MatchString(pids[0]) {
			return "", fmt.Errorf("%w: listener owner ambiguous", ErrChromeProfile)
		}
		pid := pids[0][1:]
		lock, err := os.Readlink(filepath.Join(wantProfile, "SingletonLock"))
		if err != nil || !strings.HasSuffix(lock, "-"+pid) {
			return "", fmt.Errorf("%w: listener is not the profile singleton owner", ErrChromeProfile)
		}
		argv, err := exec.CommandContext(ctx, "ps", "-ww", "-p", pid, "-o", "command=").Output()
		bin := c.ChromePath
		if bin == "" {
			bin = findChrome()
		}
		line := strings.TrimSpace(string(argv))
		profileFlag := "--user-data-dir=" + wantProfile
		portFlag := "--remote-debugging-port=" + strconv.Itoa(port)
		if err != nil || bin == "" || !strings.HasPrefix(line, bin+" ") ||
			strings.Count(line, "--user-data-dir=") != 1 || strings.Count(line, "--remote-debugging-port=") != 1 ||
			!regexp.MustCompile(`(?:^|\s)`+regexp.QuoteMeta(profileFlag)+`(?:\s|$)`).MatchString(line) ||
			!regexp.MustCompile(`(?:^|\s)`+regexp.QuoteMeta(portFlag)+`(?:\s|$)`).MatchString(line) {
			return "", fmt.Errorf("%w: listener executable/profile/port mismatch", ErrChromeProfile)
		}
		command.Result.Arguments = []string{profileFlag, portFlag}
	}
	for _, arg := range command.Result.Arguments {
		if strings.HasPrefix(arg, "--user-data-dir=") {
			actual, err := filepath.Abs(strings.TrimPrefix(arg, "--user-data-dir="))
			profileOK = err == nil && actual == wantProfile
		}
		if strings.HasPrefix(arg, "--remote-debugging-port=") {
			portOK = arg == "--remote-debugging-port="+strconv.Itoa(port)
		}
	}
	if !profileOK || !portOK {
		return "", fmt.Errorf("%w: browser profile or port mismatch", ErrChromeProfile)
	}
	return v.WebSocketDebuggerURL, nil
}

// launch starts Chrome with the dedicated profile and the debug port,
// detached from this process.
func (c *Client) launch(ctx context.Context, port int) error {
	bin := c.ChromePath
	if bin == "" {
		bin = findChrome()
		if bin == "" {
			return ErrChromeNotFound
		}
	}
	if err := os.MkdirAll(c.ProfileDir, 0o755); err != nil {
		return err
	}
	// Never forward launch arguments into a live non-debuggable Chrome using
	// this profile. A singleton whose owner is provably gone is a different
	// thing: Chrome is launched detached, so killing this process strands a
	// lock that would otherwise refuse every relaunch until someone moved it by
	// hand. Chrome needs the same proof we do and reclaims such a lock itself,
	// so nothing is removed here; a live or unprovable owner still fails closed.
	lockPath := filepath.Join(c.ProfileDir, "SingletonLock")
	singleton := false
	for _, name := range []string{"SingletonLock", "SingletonSocket"} {
		if _, err := os.Lstat(filepath.Join(c.ProfileDir, name)); err == nil {
			singleton = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if singleton {
		if !orphanedSingleton(lockPath) {
			return fmt.Errorf("%w: dedicated profile is already locked; inspect its owning Chrome and start its exact configured debugging port; no lock was removed", ErrChromeLaunch)
		}
		log.Printf("[chatgpt] profile singleton owner is gone; relaunching so Chrome can reclaim the stale lock")
	}
	args := []string{
		"--enable-automation",
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + c.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-session-crashed-bubble",
		"--disable-features=Translate",
	}
	if c.Background {
		args = append(args,
			"--window-position=-2400,-2400",
			"--window-size=1280,800",
			"--start-minimized",
		)
	}
	args = append(args, HomePage)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrChromeLaunch, err)
	}
	// Detach: the Chrome process outlives this one.
	go cmd.Wait() //nolint:errcheck
	return nil
}

// singletonOwner reads the "<nodename>-<pid>" owner Chrome records in a
// profile's SingletonLock symlink. Any other shape proves nothing.
func singletonOwner(path string) (host string, pid int, ok bool) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", 0, false
	}
	cut := strings.LastIndex(target, "-")
	if cut <= 0 || cut == len(target)-1 {
		return "", 0, false
	}
	pid, err = strconv.Atoi(target[cut+1:])
	if err != nil || pid <= 0 {
		return "", 0, false
	}
	return target[:cut], pid, true
}

// processAlive reports whether pid is a live process. EPERM means it exists but
// belongs to someone else, which still counts.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// orphanedSingleton reports whether this profile's lock was left behind by a
// Chrome on THIS machine that is no longer running. A foreign nodename, an
// unparsable target, or a live PID is not proof and stays fail-closed.
func orphanedSingleton(lockPath string) bool {
	host, pid, ok := singletonOwner(lockPath)
	if !ok {
		return false
	}
	ourHost, err := os.Hostname()
	if err != nil || host != ourHost {
		return false
	}
	return !processAlive(pid)
}

func findChrome() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// tabCtx returns the shared tab context, creating it on first use.
// Callers must already hold c.mu (the public methods do).
func (c *Client) tabCtx(ctx context.Context) (context.Context, error) {
	if c.tab != nil {
		// The cached tab is the common path, so verify it here — not just
		// in ensureBrowser. A silently dropped websocket to a live Chrome
		// keeps the UUID check green while every Run fails instantly, so
		// probe the real chain and rebuild on failure instead of failing
		// every turn until process restart.
		if c.tabAlive() {
			return c.tab, nil
		}
		log.Printf("[chatgpt] tab connection dead; re-attaching")
		c.teardownLocked()
	}
	if c.SetupLocks && c.pool == nil {
		unlock, err := LockSetup(ctx, c.ProfileDir)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	if err := c.ensureBrowser(ctx); err != nil {
		return nil, err
	}
	if c.SetupLocks && c.pool != nil {
		unlock, err := LockSetup(ctx, c.ProfileDir)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	// A freshly launched browser answers /json/version before its first
	// window exists, and Target.createTarget fails until then ("no browser
	// is open") — so retry instead of failing fast.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		tid, err := c.findOrCreateTab()
		if err == nil {
			tab, done := chromedp.NewContext(c.browser, chromedp.WithTargetID(tid))
			if aerr := chromedp.Run(tab, chromedp.ActionFunc(func(context.Context) error { return nil })); aerr == nil {
				c.tab, c.tabDone = tab, done
				return c.tab, nil
			} else {
				done()
				lastErr = aerr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("alloc: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// findOrCreateTab reuses an existing chatgpt.com page, or opens one through
// the DevTools HTTP API — which reliably creates a window even when none
// exists, unlike Target.createTarget over the websocket (fails with "no
// browser is open" when every window is closed). HTTP is also used for
// listing: chromedp.Targets needs a full tab context, which is what we're
// trying to build here.
func (c *Client) findOrCreateTab() (target.ID, error) {
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var infos []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := httpGetJSON(base+"/json/list", &infos); err != nil {
		return "", err
	}
	// A newly restarted MCP client must not adopt the adapter's newest tab.
	// Target IDs survive navigation; a stale ID after Chrome exits is harmless.
	privateID, err := os.ReadFile(filepath.Join(c.ProfileDir, "openai-tab-id"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, info := range infos {
		if c.DedicatedTab {
			break
		}
		if info.ID != string(privateID) && info.Type == "page" && strings.Contains(info.URL, "chatgpt.com") {
			return target.ID(info.ID), nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := httpPutJSON(base+"/json/new?"+HomePage, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("empty target id from /json/new")
	}
	c.ownedTarget = target.ID(created.ID)
	if c.DedicatedTab && c.pool == nil {
		// Pool workers each own a short-lived target and must not clobber
		// the shared file: it would point the next restart at an already-
		// closed target. Owned clients never adopt by ID anyway.
		if err := os.MkdirAll(c.ProfileDir, 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(c.ProfileDir, "openai-tab-id"), []byte(created.ID), 0600); err != nil {
			return "", err
		}
	}
	return target.ID(created.ID), nil
}

func httpGetJSON(url string, v any) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func httpPutJSON(url string, v any) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// --- public API --------------------------------------------------------------

// Status opens (or attaches to) Chrome, loads chatgpt.com and reports the
// state of the page — mainly whether the user is logged in.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tab, err := c.tabCtx(ctx)
	if err != nil {
		return nil, err
	}
	navCtx, cancel := context.WithTimeout(tab, 45*time.Second)
	defer cancel()
	if err := chromedp.Run(navCtx, chromedp.Navigate(HomePage)); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	// Give the SPA a moment to render.
	time.Sleep(1500 * time.Millisecond)

	return c.probeState(ctx, tab)
}

// LoginState probes the current page without navigating — safe to call
// repeatedly while the user is mid-login in the Chrome window.
func (c *Client) LoginState(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tab, err := c.tabCtx(ctx)
	if err != nil {
		return nil, err
	}
	return c.probeState(ctx, tab)
}

func (c *Client) probeState(ctx context.Context, tab context.Context) (*Status, error) {
	tab, cancel := requestContext(ctx, tab)
	defer cancel()
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	s := &Status{Profile: c.ProfileDir, ChromePort: port}
	probe := `JSON.stringify((() => {
	  const q = (sel) => !!document.querySelector(sel);
	  const composer = () => !!(document.querySelector('#prompt-textarea')
	    || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]')
	    || document.querySelector('.ProseMirror[contenteditable="true"]'));
	  const bodyText = (document.body && typeof document.body.innerText === 'string') ? document.body.innerText : '';
	  return {
	    composer: composer(),
	    sidebar: q('#sidebar'),
	    loginBtn: q('[data-testid="login-button"]'),
	    newChat: q('a[aria-label="New chat"], button[aria-label="New chat"], a[href="/"]'),
	    challenge: !!document.querySelector('iframe[src*="challenges.cloudflare"]') || /verify you are human|verify.challenge|cloudflare.challenge/i.test(bodyText.slice(0, 4000)) || /just a moment|attention required|verify you are human/i.test(document.title || ''),
	    bodyLen: Math.min(bodyText.length, 1048576),
	    url: location.href,
	    title: document.title,
	  };
	})())`
	var raw string
	if err := chromedp.Run(tab, chromedp.Evaluate(probe, &raw)); err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	var p struct {
		Composer  bool   `json:"composer"`
		Sidebar   bool   `json:"sidebar"`
		LoginBtn  bool   `json:"loginBtn"`
		NewChat   bool   `json:"newChat"`
		Challenge bool   `json:"challenge"`
		BodyLen   int    `json:"bodyLen"`
		URL       string `json:"url"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("probe decode: %w", err)
	}
	s.Composer = p.Composer
	s.Challenge = p.Challenge
	s.BodyLen = p.BodyLen
	s.NewChat = p.NewChat
	s.LoginBtn = p.LoginBtn
	s.URL = p.URL
	s.Title = p.Title
	// Logged in = composer or sidebar present and no login button.
	s.LoggedIn = (p.Composer || p.Sidebar) && !p.LoginBtn
	return s, nil
}

// Ask sends a prompt to ChatGPT Web and returns the final assistant answer.
// When newSession is true the conversation is reset first (default behavior —
// deterministic, isolated calls). timeout bounds the whole call.
func (c *Client) Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error) {
	return c.ask(ctx, prompt, newSession, timeout, 5*time.Second, nil)
}

// AskStream is Ask with live progress: onProgress is called with the full
// answer-so-far each time the polled text grows. Use it to stream deltas to
// an SSE client (diff against the previous value to get the new slice).
func (c *Client) AskStream(ctx context.Context, prompt string, newSession bool, timeout time.Duration, onProgress func(string)) (string, error) {
	return c.ask(ctx, prompt, newSession, timeout, 2*time.Second, onProgress)
}

func (c *Client) ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration, pollInterval time.Duration, onProgress func(string)) (string, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, timeoutCancel := context.WithTimeout(ctx, timeout)
	defer timeoutCancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	page := ""
	if newSession {
		page = HomePage
	}
	res, err := c.askLocked(ctx, TurnOptions{Prompt: prompt}, page, timeout, pollInterval, onProgress)
	return res.Answer, err
}

// sameURL reports whether two URLs address the same page, ignoring hash
// and trailing slash.
func sameURL(a, b string) bool {
	trim := func(s string) string {
		if i := strings.Index(s, "#"); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSuffix(s, "/")
	}
	return trim(a) == trim(b)
}

// nonRetryablePreSubmit reports pre-send outcomes that describe a state only a
// person can resolve. Retrying them cannot help and can look like a resubmission,
// so they keep their own identity instead of being folded into ErrNotSubmitted.
func nonRetryablePreSubmit(err error) bool {
	return errors.Is(err, ErrForeignDraft) || errors.Is(err, ErrDraftOwned) ||
		errors.Is(err, ErrNoComposer) || errors.Is(err, ErrNotLoggedIn) ||
		errors.Is(err, ErrPageChallenge)
}

// classifyPreSendFailure is askLocked's defer, as a pure function so the
// classification can be asserted without a browser.
func classifyPreSendFailure(err error, submissionAttempted bool) error {
	if err != nil && !submissionAttempted && !nonRetryablePreSubmit(err) {
		return errors.Join(ErrNotSubmitted, err)
	}
	return err
}

// askLocked owns navigation through URL capture under one browser transaction.
type flowIDKey struct{}

func flowID(ctx context.Context) string {
	id, _ := ctx.Value(flowIDKey{}).(string)
	return id
}

func (c *Client) askLocked(ctx context.Context, opt TurnOptions, page string, timeout, pollInterval time.Duration, onProgress func(string)) (out TurnResult, err error) {
	id := fmt.Sprintf("chatgpt-%x", time.Now().UnixNano())
	ctx = context.WithValue(ctx, flowIDKey{}, id)
	submissionAttempted := false
	started := time.Now()
	log.Printf("[chatgpt-flow] id=%s stage=start prompt_bytes=%d page=%t thinking=%v timeout=%s", id, len(opt.Prompt), page != "", opt.Thinking, timeout)
	defer func() {
		log.Printf("[chatgpt-flow] id=%s stage=finish err=%v submitted=%v answer_bytes=%d reasoning_bytes=%d url=%t duration=%s", id, err, submissionAttempted, len(out.Answer), len(out.Reasoning), out.URL != "", time.Since(started).Round(time.Millisecond))
		// A typed draft refusal is documented as NOT wrapping ErrNotSubmitted:
		// re-adding it here would hand callers a "safe retry" that cannot work.
		err = classifyPreSendFailure(err, submissionAttempted)
		if err != nil && submissionAttempted && out.URL == "" && c.tab != nil {
			// A failed click or cancelled poll can return before the normal
			// URL capture. Preserve an already-minted thread for read-only
			// recovery; never replace it with a project/homepage URL.
			probe, done := context.WithTimeout(c.tab, 2*time.Second)
			defer done()
			var url string
			if chromedp.Run(probe, chromedp.Location(&url)) == nil && ThreadPattern.MatchString(url) {
				out.URL = url
			}
		}
	}()
	tab, err := c.tabCtx(ctx)
	if err != nil {
		return out, err
	}
	tab, cancel := requestContext(ctx, tab)
	defer cancel()

	// 1. Navigate to the target page, unless the tab is already there. A
	// same-URL Navigate is a full reload: wasted seconds plus a fresh React
	// mount on every turn. Consecutive turns on one thread skip it; the
	// readiness check below still verifies the composer before typing.
	if page != "" {
		var current string
		if err := chromedp.Run(tab, chromedp.Evaluate(`location.href`, &current)); err == nil && sameURL(current, page) {
			log.Printf("[chatgpt] already on %s, skipping reload", page)
		} else {
			log.Printf("[chatgpt] navigating to %s", page)
			navCtx, cancel := context.WithTimeout(tab, 120*time.Second)
			defer cancel()
			if err := chromedp.Run(navCtx, chromedp.Navigate(page)); err != nil {
				return out, fmt.Errorf("navigate: %w", err)
			}
		}
	}

	// 2. Wait up to 30s for the SPA to finish loading & hydrating
	log.Printf("[chatgpt] waiting for page ready...")
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return out, c.recoverBlankProjectPage(ctx, tab, err)
	}
	if projectPattern.MatchString(page) {
		var projectComposer bool
		if err := chromedp.Run(tab, chromedp.Evaluate(`(() => {
		  const els = [...document.querySelectorAll('[contenteditable="true"]')];
		  return els.some(e => (e.getAttribute('aria-label') || '').startsWith('New chat in '));
		})()`, &projectComposer)); err != nil {
			return out, err
		}
		if !projectComposer {
			return out, errors.New("Project composer not verified; REPAIR: check saved openai-project-url and Project DOM; no prompt submitted")
		}
	}

	// 2b. Optionally switch to the Thinking model so reasoning is emitted.
	if opt.Thinking {
		if err := c.selectThinking(ctx, tab); err != nil {
			return out, err
		}
	}

	// 3. Type the prompt.
	log.Printf("[chatgpt] typing prompt (%d bytes)...", len(opt.Prompt))
	if err := c.typePrompt(ctx, tab, opt.Prompt); err != nil {
		return out, err
	}

	// 3b. Upload any image/video attachments into the composer file input.
	if len(opt.Attachments) > 0 {
		log.Printf("[chatgpt] attaching %d file(s)...", len(opt.Attachments))
		files, err := c.attachFiles(ctx, tab, opt.Attachments)
		if err != nil {
			return out, err
		}
		defer func() {
			for _, f := range files {
				_ = os.Remove(f)
			}
		}()
	}

	// 4. Submit. No composer verification: the bridge types and sends.
	beforeIDs, err := responseBaseline(tab)
	if err != nil {
		return out, err
	}
	// The shadow reader is opt-in measurement only. It subscribes before the
	// click so the response stream is captured from its first frame, and its
	// result is compared after the turn. It never influences what is returned.
	var shadow *shadowCapture
	if streamCaptureEnabled() {
		var stopShadow func()
		shadow, stopShadow = startShadowCapture(tab, opt.Prompt)
		defer stopShadow()
		// Registered as a closure: the comparison must see the final answer.
		defer func() { reportShadow(shadow, id, out.Answer) }()
	}
	log.Printf("[chatgpt] target=%s submitting prompt...", chromedp.FromContext(tab).Target.TargetID)
	// A click whose outcome is unknown must stay uncertain, so mark the attempt
	// up front. submit still downgrades this to ErrNotSubmitted when the DOM
	// proves the turn never reached the thread.
	submissionAttempted = true
	bubbleID, err := c.submit(ctx, tab, opt.Prompt)
	if err != nil {
		return out, err
	}
	out.BubbleID = bubbleID

	// 5. Wait for the answer.
	log.Printf("[chatgpt] waiting for answer (timeout: %v)...", timeout)
	answer, reasoning, err := c.waitForAnswer(ctx, tab, timeout, pollInterval, onProgress, beforeIDs, shadow, opt.Thinking)
	out.Answer, out.Reasoning = answer, reasoning
	if err == nil {
		// Rasterize substantive answer SVGs to PNG bridge-side: the TUI
		// demotes SVG to plain text while raster image parts preview.
		// Best-effort; a nil result keeps the text fallback. Skipped when
		// the answer carries Mermaid source: the client renders the code
		// itself, so a screenshot would only duplicate the diagram.
		if !hasMermaidFence(answer) {
			out.Images = c.rasterizeAnswerSVG(tab, answer, beforeIDs)
		}
	}
	var url string
	// Capture an uncertain submitted turn's URL even after request cancellation.
	// This bounded, read-only diagnostic never sends or retries a browser action.
	probe, done := context.WithTimeout(c.tab, 2*time.Second)
	defer done()
	if locationErr := chromedp.Run(probe, chromedp.Location(&url)); err == nil {
		err = locationErr
	}
	out.URL = url
	return out, err
}

// requestContext keeps the CDP executor but ties actions to request cancellation.
func requestContext(ctx, tab context.Context) (context.Context, context.CancelFunc) {
	child, cancel := context.WithCancel(tab)
	stop := context.AfterFunc(ctx, cancel)
	return child, func() { stop(); cancel() }
}

// AskThread restores an explicit stored thread in this client's dedicated tab.
// Empty URL starts a new thread. The caller serializes the whole transaction.
func (c *Client) AskThread(ctx context.Context, prompt, threadURL string) (string, string, error) {
	res, err := c.AskThreadTurn(ctx, TurnOptions{Prompt: prompt, URL: threadURL})
	return res.Answer, res.URL, err
}

// AskThreadTurn is AskThread plus image/video attachments and Thinking reasoning.
func (c *Client) AskThreadTurn(ctx context.Context, opt TurnOptions) (out TurnResult, err error) {
	enteredTurn := false
	defer func() {
		if err != nil && !enteredTurn {
			err = errors.Join(ErrNotSubmitted, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	if opt.URL != "" && !ThreadPattern.MatchString(opt.URL) {
		return TurnResult{}, errors.New("invalid ChatGPT thread URL")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tab, err := c.tabCtx(ctx)
	if err != nil {
		return TurnResult{}, err
	}
	page := opt.URL
	projectURL := ""
	if page == "" {
		unlock := func() {}
		if c.SetupLocks {
			var err error
			unlock, err = LockSetup(ctx, c.ProfileDir)
			if err != nil {
				return TurnResult{}, err
			}
			defer unlock()
		}
		switch {
		case opt.ProjectURL != "":
			if !projectPattern.MatchString(opt.ProjectURL) {
				return TurnResult{}, errors.New("invalid ChatGPT project URL")
			}
			page, projectURL = opt.ProjectURL, opt.ProjectURL
		case opt.ProjectName != "":
			p, err := c.ensureProjectNamed(ctx, tab, opt.ProjectName)
			if err != nil {
				return TurnResult{}, errors.Join(ErrProjectSetup, err)
			}
			page, projectURL = p, p
		default:
			page, err = c.ensureProject(ctx, tab)
			if err != nil {
				return TurnResult{}, err
			}
		}
		unlock() // shared project setup ends before any prompt is submitted
	}
	enteredTurn = true
	res, err := c.askLocked(ctx, opt, page, DefaultTimeout, 3*time.Second, nil)
	if err != nil {
		return TurnResult{URL: res.URL, BubbleID: res.BubbleID}, err
	}
	res.ProjectURL = projectURL
	if !ThreadPattern.MatchString(res.URL) {
		return TurnResult{}, errors.New("answer completed but no persistent thread URL; turn cannot be safely continued")
	}
	return res, nil
}

var ThreadPattern = regexp.MustCompile(`^https://chatgpt\.com/(?:g/g-p-[a-zA-Z0-9-]+/)?c/[a-zA-Z0-9-]+$`)

func (c *Client) waitPageReady(ctx context.Context, tab context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := tab.Err(); err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(tab, 3*time.Second)
		var ready bool
		js := `(() => {
		  if (document.readyState !== 'complete') return false;
		  const el = document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
		  if (!el) return false;
		  const rect = el.getBoundingClientRect();
		  return rect.width > 0 && rect.height > 0;
		})()`
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &ready))
		cancel()
		if ready {
			// Grace period for React hydration / event listener attachment
			return pause(tab, 2*time.Second)
		}
		if err := pause(tab, time.Second); err != nil {
			return err
		}
	}
	return ErrNoComposer
}

// pageLoadState is the content-free shape of the page at a failed readiness
// check: one capped length plus flags, never text.
type pageLoadState struct {
	ReadyComplete bool
	BodyTextLen   int
	Composer      bool
	Login         bool
	Challenge     bool
}

type missingComposerCause int

const (
	missingComposerUnknown missingComposerCause = iota
	missingComposerLoading
	missingComposerAuth
	missingComposerChallenge
	missingComposerBlank
)

func (m missingComposerCause) String() string {
	switch m {
	case missingComposerLoading:
		return "loading"
	case missingComposerAuth:
		return "auth"
	case missingComposerChallenge:
		return "challenge"
	case missingComposerBlank:
		return "blank"
	default:
		return "unknown"
	}
}

// classifyMissingComposer decides a composer-less page. Only the blank loaded
// page may see a single reload: nothing was typed yet, so no submit and no
// draft is at risk. Auth, challenge, loading and unknown shapes must never
// trigger hidden navigation.
func classifyMissingComposer(ps pageLoadState) (missingComposerCause, bool) {
	switch {
	case ps.Login:
		return missingComposerAuth, false
	case ps.Challenge:
		return missingComposerChallenge, false
	case !ps.ReadyComplete:
		return missingComposerLoading, false
	case !ps.Composer && ps.BodyTextLen == 0:
		return missingComposerBlank, true
	default:
		return missingComposerUnknown, false
	}
}

// probePageLoadState reads one content-free snapshot of the page: a single
// readonly evaluate returning flags and one capped length. The challenge
// regex runs in-page; only its boolean leaves the browser.
func (c *Client) probePageLoadState(tab context.Context) (pageLoadState, error) {
	ctx, cancel := context.WithTimeout(tab, 10*time.Second)
	defer cancel()
	js := `(() => {
	  const q = (sel) => !!document.querySelector(sel);
	  const body = document.body;
	  const textLen = body && typeof body.innerText === 'string' ? body.innerText.length : 0;
	  return {
	    ready: document.readyState === 'complete',
	    bodyLen: Math.min(textLen, 1048576),
	    composer: !!(document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]')),
	    login: q('[data-testid="login-button"]'),
	    challenge: !!document.querySelector('iframe[src*="challenges.cloudflare"]') || /verify you are human|verify.challenge|cloudflare.challenge/i.test((body && body.innerText || '').slice(0, 4000)) || /just a moment|attention required|verify you are human/i.test(document.title || ''),
	  };
	})()`
	var raw struct {
		Ready     bool `json:"ready"`
		BodyLen   int  `json:"bodyLen"`
		Composer  bool `json:"composer"`
		Login     bool `json:"login"`
		Challenge bool `json:"challenge"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &raw)); err != nil {
		return pageLoadState{}, err
	}
	return pageLoadState{
		ReadyComplete: raw.Ready,
		BodyTextLen:   raw.BodyLen,
		Composer:      raw.Composer,
		Login:         raw.Login,
		Challenge:     raw.Challenge,
	}, nil
}

// blankReloadAllowed reports whether a single pre-submit reload risks no
// draft: only a missing record or an already-fenced (submitted) one.
func blankReloadAllowed(rec *draftRecord) bool {
	return rec == nil || rec.Submitted
}

// recoverBlankProjectPage handles a waitPageReady failure on the turn path
// (askLocked step 2), where nothing has been typed or submitted yet. It
// probes the content-free page shape once and reloads at most once, and only
// for a blank loaded page with no unsubmitted draft. Auth, challenge,
// loading and unknown shapes return classified errors with no navigation; a
// canceled context returns without probing. Nil return means the re-wait
// passed and the caller proceeds with the turn.
func (c *Client) recoverBlankProjectPage(ctx context.Context, tab context.Context, waitErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ps, err := c.probePageLoadState(tab)
	if err != nil {
		log.Printf("[chatgpt] blank-page probe failed: %v", err)
		return waitErr
	}
	cause, recoverable := classifyMissingComposer(ps)
	log.Printf("[chatgpt] missing composer cause=%s ready=%v bodyLen=%d composer=%v login=%v challenge=%v recoverable=%v", cause, ps.ReadyComplete, ps.BodyTextLen, ps.Composer, ps.Login, ps.Challenge, recoverable)
	switch cause {
	case missingComposerAuth:
		return ErrNotLoggedIn
	case missingComposerChallenge:
		return ErrPageChallenge
	}
	if !recoverable {
		return waitErr
	}
	if !blankReloadAllowed(c.draftOwnership()) {
		log.Printf("[chatgpt] blank-page reload refused: unsubmitted draft on record")
		return ErrBlankProjectPage
	}
	reloadCtx, cancel := context.WithTimeout(tab, 30*time.Second)
	defer cancel()
	if err := chromedp.Run(reloadCtx, chromedp.Reload()); err != nil {
		log.Printf("[chatgpt] blank-page reload failed: %v", err)
		return ErrBlankProjectPage
	}
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		log.Printf("[chatgpt] blank-page re-wait failed: %v", err)
		return ErrBlankProjectPage
	}
	log.Printf("[chatgpt] blank-page reload recovered the composer")
	return nil
}

func (c *Client) startNewChat(ctx context.Context, tab context.Context) error {
	clickCtx, cancel := context.WithTimeout(tab, 20*time.Second)
	defer cancel()
	clickJS := `(() => {
	  if (location.pathname === '/' || location.pathname === '') return true;
	  const sels = ['a[aria-label="New chat"]', 'button[aria-label="New chat"]', 'a[href="/"]'];
	  for (const sel of sels) {
	    const el = document.querySelector(sel);
	    if (el) { el.click(); return true; }
	  }
	  const all = [...document.querySelectorAll('a, button')];
	  const el = all.find(e => (e.textContent || '').trim().replace(/\s+/g, ' ') === 'New chat');
	  if (el) { el.click(); return true; }
	  location.href = 'https://chatgpt.com/';
	  return true;
	})()`
	var ok bool
	if err := chromedp.Run(clickCtx, chromedp.Evaluate(clickJS, &ok)); err != nil {
		return fmt.Errorf("new chat: %w", err)
	}
	// Wait for composer to be ready on the fresh session
	return c.waitPageReady(ctx, tab, 20*time.Second)
}

// composerSelector finds the composer in every renderer we have seen: the
// textarea renderer, the project/form renderer, and ProseMirror.
const composerSelector = `document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]')`

// composerURLSpace mirrors the normalization of the pre-submit check: the
// composer inserts a space before a bare URL, and draft ownership has to be
// judged on that same normal form or our own draft looks unfamiliar.
var composerURLSpace = regexp.MustCompile(`([<("]) ?(https?://)`)

func canonicalComposerText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return composerURLSpace.ReplaceAllString(strings.TrimSpace(s), "$1$2")
}

// composerTextKey is the comparison form of composer text: whitespace is
// ignored entirely, because the editor may add, merge or drop line breaks
// between blocks while absorbing an insertion ("instead.\n\nNo" arrives as
// "instead.No"). Word content must still match exactly, so a real edit is
// never mistaken for the bridge's own draft.
func composerTextKey(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// looksLikeBridgeDraft reports whether a composer document carries the envelope
// every bridge prompt is composed with. It is used ONLY to replace a stale
// bridge draft whose in-memory record was lost (adapter restart); that marker
// pair is not something a person types by accident, while the text itself is
// still verified before anything is deleted.
func looksLikeBridgeDraft(text string) bool {
	return strings.Contains(text, "CURRENT REQUEST") &&
		strings.Contains(text, "Conversation messages for this turn")
}

// ---------------------------------------------------------------------------
// Composer draft provenance.
//
// A refusal that compares the leftover draft only with the prompt about to be
// typed can never recognise the bridge's own unsent draft from an EARLIER
// request: a different prompt makes it look foreign forever. This layer records
// what the bridge wrote, in one logical-document form, so a superseded draft
// can be replaced after proving it is still exactly ours and that no submission
// action was ever dispatched for it.
// ---------------------------------------------------------------------------

// composerDoc is one read of the composer's LOGICAL document. `logical` is
// false whenever the editor model could not be read, in which case nothing may
// be concluded about ownership: the visual DOM is not a substitute.
type composerDoc struct {
	Found      bool   `json:"found"`
	Logical    bool   `json:"logical"`
	Text       string `json:"text"`
	Paragraphs int    `json:"paragraphs"`
	// Rendered is a NON-authoritative hint: how much text the visual DOM shows.
	// It is used only to decide whether ANYTHING landed (so the legacy paste
	// fallback never runs over text that already arrived). It must never decide
	// ownership or exactness — the model does that.
	Rendered int    `json:"rendered"`
	URL      string `json:"url"`
}

// composerDocJS is the ONLY reader used for draft classification, post-wipe
// verification, settlement and the pre-submit gate. Reading the ProseMirror
// model everywhere is what keeps those four decisions consistent: the visual
// DOM carries extra hard breaks (measured live: model 7 newlines vs DOM 17 for
// the same document), so mixing the two silently changes the verdict.
func composerDocJS(sel string) string {
	return `(() => {
	  const el = ` + sel + `;
	  if (!el) return { found: false, logical: false, text: '', paragraphs: 0, rendered: 0, url: '' };
	  // Reading must not steal focus or mutate the editor: focus belongs to the
	  // explicit insertion and wipe paths only.
	  const node = el.pmViewDesc && el.pmViewDesc.node;
	  let text = null, paragraphs = 0;
	  if (node && node.type && node.type.name === 'doc' && typeof node.textBetween === 'function') {
	    text = node.textBetween(0, node.content.size, '\n', '\n');
	    if (typeof node.descendants === 'function') {
	      node.descendants(n => { if (n && n.type && n.type.name === 'paragraph') paragraphs++; });
	    }
	  } else if (el.tagName === 'TEXTAREA') {
	    text = el.value;
	  }
	  return { found: true, logical: text !== null, text: text === null ? '' : text, paragraphs: paragraphs, rendered: (el.innerText || el.value || '').trim().length, url: location.href };
	})()`
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

const (
	verdictExact     = "exact"
	verdictTruncated = "truncated" // the document is a strict prefix of the prompt
	verdictExtra     = "extra"     // the prompt is a strict prefix of the document
	verdictDiverged  = "diverged"  // neither is a prefix of the other
)

// composerAlignment relates a settled document to the prompt meant to be typed.
// Every length is UTF-16 code units — the unit the editor reports — so a
// diagnostic can never again compare JS units with Go bytes. It carries no text.
type composerAlignment struct {
	Document int    `json:"document_units"`
	Prompt   int    `json:"prompt_units"`
	Prefix   int    `json:"common_prefix_units"`
	Suffix   int    `json:"common_suffix_units"`
	Verdict  string `json:"verdict"`
}

func alignComposer(document, prompt string) composerAlignment {
	d, p := composerTextKey(document), composerTextKey(prompt)
	a := composerAlignment{Document: utf16Len(d), Prompt: utf16Len(p)}
	dr, pr := []rune(d), []rune(p)
	i := 0
	for i < len(dr) && i < len(pr) && dr[i] == pr[i] {
		i++
	}
	a.Prefix = utf16Len(string(dr[:i]))
	j := 0
	for j < len(dr)-i && j < len(pr)-i && dr[len(dr)-1-j] == pr[len(pr)-1-j] {
		j++
	}
	a.Suffix = utf16Len(string(dr[len(dr)-j:]))
	switch {
	case d == p:
		a.Verdict = verdictExact
	case strings.HasPrefix(p, d):
		a.Verdict = verdictTruncated
	case strings.HasPrefix(d, p):
		a.Verdict = verdictExtra
	default:
		a.Verdict = verdictDiverged
	}
	return a
}

// draftFingerprint is a content fingerprint used ONLY as a diagnostic and as an
// integrity check on a recorded snapshot. Ownership is decided by exact logical
// string equality, never by a hash prefix: a truncated digest would let a
// collision authorize an overwrite. Only this and lengths ever leave the client
// — never the draft text.
func draftFingerprint(s string) string {
	sum := sha256.Sum256([]byte(canonicalComposerText(s)))
	return fmt.Sprintf("%x", sum[:])
}

// draftFingerprintShort is the log-sized form. It is never used for a decision.
func draftFingerprintShort(s string) string {
	return draftFingerprint(s)[:8]
}

// draftRecord is what the bridge actually LEFT in the composer: the exact
// logical document observed after its insertion attempt, where it was typed,
// and whether any Send action was ever dispatched for it. The intended prompt is
// kept only to recognise a same-turn reuse — it is never proof of what landed.
type draftRecord struct {
	Doc       string
	Raw       string
	Prompt    string
	Hash      string
	Units     int
	Newlines  int
	Target    string
	Thread    string
	Submitted bool
	SetAt     time.Time
}

type draftAction int

const (
	draftTypeComposer draftAction = iota // empty composer: type the prompt
	draftReuse                           // same turn, document already exact
	draftReplace                         // verified own snapshot from an earlier attempt
	draftRefuseForeign
	draftRefuseOwned
)

// decideDraft is the whole ownership rule. It never mutates anything and never
// inspects text beyond canonical comparison.
//
// The ONLY thing that can authorize a clear is an exact match against the
// snapshot the bridge observed itself writing. There is deliberately no
// length-only, prefix-only or hash-prefix path: a user who edits a draft —
// including deleting it down to a prefix — no longer matches that snapshot and
// keeps their text.
func decideDraft(doc composerDoc, rec *draftRecord, prompt, tid, thread string) draftAction {
	if !doc.Found {
		return draftRefuseForeign
	}
	if doc.Text == "" || canonicalComposerText(doc.Text) == "" {
		return draftTypeComposer
	}
	// A non-empty document we cannot read from the editor model proves nothing.
	if !doc.Logical {
		return draftRefuseForeign
	}
	if rec == nil || rec.Doc == "" || rec.Hash == "" {
		return draftRefuseForeign
	}
	// A record that does not describe its own snapshot is not evidence.
	if draftFingerprint(rec.Doc) != rec.Hash {
		return draftRefuseForeign
	}
	// Any dispatched Send makes the draft evidence, not scratch space.
	if rec.Submitted {
		return draftRefuseOwned
	}
	// A different tab or thread means the binding no longer holds; keep it.
	if rec.Target != tid || rec.Thread != thread {
		return draftRefuseForeign
	}
	if composerTextKey(doc.Text) != composerTextKey(rec.Doc) {
		return draftRefuseForeign
	}
	if composerTextKey(prompt) == composerTextKey(rec.Doc) {
		return draftReuse
	}
	return draftReplace
}

func (c *Client) draftOwnership() *draftRecord {
	c.draftMu.Lock()
	defer c.draftMu.Unlock()
	if c.draftOwn == nil {
		return nil
	}
	copied := *c.draftOwn
	return &copied
}

func (c *Client) setDraftOwnership(rec *draftRecord) {
	c.draftMu.Lock()
	defer c.draftMu.Unlock()
	if rec == nil {
		c.draftOwn = nil
		return
	}
	copied := *rec
	c.draftOwn = &copied
}

func (c *Client) clearDraftOwnership() {
	c.draftMu.Lock()
	c.draftOwn = nil
	c.draftMu.Unlock()
}

// recordOwnedDraft stores the exact logical document the editor was observed to
// hold after an insertion attempt. That observation — never the prompt that was
// meant to be typed — is what a later request may compare against. An empty
// observation clears the record for this target/thread only, and re-observing
// the same document never drops an existing Send fence.
func (c *Client) recordOwnedDraft(tid, thread, prompt, observed string) {
	doc := canonicalComposerText(observed)
	c.draftMu.Lock()
	defer c.draftMu.Unlock()
	if doc == "" {
		if c.draftOwn != nil && c.draftOwn.Target == tid && c.draftOwn.Thread == thread {
			c.draftOwn = nil
		}
		return
	}
	submitted := false
	if c.draftOwn != nil && c.draftOwn.Doc == doc && c.draftOwn.Target == tid && c.draftOwn.Thread == thread {
		submitted = c.draftOwn.Submitted
	}
	c.draftOwn = &draftRecord{
		Doc:       doc,
		Raw:       observed,
		Prompt:    canonicalComposerText(prompt),
		Hash:      draftFingerprint(doc),
		Units:     utf16Len(doc),
		Newlines:  strings.Count(doc, "\n"),
		Target:    tid,
		Thread:    thread,
		Submitted: submitted,
		SetAt:     time.Now(),
	}
}

// markDraftSubmitted fences a draft permanently once a Send action is
// dispatched, or its outcome is unknown. A fenced draft is never cleared,
// reused or resent: the composer still holding it is not evidence that the
// turn never left.
func (c *Client) markDraftSubmitted(targetID, prompt string) {
	c.draftMu.Lock()
	defer c.draftMu.Unlock()
	if c.draftOwn == nil {
		return
	}
	if targetID != "" && c.draftOwn.Target != "" && c.draftOwn.Target != targetID {
		return
	}
	if prompt != "" && c.draftOwn.Prompt != "" && canonicalComposerText(prompt) != c.draftOwn.Prompt {
		return
	}
	c.draftOwn.Submitted = true
}

// wipeComposer clears the owned composer with a trusted deletion and PROVES the
// logical document is empty before returning. Anything short of proven-empty is
// a refusal: typing into a partly cleared editor is how text gets mixed, and a
// merely visual empty state can still hold model content.
// preSubmitGate is the last thing between a typed prompt and Send.
//
// It reads the composer's LOGICAL document with the same reader the draft
// classifier uses — never innerText, never a visual guess — and requires it to
// be exactly this turn's prompt. Anything else is a state a person must resolve:
// the text is preserved, nothing is submitted, and no blind wipe runs. The ONE
// safe clear is the bridge's own still-unedited residual for this same prompt,
// and even that only after the wipe proves the editor is logically empty.
func (c *Client) preSubmitGate(tab context.Context, prompt string) error {
	sel := composerSelector
	tid := ""
	if tr := chromedp.FromContext(tab); tr != nil && tr.Target != nil {
		tid = string(tr.Target.TargetID)
	}
	var doc composerDoc
	if err := chromedp.Run(tab, chromedp.Evaluate(composerDocJS(sel), &doc)); err != nil {
		return fmt.Errorf("%w: read composer: %v", ErrNotSubmitted, err)
	}
	if !doc.Found {
		return ErrNoComposer
	}
	if doc.Logical && composerTextKey(doc.Text) == composerTextKey(prompt) {
		return nil
	}
	log.Printf("[chatgpt] pre-submit composer mismatch logical=%v target=%s document_units=%d document_hash=%s prompt_units=%d",
		doc.Logical, tid, utf16Len(doc.Text), draftFingerprintShort(doc.Text), utf16Len(prompt))
	rec := c.draftOwnership()
	owned := rec != nil && !rec.Submitted && rec.Target == tid && rec.Thread == doc.URL &&
		rec.Doc != "" && composerTextKey(rec.Prompt) == composerTextKey(prompt) && composerTextKey(doc.Text) == composerTextKey(rec.Doc)
	if !owned {
		// A user edit after typing lands here. Their text wins, and a retry
		// cannot fix it, so it must not be classified as a safe resubmission.
		return fmt.Errorf("%w (target=%s document_units=%d document_hash=%s prompt_units=%d retryable=false); the composer text is not this turn's prompt and this bridge cannot prove it wrote it, so nothing was submitted and it was left untouched",
			ErrForeignDraft, tid, utf16Len(doc.Text), draftFingerprintShort(doc.Text), utf16Len(prompt))
	}
	if err := c.wipeComposer(tab, sel); err != nil {
		return fmt.Errorf("pre-submit wipe: %w", err)
	}
	var after composerDoc
	if err := chromedp.Run(tab, chromedp.Evaluate(composerDocJS(sel), &after)); err != nil {
		return fmt.Errorf("%w: verify composer wipe: %v", ErrNotSubmitted, err)
	}
	if canonicalComposerText(after.Text) != "" {
		return fmt.Errorf("%w: composer wipe could not be verified, so nothing was submitted", ErrNotSubmitted)
	}
	return fmt.Errorf("%w: the composer did not hold this turn's text, so it was cleared and nothing was submitted", ErrNotSubmitted)
}

func (c *Client) wipeComposer(ctx context.Context, sel string) error {
	rec := c.draftOwnership()
	tr := chromedp.FromContext(ctx)
	if rec == nil || rec.Submitted || tr == nil || tr.Target == nil || rec.Target != string(tr.Target.TargetID) || draftFingerprint(rec.Doc) != rec.Hash {
		return ErrForeignDraft
	}
	expected := rec.Raw
	if expected == "" {
		expected = rec.Doc
	}
	return c.clearComposerExact(ctx, sel, expected, rec.Thread)
}

// clearComposerExact deletes the composer's contents only while the SAME
// in-page read still shows the expected document and thread. Text comparison
// ignores whitespace runs: the editor may reshape line breaks while absorbing
// the insertion, and that must not turn the bridge's own draft into a refusal.
func (c *Client) clearComposerExact(ctx context.Context, sel, expected, thread string) error {
	keyJS := `const key = t => (t || '').replace(/\s+/g, '');`
	// Compare and delete in ONE editor transaction. A CDP selection followed by
	// a separate Backspace could erase an edit arriving between those calls.
	js := `(() => {
	  const el = ` + sel + `;
	  ` + keyJS + `
	  const doc = ` + composerDocJS(sel) + `;
	  if (!el || !doc.logical || doc.url !== ` + strconv.Quote(thread) + ` || key(doc.text) !== key(` + strconv.Quote(expected) + `)) return false;
	  const view = el.pmViewDesc && el.pmViewDesc.view;
	  if (view && view.state && view.dispatch) {
	    view.dispatch(view.state.tr.delete(0, view.state.doc.content.size));
	  } else if (el.tagName === 'TEXTAREA') {
	    el.value = '';
	    el.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'deleteContentBackward' }));
	  } else {
	    el.focus();
	    const range = document.createRange();
	    range.selectNodeContents(el);
	    const selection = document.getSelection();
	    if (!selection) return false;
	    selection.removeAllRanges();
	    selection.addRange(range);
	    const current = ` + composerDocJS(sel) + `;
	    if (!current.logical || key(current.text) !== key(` + strconv.Quote(expected) + `) || current.url !== ` + strconv.Quote(thread) + `) return false;
	    // No asynchronous gap between the last comparison and the deletion.
	    // Editors that ignore this command fail the logical-empty check below;
	    // never fall back to a later, unchecked Backspace.
	    document.execCommand('delete', false);
	  }
	  return true;
	})()`
	var cleared bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &cleared)); err != nil {
		return fmt.Errorf("type prompt wipe: %w", err)
	}
	if !cleared {
		return ErrForeignDraft
	}
	var after composerDoc
	if err := chromedp.Run(ctx, chromedp.Evaluate(composerDocJS(sel), &after)); err != nil {
		return fmt.Errorf("type prompt wipe: %w", err)
	}
	if !after.Found {
		return ErrNoComposer
	}
	if canonicalComposerText(after.Text) != "" {
		return fmt.Errorf("%w: wipe did not produce an empty document (document_units=%d document_paragraphs=%d document_hash=%s); nothing was typed, the existing text was left as found",
			ErrForeignDraft, utf16Len(after.Text), after.Paragraphs, draftFingerprint(after.Text))
	}
	return nil
}

// insertMode picks how prompt text reaches the composer. Text holding a newline
// must not go through the single-line native insert: measured on the Project
// composer, one native insert lands only the text up to the FIRST newline and
// silently drops the rest, which is what poisoned bridge threads.
type insertMode int

const (
	insertNative   insertMode = iota // trusted single insert, no newline
	insertLinewise                   // trusted insert per line, trusted line breaks
	insertLegacy                     // oversized: existing execCommand/paste path
)

// composerInlineLimit is the largest prompt the trusted input path handles;
// larger catalog and history pastes keep the bounded legacy path.
const composerInlineLimit = 4096

func insertModeFor(text string) insertMode {
	if len(text) > composerInlineLimit {
		return insertLegacy
	}
	if strings.Contains(text, "\n") {
		return insertLinewise
	}
	return insertNative
}

// composerKeyActions is one trusted key press as keydown plus keyup. Trusted
// key events are what the editor honours; the deprecated command pair does not
// clear its composer. A key that generates text carries it, exactly as a real
// keyboard event does.
func composerKeyActions(typ input.KeyType, key, code, text string, vk int64, mod input.Modifier) []chromedp.Action {
	press := func(t input.KeyType) *input.DispatchKeyEventParams {
		return input.DispatchKeyEvent(t).WithKey(key).WithCode(code).WithText(text).
			WithWindowsVirtualKeyCode(vk).WithNativeVirtualKeyCode(vk).WithModifiers(mod)
	}
	return []chromedp.Action{press(typ), press(input.KeyUp)}
}

// composerInsertActions types text with the mechanism its content requires:
// one native insert for a single line, otherwise trusted line breaks between
// trusted per-line inserts so every line lands.
func composerInsertActions(text string) []chromedp.Action {
	if insertModeFor(text) != insertLinewise {
		return []chromedp.Action{input.InsertText(text)}
	}
	lines := strings.Split(text, "\n")
	actions := make([]chromedp.Action, 0, 2*len(lines))
	for i, line := range lines {
		if i > 0 {
			actions = append(actions, composerKeyActions(input.KeyDown, "Enter", "Enter", "\r", 13, input.ModifierShift)...)
		}
		if line != "" {
			actions = append(actions, input.InsertText(line))
		}
	}
	return actions
}

func (c *Client) typePrompt(ctx context.Context, tab context.Context, prompt string) error {
	id := flowID(ctx)
	log.Printf("[chatgpt-flow] id=%s stage=composer_wait start prompt_bytes=%d", id, len(prompt))
	tab, cancelRequest := requestContext(ctx, tab)
	defer cancelRequest()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Wait for the composer to render — after a Navigate the SPA can take a
	// moment, and failing immediately breaks follow-ups (new_session=false).
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := tab.Err(); err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(tab, 5*time.Second)
		var ready bool
		err := chromedp.Run(checkCtx, chromedp.Evaluate(
			`!!(document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]'))`, &ready))
		cancel()
		if err == nil && ready {
			break
		}
		if time.Now().After(deadline) {
			return ErrNoComposer
		}
		if err := pause(tab, 500*time.Millisecond); err != nil {
			return err
		}
	}

	// Fire and forget: insert the prompt and return. The composer is never
	// compared against the prompt, never refused, and never cleared, so a
	// stale draft may mix with this turn's text and a partial insertion may
	// be sent. That tradeoff is deliberate.
	typeCtx, cancel := context.WithTimeout(tab, 30*time.Second)
	defer cancel()
	compSel := composerSelector
	focusJS := `(() => { const el = ` + compSel + `; if (!el) return false; el.focus(); const sel = document.getSelection(); if (sel) { const range = document.createRange(); range.selectNodeContents(el); range.collapse(false); sel.removeAllRanges(); sel.addRange(range); } return true; })()`
	var focused bool
	if err := chromedp.Run(typeCtx, chromedp.Evaluate(focusJS, &focused)); err != nil {
		return fmt.Errorf("type prompt focus: %w", err)
	}
	if !focused {
		log.Printf("[chatgpt-flow] id=%s stage=composer_focus result=missing", id)
		return ErrNoComposer
	}
	log.Printf("[chatgpt-flow] id=%s stage=composer_focus result=ok", id)
	// Some renderers ignore trusted input, and oversized prompts can only ride
	// the legacy command/paste path. Emptiness before the write tells the two
	// cases apart without comparing against the prompt afterwards.
	hasText := func() bool {
		var doc composerDoc
		if err := chromedp.Run(typeCtx, chromedp.Evaluate(composerDocJS(compSel), &doc)); err != nil {
			return false
		}
		return doc.Found && (doc.Rendered > 0 || canonicalComposerText(doc.Text) != "")
	}
	emptyBefore := !hasText()
	var insErr error
	// Native input needs the target executor installed by chromedp.Run.
	needLegacy := insertModeFor(prompt) == insertLegacy
	if !needLegacy {
		insCtx, insCancel := context.WithTimeout(typeCtx, 20*time.Second)
		insErr = chromedp.Run(insCtx, composerInsertActions(prompt)...)
		insCancel()
		// Give the editor a moment to absorb trusted input before falling back.
		needLegacy = emptyBefore && !func() bool {
			until := time.Now().Add(1500 * time.Millisecond)
			for {
				if hasText() {
					return true
				}
				if time.Now().After(until) {
					return false
				}
				if err := pause(tab, 100*time.Millisecond); err != nil {
					return false
				}
			}
		}()
	}
	if prompt != "" && needLegacy {
		fbCtx, fbCancel := context.WithTimeout(tab, 90*time.Second)
		defer fbCancel()
		legacyJS := fmt.Sprintf(`(() => {
		  const t = %s;
		  const el = %s;
		  if (!el) return 'missing';
		  try { document.execCommand('insertText', false, t); } catch (e) {}
		  if ((el.innerText || el.value || '').trim().length > 0) return 'ok';
		  try {
		    const dt = new DataTransfer();
		    dt.setData('text/plain', t);
		    el.dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: dt }));
		    if ((el.innerText || el.value || '').trim().length > 0) return 'ok';
		  } catch (e) {}
		  return 'empty';
		})()`, strconv.Quote(prompt), compSel)
		var legacy string
		if err := chromedp.Run(fbCtx, chromedp.Evaluate(legacyJS, &legacy)); err != nil {
			return fmt.Errorf("type prompt: %w", err)
		}
		if legacy != "ok" {
			return ErrNoComposer
		}
	}
	log.Printf("[chatgpt] typed prompt bytes=%d inserr=%v", len(prompt), insErr)
	log.Printf("[chatgpt-flow] id=%s stage=composer_insert result=returned native_error=%v legacy=%v", id, insErr, needLegacy)
	return nil
}

// sendButtonExpr resolves the Send control across the renderers we have seen.
// The readiness read and the click share this one lookup so they can never
// disagree about which control is being clicked.
const sendButtonExpr = `(document.querySelector('[data-testid="send-button"]')
	    || document.querySelector('form[data-chatgpt-composer] button[aria-label^="Send"]')
	    || document.querySelector('button[aria-label="Send message"]')
	    || document.querySelector('button[aria-label="Send prompt"]')
	    || document.querySelector('button[aria-label="Send"]'))`

func (c *Client) submit(ctx context.Context, tab context.Context, prompt string) (string, error) {
	tab, cancelRequest := requestContext(ctx, tab)
	defer cancelRequest()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	readyJS := fmt.Sprintf("(() => { const btn = %s; return !!btn && !btn.disabled; })()", sendButtonExpr)
	sendJS := fmt.Sprintf(`(() => {
	  const btn = %s;
	  if (btn && !btn.disabled) {
	    btn.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, cancelable: true }));
	    btn.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, cancelable: true }));
	    btn.dispatchEvent(new MouseEvent("mouseup", { bubbles: true, cancelable: true }));
	    btn.click();
	    return true;
	  }
	  return false;
	})()`, sendButtonExpr)
	id := flowID(ctx)
	log.Printf("[chatgpt-flow] id=%s stage=send_wait start prompt_bytes=%d", id, len(prompt))
	started := time.Now()
	// Phase 1 only READS whether Send became enabled. A large paste is absorbed
	// asynchronously and can block the renderer's main thread for seconds, so
	// this gets its own deadline and a per-read timeout wider than a click
	// needs. Nothing is dispatched here, so failing this phase is provably
	// pre-send and must never mark the turn uncertain.
	readyDeadline := time.Now().Add(30 * time.Second)
	var lastRead error
	for {
		if err := tab.Err(); err != nil {
			return "", err
		}
		probe, cancel := context.WithTimeout(tab, 10*time.Second)
		var enabled bool
		err := chromedp.Run(probe, chromedp.Evaluate(readyJS, &enabled))
		cancel()
		if err == nil && enabled {
			log.Printf("[chatgpt-flow] id=%s stage=send_ready enabled=true waited=%s", id, time.Since(started).Round(time.Millisecond))
			break
		}
		lastRead = err
		if time.Now().After(readyDeadline) {
			log.Printf("[chatgpt-flow] id=%s stage=send_ready enabled=false waited=%s last_err=%v", id, time.Since(started).Round(time.Millisecond), lastRead)
			return "", fmt.Errorf("%w: Send control never became enabled after %s with %d bytes inserted; nothing was submitted",
				ErrNotSubmitted, time.Since(started).Round(time.Millisecond), len(prompt))
		}
		if err := pause(tab, 500*time.Millisecond); err != nil {
			return "", err
		}
	}
	// Phase 2: dispatch exactly once. Only a failure or an unprovable reply
	// here can leave the turn uncertain.
	clickCtx, cancel := context.WithTimeout(tab, 15*time.Second)
	var clicked bool
	err := chromedp.Run(clickCtx, chromedp.Evaluate(sendJS, &clicked))
	cancel()
	// An interrupted evaluation may have dispatched before its reply was lost.
	if err != nil || clicked {
		// A dispatched (or unprovable) Send makes the draft evidence of an
		// uncertain turn: it must never be cleared, reused or resent just
		// because the composer still holds it.
		c.markDraftSubmitted(string(chromedp.FromContext(tab).Target.TargetID), prompt)
	}
	log.Printf("[chatgpt-flow] id=%s stage=send_click clicked=%v err=%v", id, clicked, err)
	if err != nil {
		return "", fmt.Errorf("send action outcome unknown: %w", err)
	}
	if !clicked {
		// The control went disabled between the read and the click. Nothing was
		// dispatched, so this is still a proven pre-send outcome.
		return "", fmt.Errorf("%w: Send control disabled again before the click; nothing was submitted", ErrNotSubmitted)
	}
	// Phase 3: observe only. A click whose turn never appears stays uncertain:
	// the request may be in flight while React has not cleared the draft yet.
	observeDeadline := time.Now().Add(10 * time.Second)
	retries := 0
	for time.Now().Before(observeDeadline) {
		retries++
		if err := pause(tab, 700*time.Millisecond); err != nil {
			return "", fmt.Errorf("send action outcome unknown: %w", err)
		}
		if err := tab.Err(); err != nil {
			return "", fmt.Errorf("send action outcome unknown: %w", err)
		}
		if sent, err := c.turnSubmitted(tab); err == nil && sent {
			// The turn reached the thread: its draft is no longer scratch
			// space and there is nothing left to reconcile.
			c.clearDraftOwnership()
			log.Printf("[chatgpt-flow] id=%s stage=send_observed result=submitted retries=%d", id, retries)
			// Journal linkage: record which bubble this submitted turn
			// became, so read-back verifies against it instead of assuming
			// pending == last bubble. Best-effort: a capture failure
			// leaves "" and read-back falls back to DOM correlation.
			bubbleID, berr := lastUserBubbleID(tab)
			if berr != nil {
				log.Printf("[chatgpt-flow] id=%s stage=bubble_capture failed: %v", id, berr)
				return "", nil
			}
			return bubbleID, nil
		}
	}
	proof, perr := c.turnUnsubmitted(tab, prompt)
	proof.ActionAttempted = true
	log.Printf("[chatgpt] send did not submit after %d observations: readable=%v compLen=%d wantLen=%d composerExact=%v userBubble=%v generating=%v sendDisabled=%v probe=%v",
		retries, proof.Readable, proof.CompLen, proof.WantLen, proof.ComposerExact, proof.UserBubble, proof.Generating, proof.SendDisabled, perr)
	if provablyUnsubmitted(proof) {
		// The composer still holds the exact text and no turn exists: nothing
		// reached the thread, so this is a pre-send refusal and must not be
		// recorded as an uncertain turn. A paste this large disables Send
		// outright, which used to wedge the chat as "outcome unproven".
		return "", fmt.Errorf("%w: the composer kept the exact text and its Send control stayed disabled; nothing was submitted", ErrNotSubmitted)
	}
	return "", errors.New("send did not submit the turn; REPAIR: inspect composer/send DOM — outcome unproven")
}

// provablyUnsubmitted is the strongest pre-send proof the DOM can give: the
// exact prompt is still in the composer and no user turn appeared, so the
// submission never reached the thread.
func provablyUnsubmitted(p sendProof) bool {
	return p.Readable && p.ComposerExact && !p.UserBubble && !p.Generating && p.SendDisabled
}

// sendProof is the read-only DOM evidence used to decide whether a Send that
// did not start a turn provably never reached the thread. Sizes and flags only:
// no prompt text reaches this struct or its logs.
type sendProof struct {
	ActionAttempted bool `json:"-"`             // any dispatched or uncertain Send remains uncertain
	Readable        bool `json:"readable"`      // composer text could be read at all
	ComposerExact   bool `json:"composerExact"` // composer still holds the expected prompt
	UserBubble      bool `json:"userBubble"`    // a user bubble already holds this prompt
	Generating      bool `json:"generating"`    // a generation indicator is present
	SendDisabled    bool `json:"sendDisabled"`  // the Send control is missing or disabled
	CompLen         int  `json:"compLen"`
	WantLen         int  `json:"wantLen"`
}

// provenUnsubmitted reports whether this evidence PROVES the turn never
// reached the thread, so the caller may retry instead of going uncertain.
// Every condition must hold: no Send was dispatched, the composer was readable
// and still holds the exact prompt, no user bubble carries it, and nothing is generating. Any
// missing condition stays uncertain — never resubmit on an incomplete read.
func (p sendProof) provenUnsubmitted() bool {
	return !p.ActionAttempted && p.Readable && p.ComposerExact && !p.UserBubble && !p.Generating
}

// turnUnsubmitted gathers sendProof from the live DOM. Read-only: it never
// clicks, types, or submits, so it is safe on an already-submitted turn.
func (c *Client) turnUnsubmitted(tab context.Context, prompt string) (sendProof, error) {
	probe, cancel := context.WithTimeout(tab, 3*time.Second)
	defer cancel()
	var p sendProof
	err := chromedp.Run(probe, chromedp.Evaluate(fmt.Sprintf(`(() => {
	  const e = document.querySelector('#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
	  const norm = s => (s || '').replace(/\r\n?/g, '\n').trim();
	  // Only the observed URL defusing space is equivalent, in either direction.
	  const canonical = s => norm(s).replace(/([<(\"]) ?(https?:\/\/)/g, '$1$2');
	  const want = canonical(%s);
	  const node = e?.pmViewDesc?.node;
	  const text = node?.type?.name === 'doc' && typeof node.textBetween === 'function'
	    ? node.textBetween(0, node.content.size, '\n', '\n')
	    : e?.tagName === 'TEXTAREA' ? e.value : null;
	  if (!e || text === null) return {readable:false};
	  const comp = canonical(text);
	  const send = document.querySelector('[data-testid="send-button"]');
	  const stop = !!document.querySelector('[data-testid="stop-button"]') || [...document.querySelectorAll('button')].some(b => /stop/i.test(b.getAttribute('aria-label') || ''));
	  const bubbles = [...document.querySelectorAll('[data-message-author-role="user"]')];
	  const last = bubbles[bubbles.length - 1];
	  const bubble = last ? canonical(last.innerText || '') : '';
	  return {
	    readable: true,
	    composerExact: comp === want,
	    userBubble: bubble !== '' && bubble === want,
	    generating: stop,
	    sendDisabled: !send || send.disabled === true,
	    compLen: comp.length,
	    wantLen: want.length,
	  };
	})()`, strconv.Quote(prompt)), &p))
	return p, err
}

// turnSubmitted reports that the composer was cleared or generation started.
func (c *Client) turnSubmitted(tab context.Context) (bool, error) {
	probe, cancel := context.WithTimeout(tab, 3*time.Second)
	defer cancel()
	var sent bool
	err := chromedp.Run(probe, chromedp.Evaluate(`(() => {
	  const el = document.querySelector('#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
	  const empty = !el || (el.innerText || el.value || '').trim().length === 0;
	  const stop = !!document.querySelector('[data-testid="stop-button"]') || [...document.querySelectorAll('button')].some(b => /stop/i.test(b.getAttribute('aria-label') || ''));
	  return empty || stop;
	})()`, &sent))
	return sent, err
}

// waitForAnswer polls the page until the assistant reply is stable (or the
// stop button disappears), handling the "Continue generating" button.
// onProgress (may be nil) receives the full answer-so-far whenever it grows.
func (c *Client) waitForAnswer(ctx context.Context, tab context.Context, timeout, pollInterval time.Duration, onProgress func(string), before []string, shadow *shadowCapture, thinking bool) (string, string, error) {
	id := flowID(ctx)
	log.Printf("[chatgpt-flow] id=%s stage=stream_wait start poll_interval=%s", id, pollInterval)
	deadline := time.Now().Add(timeout)
	lastText := ""
	stablePolls := 0
	continues := 0
	// emptyPolls counts consecutive polls with no answer text and no active
	// generation. A challenge interstitial reads exactly like that (len=0,
	// no stop button), so every ~30s of nothing gets one read-only page
	// probe: naming the verification early beats a 10-minute timeout that
	// says nothing about what happened.
	emptyPolls := 0

	empty := emptyAnswerWatch{Started: time.Now()}

	for time.Now().Before(deadline) {
		st, err := responseProbe(tab, before)
		if err != nil {
			return "", "", fmt.Errorf("poll: %w", err)
		}
		log.Printf("[chatgpt] target=%s poll: stop=%v, cont=%v, err=%v, len=%d reasoning=%d", chromedp.FromContext(tab).Target.TargetID, st.StopBtn, st.ContinueBtn, st.ErrorText, len(st.Last), len(st.Reasoning))
		log.Printf("[chatgpt-flow] id=%s stage=stream_poll stop=%v continue=%v page_error=%v answer_bytes=%d reasoning_bytes=%d complete=%v", id, st.StopBtn, st.ContinueBtn, st.ErrorText, len(st.Last), len(st.Reasoning), st.Complete)

		// A completed stream is proof the reply exists even when the DOM has
		// not settled. Only the identity comes from the stream; the text is
		// read back from the authenticated mapping under the same schema and
		// ancestry checks. Reasoning is only available from the page, so a
		// thinking turn keeps the ordinary path.
		// A capture that reached an explicit completion holds the server's own
		// final message with its identity already asserted, so it is the
		// authority for this turn. The page is not consulted: settling the DOM
		// is what produces "a previous reply could not be confirmed". Reasoning
		// exists only in the page, so a thinking turn keeps the ordinary path.
		if shadow != nil && !thinking {
			if text, ok := shadow.answer(); ok {
				log.Printf("[chatgpt-flow] id=%s stage=stream_confirmed answer_bytes=%d", id, len(text))
				return text, "", nil
			}
		}

		if st.ErrorText {
			return "", "", errors.New("ChatGPT reported a page error; inspect the stored thread before recovery; no automatic resubmission")
		}
		if empty.expired(st, time.Now()) {
			return "", "", ErrMissingAnswer
		}
		if st.SourceVerified && st.Fresh && st.Complete && st.Last != "" && !st.StopBtn && !st.ContinueBtn {
			// Finished/end_turn source and ancestry were verified in one
			// readback. Repeating the same authenticated fetch twice adds
			// seconds without providing another completion guarantee.
			log.Printf("[chatgpt] target=%s final_source_verified bytes=%d", chromedp.FromContext(tab).Target.TargetID, len(st.Last))
			return strings.TrimSpace(st.Last), strings.TrimSpace(st.Reasoning), nil
		}
		if st.StopBtn {
			stablePolls = 0
			if onProgress != nil && st.Last != "" && st.Last != lastText {
				log.Printf("[chatgpt-flow] id=%s stage=stream_delta answer_bytes=%d", id, len(st.Last))
				onProgress(st.Last)
				lastText = st.Last
			}
			if err := pause(tab, pollInterval); err != nil {
				return "", "", err
			}
			continue
		}
		if st.ContinueBtn && st.Last != "" && st.Last == lastText {
			if continues >= 3 {
				return "", "", errors.New("ChatGPT requires more than three Continue generating clicks")
			}
			continues++
			clickCtx, cancel := context.WithTimeout(tab, 5*time.Second)
			_ = chromedp.Run(clickCtx, chromedp.Evaluate(
				`(() => { const b = document.querySelector('[data-testid="continue-generate-button"]')
				  || [...document.querySelectorAll('button')].find(x => /continue generating/i.test((x.textContent||'').trim()));
				  if (b) { b.click(); return true; } return false; })()`, nil))
			cancel()
			if err := pause(tab, 2*time.Second); err != nil {
				return "", "", err
			}
			continue
		}
		if st.Last == lastText && st.Last != "" {
			stablePolls++
			if stablePolls >= 2 {
				return strings.TrimSpace(st.Last), strings.TrimSpace(st.Reasoning), nil
			}
		} else {
			if onProgress != nil && st.Last != "" {
				onProgress(st.Last)
			}
			lastText = st.Last
			stablePolls = 0
		}
		if st.Last == "" && !st.StopBtn && !st.ContinueBtn {
			emptyPolls++
			if emptyPolls%10 == 0 {
				if ps, perr := c.probePageLoadState(tab); perr == nil && ps.Challenge {
					log.Printf("[chatgpt-flow] id=%s stage=challenge_mid_turn bodyLen=%d emptyPolls=%d", id, ps.BodyTextLen, emptyPolls)
					return "", "", ErrChallengeMidTurn
				}
			}
		} else if st.Last != "" || st.StopBtn {
			emptyPolls = 0
		}
		if err := pause(tab, pollInterval); err != nil {
			return "", "", err
		}
	}
	return "", "", ErrNoResponse
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
