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
	return &Client{ChromePort: port, ProfileDir: profile, ChromePath: chrome}
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

// LoginState attaches (read-only) and reports the signed-in state.
func (c *Client) LoginState(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowser(ctx); err != nil {
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
	if err := c.ensureBrowser(ctx); err != nil {
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

// Ask submits a prompt and returns the answer text.
func (c *Client) Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureBrowser(ctx); err != nil {
		return "", err
	}
	rctx, cancel := operationContext(c.tab, ctx, timeout)
	defer cancel()

	if newSession {
		for _, sel := range newChatSelectors {
			var found bool
			if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
				_ = chromedp.Run(rctx, chromedp.Click(sel, chromedp.ByQuery))
				time.Sleep(1 * time.Second)
				break
			}
		}
	}

	typed := false
	for _, sel := range composerSelectors {
		var found bool
		if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
			if err := chromedp.Run(rctx, chromedp.SendKeys(sel, prompt, chromedp.ByQuery)); err == nil {
				typed = true
				break
			}
		}
	}
	if !typed {
		return "", ErrNoComposer
	}

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
		return "", ErrNotSubmitted
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-rctx.Done():
			return "", rctx.Err()
		default:
		}
		var answer string
		for _, sel := range answerSelectors {
			var found bool
			if err := chromedp.Run(rctx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &found)); err == nil && found {
				if err := chromedp.Run(rctx, chromedp.Text(sel, &answer, chromedp.ByQuery)); err == nil && strings.TrimSpace(answer) != "" {
					return strings.TrimSpace(answer), nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("meta: no answer within timeout")
}

// ReadThreadTurn reads a specific turn (read-only, no submit).
func (c *Client) ReadThreadTurn(ctx context.Context, url, prompt string) (TurnResult, error) {
	return TurnResult{}, fmt.Errorf("meta: ReadThreadTurn not implemented")
}

// ReadLatestThreadTurn reads the latest turn (read-only, no submit).
func (c *Client) ReadLatestThreadTurn(ctx context.Context, url string) (string, TurnResult, error) {
	return "", TurnResult{}, fmt.Errorf("meta: ReadLatestThreadTurn not implemented")
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

func IsPreSubmit(err error) bool { return false }
func IsNavigateError(err error) bool { return false }

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
		`textarea`,
		`div[contenteditable="true"]`,
		`div[role="textbox"]`,
		`input[type="text"]`,
	}
	sendSelectors = []string{
		`button[aria-label*="Send"]`,
		`button[aria-label*="Submit"]`,
		`button[type="submit"]`,
	}
	newChatSelectors = []string{
		`button[aria-label*="New chat"]`,
		`a[aria-label*="New chat"]`,
	}
	answerSelectors = []string{
		`div[role="article"]`,
		`div[class*="response"]`,
		`div[class*="answer"]`,
		`div[class*="message"]`,
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
