package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const bridgeProject = "OpenCode Bridge"

var projectPattern = regexp.MustCompile(`^https://chatgpt\.com/g/g-p-[a-zA-Z0-9-]+/project$`)

// ensureProject uses only the logged-in UI, never private backend endpoints.
// A saved project URL avoids rediscovery and duplicate creation after restarts.
// Failure is closed: never silently submit an adapter chat on the main page.
func (c *Client) ensureProject(ctx, tab context.Context) (string, error) {
	path := filepath.Join(c.ProfileDir, "openai-project-url")
	b, err := os.ReadFile(path)
	if err == nil {
		if !projectPattern.Match(b) {
			return "", errors.New("invalid openai-project-url; REPAIR: remove or replace with OpenCode Bridge Project URL")
		}
		return string(b), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	url, err := c.ensureProjectNamed(ctx, tab, bridgeProject)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(url), 0600); err != nil {
		return "", err
	}
	return url, nil
}

// ensureProjectNamed reuses (or creates via the same proven UI-dialog flow)
// the ChatGPT project with the given display name. Unlike ensureProject it
// keeps no disk cache: the caller persists the returned URL (the adapter
// holds it in session state). Failure is closed: never silently fall back to
// another project.
func (c *Client) ensureProjectNamed(ctx, tab context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("project name required; REPAIR: supply a session project name")
	}
	tab, cancel := requestContext(ctx, tab)
	defer cancel()
	if err := chromedp.Run(tab, chromedp.Navigate(HomePage)); err != nil {
		return "", err
	}
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return "", err
	}
	// The observed UI exposes a separate named new-chat button per Project.
	// Match by exact aria-label equality in JS: names carry spaces, dashes
	// and slashes that make selector interpolation fragile.
	findJS := `(() => {
	 const want = "New chat in " + ` + strconv.Quote(name) + `;
	 const b = [...document.querySelectorAll('[aria-label]')].find(e => e.getAttribute('aria-label') === want);
	 if (!b) return false; b.click(); return true;
	})()`
	var found bool
	if err := chromedp.Run(tab, chromedp.Evaluate(findJS, &found)); err != nil {
		return "", err
	}
	if !found {
		var opened bool
		if err := chromedp.Run(tab, chromedp.Evaluate(`(() => {
		 const b = document.querySelector('[aria-label="Add new project"]');
		 if (!b) return false; b.click(); return true;
		})()`, &opened)); err != nil || !opened {
			return "", fmt.Errorf("Project controls unavailable; REPAIR: create %q in logged Chrome", name)
		}
		wait, done := context.WithTimeout(tab, 10*time.Second)
		err := chromedp.Run(wait, chromedp.WaitVisible("#chatgpt-project-name", chromedp.ByQuery))
		done()
		if err != nil {
			return "", fmt.Errorf("Project name dialog: %w", err)
		}
		setName := `(() => {
		 const i = document.querySelector('#chatgpt-project-name');
		 Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(i,` + strconv.Quote(name) + `);
		 i.dispatchEvent(new Event('input',{bubbles:true}));
		})()`
		if err := chromedp.Run(tab, chromedp.Evaluate(setName, nil)); err != nil {
			return "", err
		}
		if err := pause(tab, 300*time.Millisecond); err != nil {
			return "", err
		}
		var created bool
		if err := chromedp.Run(tab, chromedp.Evaluate(`(() => {
		 const b = [...document.querySelectorAll('[role="dialog"] button')].find(e => e.innerText.trim() === 'Create project');
		 if (!b || b.disabled) return false; b.click(); return true;
		})()`, &created)); err != nil || !created {
			return "", fmt.Errorf("Project creation button unavailable; REPAIR: finish %q creation in logged Chrome", name)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var url string
		if err := chromedp.Run(tab, chromedp.Location(&url)); err != nil {
			return "", err
		}
		if projectPattern.MatchString(url) {
			log.Printf("[chatgpt] project=%q url=%s reused=%v", name, url, found)
			return url, nil
		}
		if err := pause(tab, 250*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("Project URL not observed for %q; REPAIR: create it in logged Chrome", name)
}
