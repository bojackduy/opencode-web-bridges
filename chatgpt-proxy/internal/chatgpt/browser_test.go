package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/gorilla/websocket"
)

func versionServer(t *testing.T, uuid string) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Browser":"Test","webSocketDebuggerUrl":%q}`, uuid)
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	return srv, port
}

func TestBrowserAlive(t *testing.T) {
	t.Run("locked dedicated profile never launches or removes singleton", func(t *testing.T) {
		profile := t.TempDir()
		lock := filepath.Join(profile, "SingletonLock")
		if err := os.Symlink("owner-host-123", lock); err != nil {
			t.Fatal(err)
		}
		c := New(9222, profile, filepath.Join(t.TempDir(), "no-chrome"))
		if err := c.launch(context.Background(), 9222); !errors.Is(err, ErrChromeLaunch) || !strings.Contains(err.Error(), "already locked") {
			t.Fatalf("got %v", err)
		}
		if target, err := os.Readlink(lock); err != nil || target != "owner-host-123" {
			t.Fatal("singleton modified")
		}
	})
	t.Run("orphaned singleton from a dead local Chrome is reclaimed, a live one still refuses", func(t *testing.T) {
		host, err := os.Hostname()
		if err != nil {
			t.Skip("hostname unavailable")
		}
		// Dead local owner. Chrome is launched detached, so killing the proxy
		// strands exactly this lock on every restart; it must not brick launch.
		dead := t.TempDir()
		pid := 1 << 30
		for pid > 1 && processAlive(pid) {
			pid--
		}
		target := fmt.Sprintf("%s-%d", host, pid)
		lock := filepath.Join(dead, "SingletonLock")
		if err := os.Symlink(target, lock); err != nil {
			t.Fatal(err)
		}
		c := New(9222, dead, "/usr/bin/true")
		if err := c.launch(context.Background(), 9222); err != nil {
			t.Fatalf("stale lock blocked relaunch: %v", err)
		}
		if got, err := os.Readlink(lock); err != nil || got != target {
			t.Fatal("stale lock removed or rewritten")
		}
		// Live local owner: the case the refusal exists for. Pointing a second
		// Chrome at a profile a running Chrome owns must stay impossible.
		live := t.TempDir()
		if err := os.Symlink(fmt.Sprintf("%s-%d", host, os.Getpid()), filepath.Join(live, "SingletonLock")); err != nil {
			t.Fatal(err)
		}
		if err := New(9222, live, "/usr/bin/true").launch(context.Background(), 9222); !errors.Is(err, ErrChromeLaunch) || !strings.Contains(err.Error(), "already locked") {
			t.Fatalf("live owner accepted: %v", err)
		}
		// A foreign nodename's PID says nothing about this machine.
		foreign := t.TempDir()
		if err := os.Symlink("other-host-1", filepath.Join(foreign, "SingletonLock")); err != nil {
			t.Fatal(err)
		}
		if err := New(9222, foreign, "/usr/bin/true").launch(context.Background(), 9222); !errors.Is(err, ErrChromeLaunch) {
			t.Fatalf("foreign lock accepted: %v", err)
		}
	})
	t.Run("absent configured port never attaches adjacent provider", func(t *testing.T) {
		var requests atomic.Int32
		var adjacent, configured net.Listener
		for i := 0; i < 100; i++ {
			var err error
			adjacent, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := adjacent.Addr().(*net.TCPAddr).Port
			configured, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port-1))
			if err == nil {
				break
			}
			adjacent.Close()
		}
		if configured == nil {
			t.Fatal("could not reserve adjacent ports")
		}
		port := configured.Addr().(*net.TCPAddr).Port
		configured.Close()
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://foreign"}`)
		}))
		srv.Listener.Close()
		srv.Listener = adjacent
		srv.Start()
		defer srv.Close()
		c := New(port, t.TempDir(), filepath.Join(t.TempDir(), "no-chrome"))
		if c.resolvePort() != port {
			t.Fatal("port changed")
		}
		if err := c.ensureBrowser(context.Background()); !errors.Is(err, ErrChromeLaunch) {
			t.Fatalf("got %v", err)
		}
		if requests.Load() != 0 || c.actualPort != 0 || c.browser != nil {
			t.Fatal("adopted adjacent browser")
		}
		if New(0, t.TempDir(), "").resolvePort() != DefaultChromePort {
			t.Fatal("default port changed")
		}
	})
	t.Run("same port requires browser command line profile proof", func(t *testing.T) {
		for _, mode := range []string{"correct", "wrong-profile", "wrong-port", "unavailable", "os-owner", "os-wrong-profile", "os-missing-lock", "os-ambiguous"} {
			t.Run(mode, func(t *testing.T) {
				profile := t.TempDir()
				var port int
				var srv *httptest.Server
				srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/json/version" {
						fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, "ws://127.0.0.1:"+strconv.Itoa(port)+"/devtools/browser/test")
						return
					}
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					var request struct {
						Method string `json:"method"`
					}
					if err := conn.ReadJSON(&request); err != nil {
						return
					}
					if request.Method != "Browser.getBrowserCommandLine" {
						t.Errorf("unsafe command %s", request.Method)
					}
					if strings.HasPrefix(mode, "os-") {
						_ = conn.WriteJSON(map[string]any{"id": 1, "error": map[string]any{"code": -32000, "message": "Command line not returned because --enable-automation not set."}})
						return
					}
					args := []string{"--user-data-dir=" + profile, "--remote-debugging-port=" + strconv.Itoa(port)}
					switch mode {
					case "wrong-profile":
						args[0] += "-gemini"
					case "wrong-port":
						args[1] += "0"
					case "unavailable":
						args = nil
					}
					_ = conn.WriteJSON(map[string]any{"id": 1, "result": map[string]any{"arguments": args}})
				}))
				defer srv.Close()
				port = srv.Listener.Addr().(*net.TCPAddr).Port
				c := New(port, profile, filepath.Join(t.TempDir(), "no-chrome"))
				if strings.HasPrefix(mode, "os-") {
					tools := t.TempDir()
					t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
					pidOutput := "p123\nf68"
					if mode == "os-ambiguous" {
						pidOutput += "\np456"
					}
					if err := os.WriteFile(filepath.Join(tools, "lsof"), []byte("#!/bin/sh\nprintf '%s\\n' '"+pidOutput+"'\n"), 0700); err != nil {
						t.Fatal(err)
					}
					actualProfile := profile
					if mode == "os-wrong-profile" {
						actualProfile += "-gemini"
					}
					line := c.ChromePath + " --user-data-dir=" + actualProfile + " --remote-debugging-port=" + strconv.Itoa(port)
					if err := os.WriteFile(filepath.Join(tools, "ps"), []byte("#!/bin/sh\nprintf '%s\\n' '"+line+"'\n"), 0700); err != nil {
						t.Fatal(err)
					}
					if mode != "os-missing-lock" {
						if err := os.Symlink("fixture-host-123", filepath.Join(profile, "SingletonLock")); err != nil {
							t.Fatal(err)
						}
					}
				}
				_, err := c.attach(context.Background(), port)
				if mode == "correct" || mode == "os-owner" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if !errors.Is(err, ErrChromeProfile) {
						t.Fatalf("got %v", err)
					}
					if err := c.ensureBrowser(context.Background()); !errors.Is(err, ErrChromeProfile) {
						t.Fatalf("did not fail closed: %v", err)
					}
					if c.launchAttempted || c.browser != nil {
						t.Fatal("launched/adopted foreign endpoint")
					}
				}
			})
		}
	})
	t.Run("pool workers never claim the shared tab file", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/json/list":
				fmt.Fprint(w, `[]`)
			case "/json/new":
				fmt.Fprint(w, `{"id":"worker-owned"}`)
			default:
				t.Errorf("unexpected ownership request %s", r.URL.Path)
			}
		}))
		defer srv.Close()
		profile := t.TempDir()
		c := New(srv.Listener.Addr().(*net.TCPAddr).Port, profile, "")
		c.actualPort = c.ChromePort
		c.DedicatedTab = true
		c.pool = &Pool{}
		id, err := c.findOrCreateTab()
		if err != nil || id != "worker-owned" || c.ownedTarget != id {
			t.Fatalf("worker did not create its own target: %s %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(profile, "openai-tab-id")); !os.IsNotExist(err) {
			t.Fatal("pool worker claimed the shared tab file")
		}
	})
	t.Run("owned clients never adopt another process target and close only their own", func(t *testing.T) {
		closed := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/json/list":
				fmt.Fprint(w, `[{"id":"other-process","type":"page","url":"https://chatgpt.com/c/live"}]`)
			case "/json/new":
				fmt.Fprint(w, `{"id":"owned-new"}`)
			case "/json/close/owned-new":
				closed <- "owned-new"
			default:
				t.Errorf("unexpected ownership request %s", r.URL.Path)
			}
		}))
		defer srv.Close()
		c := New(srv.Listener.Addr().(*net.TCPAddr).Port, t.TempDir(), "")
		c.actualPort = c.ChromePort
		c.DedicatedTab, c.SetupLocks = true, true
		if !c.UsesOwnedTabs() {
			t.Fatal("MCP/pooled owned client did not advertise owned-turn guard")
		}
		id, err := c.findOrCreateTab()
		if err != nil || id != "owned-new" || c.ownedTarget != id {
			t.Fatalf("adopted another process target: %s %v", id, err)
		}
		c.Close()
		if <-closed != "owned-new" {
			t.Fatal("owned shutdown did not close exactly its own target")
		}
		c.Close() // idempotent, must not close unrelated targets
	})
	t.Run("shared generations exclude legacy turns while setup remains separately serialized", func(t *testing.T) {
		profile := t.TempDir()
		a, err := LockOwnedTurn(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		defer a()
		b, err := LockOwnedTurn(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		defer b()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if _, err := LockTurn(ctx, profile); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("legacy whole-turn lock interleaved with owned generations: %v", err)
		}
		setup, err := LockSetup(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		defer setup()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel2()
		if _, err := LockSetup(ctx2, profile); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("project setup failed to serialize: %v", err)
		}
		setup()
		a()
		b()
		exclusive, err := LockTurn(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		exclusive()
		state := filepath.Join(profile, "state.json")
		owner, err := LockState(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		defer owner()
		ctx3, cancel3 := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel3()
		if _, err := LockState(ctx3, state); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("two adapter writers admitted to same journal: %v", err)
		}
		if _, err := os.Stat(state + ".owner.lock"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("owned live composer insertion settles the patched full document", func(t *testing.T) {
		if os.Getenv("CHATGPT_LIVE_COMPOSER") != "1" {
			t.Skip("set CHATGPT_LIVE_COMPOSER=1 to run against the dedicated logged-in Chrome")
		}
		// Read-only against existing threads: a new owned target, and only
		// insertion. No Send, no journal, no shared tab file, no adapter restart.
		base := fmt.Sprintf("http://127.0.0.1:%d", DefaultChromePort)
		resp, err := http.Get(base + "/json/version")
		if err != nil {
			t.Skipf("dedicated Chrome unavailable: %v", err)
		}
		var ver struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		err = json.NewDecoder(resp.Body).Decode(&ver)
		resp.Body.Close()
		if err != nil || ver.WebSocketDebuggerURL == "" {
			t.Skipf("no debugger endpoint: %v", err)
		}
		req, _ := http.NewRequest(http.MethodPut, base+"/json/new?about:blank", nil)
		created, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Skipf("cannot create owned target: %v", err)
		}
		var tgt struct {
			ID string `json:"id"`
		}
		err = json.NewDecoder(created.Body).Decode(&tgt)
		created.Body.Close()
		if err != nil || tgt.ID == "" {
			t.Skipf("owned target id missing: %v", err)
		}
		defer http.Get(base + "/json/close/" + tgt.ID) //nolint:errcheck
		alloc, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), ver.WebSocketDebuggerURL)
		defer cancelAlloc()
		tab, cancelTab := chromedp.NewContext(alloc, chromedp.WithTargetID(target.ID(tgt.ID)))
		defer cancelTab()
		tab, cancelTimeout := context.WithTimeout(tab, 120*time.Second)
		defer cancelTimeout()
		if err := chromedp.Run(tab, chromedp.Navigate("https://chatgpt.com/")); err != nil {
			t.Skipf("navigate failed: %v", err)
		}
		c := &Client{}
		// Medium multiline Unicode with tabs and a URL, then a small followup on
		// the same editor. Detecting any leftover concat relies on exact model
		// equality, so a partial read here is a real failure, not a flake.
		line := "Dữ liệu 😀 https://example.com/a\tvalue " + strings.Repeat("x", 160) + "\n"
		prompts := []string{
			strings.Repeat(line, 120)[:19000],
			line[:120],
		}
		for _, prompt := range prompts {
			if err := c.typePrompt(tab, tab, prompt); err != nil {
				t.Fatalf("patched typePrompt failed for %d bytes: %v", len(prompt), err)
			}
			var exact bool
			if err := chromedp.Run(tab, chromedp.Evaluate(`(() => {const e=document.querySelector('.ProseMirror[contenteditable="true"]');const n=e&&e.pmViewDesc&&e.pmViewDesc.node;return n&&n.type&&n.type.name==='doc'&&n.textBetween(0,n.content.size,'\n','\n')===`+quoteJS(prompt)+`})()`, &exact)); err != nil || !exact {
				t.Fatalf("model does not hold the full prompt: bytes=%d exact=%v err=%v", len(prompt), exact, err)
			}
		}
		// Clean only this owned draft; never submit.
		if err := chromedp.Run(tab, chromedp.Evaluate(`(() => {const e=document.querySelector('.ProseMirror[contenteditable="true"]');if(e){e.focus();document.execCommand('selectAll');document.execCommand('delete')}})()`, nil)); err != nil {
			t.Fatalf("cleanup failed: %v", err)
		}
	})

	const uuid = "ws://127.0.0.1:9222/devtools/browser/abc-123"
	_, port := versionServer(t, uuid)
	c := New(0, t.TempDir(), "")
	c.actualPort = port
	if c.browserAlive() {
		t.Fatal("empty wsURL must report stale")
	}
	c.wsURL = uuid
	if !c.browserAlive() {
		t.Fatal("matching endpoint must report alive")
	}
	c.wsURL = "ws://127.0.0.1:9222/devtools/browser/rotated-456"
	if c.browserAlive() {
		t.Fatal("rotated endpoint (Chrome restart) must report stale")
	}
	// Dead listener must report stale, never hang: bind then close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.actualPort = dead
	if c.browserAlive() {
		t.Fatal("unreachable port must report stale")
	}
}
