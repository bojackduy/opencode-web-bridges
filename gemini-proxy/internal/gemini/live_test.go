//go:build live

// Live DOM discovery probe for Gemini Web. MANUAL ONLY:
//
//	GEMINI_LIVE=1 go test -tags live -run TestLiveStatus ./internal/gemini/ -v
//
// Read-only: attaches (or headless-launches, isolated profile ~/.gemini-proxy
// on CDP 9223), reports login/composer selector hits plus title/URL. It never
// types, submits, closes tabs, or touches the chatgpt bridge's profile/port.
// `go test ./...` (no tags) never runs this file.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestLiveStatus(t *testing.T) {
	if os.Getenv("GEMINI_LIVE") != "1" {
		t.Skip("manual live probe: set GEMINI_LIVE=1")
	}
	c := New(DefaultChromePort, "", "")
	c.Background = true // headless: no display needed, isolated profile
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := c.LoginState(ctx)
	if err != nil {
		t.Fatalf("LoginState: %v", err)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	fmt.Printf("STATUS\n%s\n", raw)

	// Per-selector hit matrix so the inventory can be verified line by line.
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Printf("CTX tab.Err=%v browser.Err=%v tabNil=%v\n", c.tab.Err(), c.browser.Err(), c.tab == nil)
	// Fresh tab from the same allocator: isolates "c.tab poisoned" vs
	// "allocator/browser dead".
	fresh, freshDone := chromedp.NewContext(c.browser)
	defer freshDone()
	fctx, fcancel := context.WithTimeout(fresh, 30*time.Second)
	defer fcancel()
	var two int
	freshErr := chromedp.Run(fctx, chromedp.Evaluate(`1+1`, &two))
	fmt.Printf("FRESH eval 1+1=%d err=%v\n", two, freshErr)
	rctx, rcancel := context.WithTimeout(c.tab, 30*time.Second)
	defer rcancel()
	// Hard diagnostics first: real URL/title/ready-state with errors surfaced
	// (the client's helpers swallow errors by design; the probe must not).
	var url, title, ready string
	var htmlLen int
	errURL := chromedp.Run(rctx, chromedp.Location(&url))
	errTitle := chromedp.Run(rctx, chromedp.Title(&title))
	errReady := chromedp.Run(rctx, chromedp.Evaluate(`document.readyState`, &ready))
	errLen := chromedp.Run(rctx, chromedp.Evaluate(`document.documentElement ? document.documentElement.outerHTML.length : -1`, &htmlLen))
	fmt.Printf("DIAG url=%q title=%q ready=%q htmlLen=%d errURL=%v errTitle=%v errReady=%v errLen=%v\n",
		url, title, ready, htmlLen, errURL, errTitle, errReady, errLen)
	// Debugger-level truth (plain HTTP, no chromedp): is the endpoint alive,
	// what targets exist, and where are they?
	dbg, dbgErr := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", c.actualPort))
	if dbgErr != nil {
		fmt.Printf("DBG version err=%v\n", dbgErr)
	} else {
		var vbuf strings.Builder
		_, _ = io.Copy(&vbuf, dbg.Body)
		_ = dbg.Body.Close()
		fmt.Printf("DBG version=%s\n", vbuf.String())
	}
	lst, lstErr := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", c.actualPort))
	if lstErr != nil {
		fmt.Printf("DBG list err=%v\n", lstErr)
	} else {
		var lbuf strings.Builder
		_, _ = io.Copy(&lbuf, lst.Body)
		_ = lst.Body.Close()
		raw := lbuf.String()
		if len(raw) > 1500 {
			raw = raw[:1500] + "…"
		}
		fmt.Printf("DBG list=%s\n", raw)
	}
	for name, sels := range map[string][]string{
		"login":    loginSelectors,
		"loggedIn": loggedInSelectors,
		"composer": composerSelectors,
		"send":     sendSelectors,
		"newChat":  newChatSelectors,
		"stop":     stopSelectors,
		"answer":   answerSelectors,
	} {
		for _, sel := range sels {
			hit := c.jsBool(rctx, fmt.Sprintf(`!!document.querySelector(%q)`, sel))
			fmt.Printf("SEL %-8s %-60q hit=%v\n", name, sel, hit)
		}
	}
	fmt.Printf("HTML_SNIPPET %.2000q\n", c.jsString(rctx, `document.documentElement.outerHTML.slice(0,2000)`))

	// DOM discovery dumps (read-only): shadow-piercing button inventory,
	// editable nodes, and the composer subtree — used to pin send/composer
	// selectors. Gemini is Lit-based; controls may live in shadow roots
	// that document.querySelector cannot see.
	const walker = `(function(sel, want, limit){
  var out=[];var stack=[document];
  while(stack.length&&out.length<limit){
    var root=stack.pop();var els=[];
    try{els=root.querySelectorAll(sel);}catch(e){continue;}
    for(var i=0;i<els.length&&out.length<limit;i++){var h=null;try{h=want(els[i]);}catch(e){}if(h)out.push(h);}
    var all=[];try{all=root.querySelectorAll('*');}catch(e){continue;}
    for(var j=0;j<all.length;j++){if(all[j].shadowRoot)stack.push(all[j].shadowRoot);}
  }return out;})`
	// evalProbe runs one discovery expression, never failing the test;
	// errors print inline so silent-empty results are distinguishable.
	evalProbe := func(name, expr string) {
		var s string
		if err := chromedp.Run(rctx, chromedp.Evaluate(expr, &s)); err != nil {
			fmt.Printf("%s ERR=%v\n", name, err)
			return
		}
		fmt.Printf("%s\n%s\n", name, s)
	}
	var buttons string
	_ = chromedp.Run(rctx, chromedp.Evaluate(walker+`(
    'button',
    function(el){var lb=el.getAttribute('aria-label')||'(none)';return el.tagName+' aria-label="'+lb+'" disabled='+(el.disabled||el.getAttribute('aria-disabled'));},
    80).join('\n')`, &buttons))
	fmt.Printf("BUTTONS\n%s\n", buttons)
	var editables string
	_ = chromedp.Run(rctx, chromedp.Evaluate(walker+`(
    '[contenteditable="true"]',
    function(el){return el.tagName+'.'+String(el.className||'').split(' ').slice(0,3).join('.')+' aria-label='+(el.getAttribute('aria-label')||'(none)');},
    20).join('\n')`, &editables))
	fmt.Printf("EDITABLES\n%s\n", editables)
	var composerScope string
	_ = chromedp.Run(rctx, chromedp.Evaluate(`(function(){
  function deep(sel){var st=[document];while(st.length){var r=st.pop();var el=null;try{el=r.querySelector(sel);}catch(e){}if(el)return el;var all=[];try{all=r.querySelectorAll('*');}catch(e){}for(var j=0;j<all.length;j++){if(all[j].shadowRoot)st.push(all[j].shadowRoot);}}return null;}
  var ed=deep('div[contenteditable="true"]');if(!ed)return '(no composer)';
  var scope=ed.closest('form')||(ed.parentElement&&ed.parentElement.parentElement);
  var html=scope?scope.outerHTML.slice(0,3000):ed.outerHTML.slice(0,1000);return html;})()`, &composerScope))
	fmt.Printf("COMPOSER_SCOPE\n%s\n", composerScope)

	// Send-control discovery (read-only): any pierced element whose
	// accessible name mentions send/submit/go, all Material icon ligatures
	// (send buttons are often just an icon), and the composer's following
	// siblings (the send control usually sits next to the input).
	evalProbe("SEND_CANDS", walker+`(
    '*',
    function(el){var lb=(el.getAttribute('aria-label')||el.getAttribute('data-tooltip')||'').toLowerCase();if(/send|submit/.test(lb))return el.tagName+' aria-label="'+lb+'" text="'+(el.textContent||'').trim().slice(0,40)+'"';return null;},
    200).join('\n')`)
	evalProbe("ICONS", walker+`(
    'mat-icon',
    function(el){return 'mat-icon fonticon="'+(el.getAttribute('fonticon')||'(none)')+'" text="'+(el.textContent||'').trim().slice(0,30)+'"';},
    200).join('\n')`)
	evalProbe("SIBS", `(function(){
  try{
  function deep(sel){var st=[document];while(st.length){var r=st.pop();var el=null;try{el=r.querySelector(sel);}catch(e){}if(el)return el;var all=[];try{all=r.querySelectorAll('*');}catch(e){}for(var j=0;j<all.length;j++){if(all[j].shadowRoot)st.push(all[j].shadowRoot);}}return null;}
  var ed=deep('div.ql-editor[contenteditable="true"]');if(!ed)return '(no composer)';
  var wrap=ed.closest('[data-test-id="textarea-wrapper"]')||ed.parentElement;
  if(!wrap)return '(no wrap)';
  var names=[];var n=wrap.nextElementSibling;var k=0;
  while(n&&k<8){var ga='?';try{ga=n.getAttribute?n.getAttribute('aria-label'):'?';}catch(e){}names.push(n.tagName+'.'+String(n.className||'').split(' ').slice(0,2).join('.')+' aria-label='+ga);n=n.nextElementSibling;k++;}
  return 'WRAP='+wrap.tagName+' SIBS=['+names.join(' | ')+']';}catch(e){return 'SIBS-ERR:'+e.message;}})()`)
}
