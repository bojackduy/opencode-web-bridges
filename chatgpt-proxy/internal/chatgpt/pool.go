package chatgpt

import (
	"context"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Pool shares one long-lived CDP connection. Its root is also the read-only
// health target; workers own separate targets, never the root or each other.
type Pool struct {
	mu      sync.Mutex
	Root    *Client
	Clients []*Client
	cancel  context.CancelFunc
}

func (c *Client) UsesOwnedTabs() bool { return c.DedicatedTab && c.SetupLocks }

func NewPool(port int, profile, chrome string, background bool, size int) *Pool {
	root := New(port, profile, chrome)
	life, cancel := context.WithCancel(context.Background())
	root.life = life
	root.Background, root.DedicatedTab = background, true
	p := &Pool{Root: root, cancel: cancel}
	for range size {
		c := New(port, profile, chrome)
		c.Background, c.DedicatedTab, c.SetupLocks = background, true, true
		c.pool = p
		p.Clients = append(p.Clients, c)
	}
	return p
}

// Called under the worker mutex and profile setup lock. The root initializes
// on its permanent tab context, never on a request deadline/cancellation child.
func (p *Pool) connectClient(ctx context.Context, c *Client) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	unlock, err := LockSetup(ctx, p.Root.ProfileDir)
	if err != nil {
		return err
	}
	defer unlock()
	p.Root.mu.Lock()
	defer p.Root.mu.Unlock()
	root, err := p.Root.tabCtx(ctx)
	if err != nil {
		return err
	}
	c.browser, c.wsURL, c.actualPort = root, p.Root.wsURL, p.Root.actualPort
	return nil
}

func (p *Pool) LoginState(ctx context.Context) (*Status, error) {
	// Snapshot the owned health tab under the locks, then release everything
	// before touching the network: Navigate + SPA hydration can take tens of
	// seconds, and holding p.mu/Root.mu across that would queue every worker
	// connect behind readiness (the stall seen in pool live probes).
	p.mu.Lock()
	unlock, err := LockSetup(ctx, p.Root.ProfileDir)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.Root.mu.Lock()
	base, terr := p.Root.tabCtx(ctx)
	p.Root.mu.Unlock()
	unlock()
	p.mu.Unlock()
	if terr != nil {
		return nil, terr
	}
	tab, cancel := requestContext(ctx, base)
	defer cancel()
	var url string
	if err := chromedp.Run(tab, chromedp.Location(&url)); err != nil {
		return nil, err
	}
	// A newly-created health target starts blank. Initialize ONLY this
	// owned target once; subsequent health probes are strictly read-only.
	// Bounded by the caller's ctx (readyz passes ~10s: a cold root reports
	// not-ready fast instead of hanging the health endpoint).
	if url == "about:blank" {
		nav, ncancel := context.WithTimeout(tab, 45*time.Second)
		defer ncancel()
		if err := chromedp.Run(nav, chromedp.Navigate("https://chatgpt.com/")); err != nil {
			return nil, err
		}
		if err := p.Root.waitPageReady(ctx, tab, 30*time.Second); err != nil {
			return nil, err
		}
	}
	return p.Root.probeState(ctx, tab)
}

func (p *Pool) Close() {
	// Shutdown intentionally cancels all workers, unlike request cancellation.
	// Do this before waiting for their mutexes so an in-flight turn can unwind.
	p.cancel()
	for _, c := range p.Clients {
		c.Close()
	}
	p.Root.Close()
}
