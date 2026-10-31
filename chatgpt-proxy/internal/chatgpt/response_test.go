package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Status must carry the challenge flag on the wire: the adapter's /readyz
// and the plugin's status/doctor read it to name a human-verification gate
// instead of misreporting logged-out. BodyLen stays content-free (a number).
func TestStatusChallengeWireShape(t *testing.T) {
	b, err := json.Marshal(Status{LoggedIn: false, Composer: false, Challenge: true, BodyLen: 120})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v["challenge"] != true {
		t.Fatalf("challenge flag lost on the wire: %s", b)
	}
	if v["body_len"] != float64(120) {
		t.Fatalf("body length lost on the wire: %s", b)
	}
}

func TestResponseDOMIdentityAndScope(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("Chrome unavailable for DOM regression fixtures")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	// root is the unbounded fixture connection. Every subtest derives its OWN
	// bounded context from it, so a slow — or externally suspended — subtest can
	// no longer drain a single shared wall-clock budget and cancel the cases
	// that follow it.
	root, cancelRoot := chromedp.NewContext(alloc)
	defer cancelRoot()
	tab := root
	// set replaces the fixture DOM. It takes the SUBTEST's *testing.T and runs
	// on root: a fixture failure is reported inside that subtest instead of
	// failing the parent test and cascading into unrelated cases.
	set := func(t *testing.T, html string) {
		t.Helper()
		if err := chromedp.Run(root, chromedp.ActionFunc(func(ctx context.Context) error { return nil })); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(root, chromedp.Evaluate(`document.body.innerHTML = `+quoteJS(html), nil)); err != nil {
			t.Fatal(err)
		}
	}
	// sub runs one browser subtest with its own budget derived from root.
	sub := func(name string, fn func(t *testing.T, tab context.Context)) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(root, 45*time.Second)
			defer cancel()
			fn(t, ctx)
		})
	}
	sub("post-click DOM is never proof of non-submission", func(t *testing.T, tab context.Context) {
		p := sendProof{Readable: true, ComposerExact: true}
		if !p.provenUnsubmitted() {
			t.Fatal("no-action exact readable draft should be retryable")
		}
		for _, p := range []sendProof{
			{ActionAttempted: true, Readable: true, ComposerExact: true},
			{ComposerExact: true}, {Readable: true},
			{Readable: true, ComposerExact: true, UserBubble: true},
			{Readable: true, ComposerExact: true, Generating: true},
		} {
			if p.provenUnsubmitted() {
				t.Fatalf("ambiguous send permitted retry: %+v", p)
			}
		}
		set(t, `<div class="ProseMirror" contenteditable="true">draft</div><button data-testid="send-button">Send</button>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.sendClicks=0;document.querySelector('button').onclick=()=>window.sendClicks++`, nil)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(tab, 2200*time.Millisecond)
		defer cancel()
		if _, err := (&Client{}).submit(ctx, tab, "draft"); err == nil || errors.Is(err, ErrNotSubmitted) {
			t.Fatalf("in-flight/cancelled Send lost uncertainty: %v", err)
		}
		var clicks int
		// Observe the click count over the unbounded root connection: the send
		// attempt is what must be counted exactly once, and a subtest-local
		// deadline expiring must not masquerade as a missing click.
		if err := chromedp.Run(root, chromedp.Evaluate(`window.sendClicks`, &clicks)); err != nil || clicks != 1 {
			t.Fatalf("possibly submitted turn repeated: clicks=%d err=%v", clicks, err)
		}
	})
	sub("chrome icon svgs are not answer visuals", func(t *testing.T, tab context.Context) {
		// A real ChatGPT message with no ".markdown" container exposes the whole
		// block, including 16-24px UI icon SVGs. Those are page chrome, not answer
		// visuals: they must not become "[SVG: no accessible description...]"
		// placeholders nor near-blank 40x40 PNGs (a 20px icon at 2x).
		fixture := `<div data-message-author-role="user">ask</div>` +
			`<div data-talvt-turn-state="complete"><div data-chatgpt-search-message-ids="msg-1">` +
			`<h4 data-conversation-role="assistant">ChatGPT said:</h4><div>` +
			`<button><svg width="20" height="20" aria-hidden="true"><path d="M0 0h20v20H0z"/></svg></button>` +
			`<svg width="20" height="20"><path d="M0 0h20v20H0z"/></svg>` +
			`<div data-d-component="row"><svg width="1em" height="1em" data-d-component="icon"><path d="M0 0h10v10H0z"/></svg><span>Alpha</span></div>` +
			`<svg width="600" height="400" viewBox="0 0 600 400"><title>Diagram</title><rect width="600" height="400" fill="#eee"/></svg>` +
			`</div></div></div>`
		set(t, fixture)
		st, err := responseProbe(tab, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(st.Last, "no accessible description") {
			t.Fatalf("chrome icon serialized as an answer visual: %q", st.Last)
		}
		if strings.Count(st.Last, "[SVG: Diagram]") != 1 {
			t.Fatalf("diagram fallback missing or duplicated: %q", st.Last)
		}
		// Rasterization must select only the substantive diagram. The previous
		// unfiltered scan also returned the two icon-sized SVGs as blank PNGs.
		var raw []struct {
			Alt     string `json:"alt"`
			DataURL string `json:"dataURL"`
		}
		js := "(" + rasterSVGJS + ")('msg-1')"
		if err := chromedp.Run(tab, chromedp.Evaluate(js, &raw, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 1 || raw[0].Alt != "Diagram" {
			t.Fatalf("rasterized chrome/icon svgs instead of the diagram: %+v", raw)
		}
		if !ValidRasterImage(raw[0].DataURL) {
			t.Fatalf("diagram did not rasterize to a valid PNG: %d bytes", len(raw[0].DataURL))
		}
	})
	old := `<div data-chatgpt-search-message-ids="old"><h4>ChatGPT said:</h4><div>OLD TOOL CALL</div></div>`
	set(t, old+`<div data-chatgpt-search-message-ids="analysis"><h4>ChatGPT said:</h4><div>Thought for 4 seconds</div></div>`)
	before, err := responseProbe(tab, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two old nodes collapse into one; the total heading count does not grow.
	// A stale role node and unrelated markdown/footer must not override NEW.
	set(t, old+`<div data-message-author-role="assistant" data-message-id="old-role">STALE</div><h4>You said:</h4><div>tool output</div><div data-talvt-turn-state="complete"><div data-chatgpt-search-message-ids="new new"><h4 data-conversation-role="assistant">ChatGPT said:</h4><div><p>NEW</p><button>Copy</button></div></div></div><div class="markdown">UNRELATED</div><h4>Latest response</h4><div>FOOTER</div>`)
	st, err := responseProbe(tab, before.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Last != "NEW" || !st.Complete {
		t.Fatalf("wrong fresh scoped response: %+v", st)
	}
	set(t, old+`<h4>You said:</h4><div><div class="markdown">PRIVATE USER TEXT</div></div><div role="alert">Something went wrong</div>`)
	st, err = responseProbe(tab, before.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Last != "" || !st.ErrorText {
		t.Fatalf("old/user text leaked or error missed: %+v", st)
	}
	set(t, `<div data-message-author-role="user">request</div><div data-message-author-role="assistant" data-message-id="legacy"><div class="markdown"><div data-markdown-copy="code-block"><pre><code class="language-json">{"x":1}</code></pre></div></div></div>`)
	st, err = responseProbe(tab, before.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(st.Last, `{"x":1}`) != 1 || !strings.Contains(st.Last, "```json") {
		t.Fatalf("nested code block corrupt: %q", st.Last)
	}
	set(t, `<div data-message-author-role="assistant" data-message-id="old-thought"><div data-thinking>PRIVATE OLD REASONING</div></div><h4>You said:</h4><div>request</div><div data-chatgpt-search-message-ids="thought-new"><h4>ChatGPT said:</h4><div><div data-testid="thought-block">Thought for 3 seconds
Scoped reasoning</div><p>Only answer</p></div></div>`)
	st, err = responseProbe(tab, before.IDs)
	if err != nil || st.Last != "Only answer" || st.Reasoning != "Scoped reasoning" || st.LastID != "thought-new" {
		t.Fatalf("reasoning/body scope regression: %+v %v", st, err)
	}
	probe := func(body string) string {
		t.Helper()
		set(t, old+`<h4>You said:</h4><div>PRIVATE USER</div><div data-message-id="rich"><h4>ChatGPT said:</h4><div><div class="markdown">`+body+`</div><button>Regenerate</button></div></div><div class="markdown">FOOTER</div>`)
		got, err := responseProbe(tab, before.IDs)
		if err != nil {
			t.Fatal(err)
		}
		return got.Last
	}
	sub("rich Markdown", func(t *testing.T, tab context.Context) {
		math := probe(`<p>Potentially <span class="katex"><span class="katex-mathml"><math><semantics><mrow><mi>O</mi><mo>(</mo><msup><mi>n</mi><mn>2</mn></msup><mo>)</mo></mrow><annotation encoding="application/x-tex">O(n^2)</annotation></semantics></math></span><span class="katex-html" aria-hidden="true">O(n^2)</span></span>. Linear <span class="katex"><span class="katex-mathml"><math><semantics><mrow>O(n)</mrow><annotation encoding="application/x-tex">O(n)</annotation></semantics></math></span><span class="katex-html" aria-hidden="true">O(n)</span></span>.</p><span class="katex-display"><span class="katex"><span class="katex-mathml"><math><semantics><annotation encoding="application/x-tex">x^2 + y^2</annotation></semantics></math></span><span class="katex-html">DUPLICATE GLYPHS</span></span></span><p>Repeat O(n) O(n) intentionally.</p>`)
		if math != "Potentially O(n^2). Linear O(n).\n\nx^2 + y^2\n\nRepeat O(n) O(n) intentionally." {
			t.Fatalf("math duplicated or source/prose lost: %q", math)
		}
		// Custom HTML elements are implicitly unwrapped. These are NOT tests of
		// escaped literal text, and neither fixture proves a reasoning channel.
		if got := probe(`<p>Answer text.</p><escape></escape>`); got != "Answer text." {
			t.Fatalf("empty escape pair leaked: %q", got)
		}
		if got := probe(`<escape>` + "\n\n" + `</escape><p>Answer text.</p><escape>` + "\n\n" + `</escape>`); got != "Answer text." {
			t.Fatalf("escape markers leaked: %q", got)
		}
		// Real escaped literal TEXT, combined and split across text/span nodes.
		for _, body := range []string{
			`<p>&lt;escape&gt;&lt;/escape&gt;</p><p>Answer text.</p>`,
			`<p>&lt;escape&gt;<span>&lt;/escape&gt;</span></p><p>Answer text.</p>`,
			`<p><span>&lt;escape&gt;</span> <span>&lt;/escape&gt;</span></p><p>Answer text.</p>`,
		} {
			if got := probe(body); got != "Answer text." {
				t.Fatalf("literal empty artifact leaked: %q", got)
			}
		}
		if got := probe(`&lt;escape&gt;&lt;/escape&gt;`); got != "" {
			t.Fatalf("artifact-only answer fabricated: %q", got)
		}
		if got := probe(`<p>Example &lt;escape&gt;&lt;/escape&gt; stays literal.</p>`); got != "Example <escape></escape> stays literal." {
			t.Fatalf("prose altered: %q", got)
		}
		if got := probe(`<p><code>&lt;escape&gt;&lt;/escape&gt;</code></p><pre><code>&lt;escape&gt;&lt;/escape&gt;</code></pre>`); !strings.Contains(got, "`<escape></escape>`") || !strings.Contains(got, "```\n<escape></escape>\n```") {
			t.Fatalf("code altered: %q", got)
		}
		wire := `<opencode_bridge_call>{"name":"x","arguments":{"literal":"<escape></escape>"}}</opencode_bridge_call>`
		if got := probe(strings.ReplaceAll(strings.ReplaceAll(wire, "&", "&amp;"), "<", "&lt;")); got != wire {
			t.Fatalf("tool string altered: %q", got)
		}
		if got := probe(`<p>Use &lt;escape&gt; tags in config.</p>`); got != "Use <escape> tags in config." {
			t.Fatalf("escape text in prose altered: %q", got)
		}
		for _, tc := range []struct{ tex, want string }{
			{`R(i+t)<R(j+t)\qquad 0\le t\le k`, "R(i+t)<R(j+t)  0≤ t≤ k"},
			{`j,\ j+1,\ \ldots,\ j+k`, "j, j+1, …, j+k"},
			{`x_{i+1}^{2} \geq \alpha`, `x\_{i+1}^{2} ≥ α`},
			{`\frac{a}{b} + \unknown{x}`, "` \\frac{a}{b} + \\unknown{x} `"},
		} {
			body := `<span class="katex-display"><span class="katex"><math><semantics><annotation encoding="application/x-tex">` + strings.ReplaceAll(tc.tex, "<", "&lt;") + `</annotation></semantics></math><span class="katex-html">DUPLICATE</span></span></span>`
			if got := probe(body); got != tc.want {
				t.Fatalf("terminal math %q: got %q want %q", tc.tex, got, tc.want)
			}
		}
		got := probe(`<h1>Title</h1><h4>Detail</h4><p>A <strong>bold</strong>, <em>soft</em>, <del>gone</del> <code>a` + "`" + `b</code> and <a href="https://example.com/a(b)?q=x&amp;y=2" title="a &quot;quote&quot;">[link]</a>.<br>Next</p><ul><li>First<ul><li>Nested</li></ul></li><li>Second</li></ul><ol start="3"><li>Third</li><li>Fourth</li></ol><blockquote><p>Quote</p><p>More</p></blockquote><table><thead><tr><th>Name</th><th>Value</th></tr></thead><tbody><tr><td>A|B</td><td><code>x|y</code><br>next</td></tr></tbody></table><hr><div role="toolbar">PRIVATE TOOLBAR</div><div data-thinking>PRIVATE THOUGHT</div><span hidden>PRIVATE HIDDEN</span>`)
		for _, want := range []string{"# Title", "#### Detail", "**bold**", "*soft*", "~~gone~~", "``a`b``", `[\[link\]](<https://example.com/a(b)?q=x&y=2> "a \"quote\"")`, "  \nNext", "- First\n  \n  - Nested", "3. Third\n4. Fourth", "> Quote\n> \n> More", "| Name | Value |\n| --- | --- |\n| A\\|B | `x\\|y`<br>next |", "\n\n---"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
		for _, forbidden := range []string{"PRIVATE", "FOOTER", "Regenerate", "OLD TOOL CALL"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("leaked %q: %s", forbidden, got)
			}
		}
		got = probe(`<img alt="Comparison diagram" src="https://example.com/diagram.png"><img src="https://chatgpt.com/backend-api/files/x?token=PRIVATE"><svg role="img"><title>Elimination</title><text>rotation 4 eliminated</text></svg><canvas></canvas><p>Literal \le in prose <code>\qquad O(n^2)</code>.</p>`)
		for _, want := range []string{
			"[Image: Comparison diagram] [source](<https://example.com/diagram.png>)",
			"[Image: no accessible description; visual content not extracted]",
			"[SVG: Elimination; rotation 4 eliminated]", "[Canvas: no accessible description; visual content not extracted]",
			"Literal \\\\le in prose `\\qquad O(n^2)`.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing fallback %q: %s", want, got)
			}
		}
		if strings.Contains(got, "PRIVATE") {
			t.Fatal("authenticated visual URL leaked")
		}
		// Semantic attributes/nesting from the inspected real comparison card;
		// ordinary div paragraphs must not be flattened along with these rows.
		card := `<div data-d-component="box"><p>The same mismatch survives when both starts shift.</p>`
		for i, pair := range [][2]string{{"abac", "abad"}, {"bac", "bad"}, {"ac", "ad"}, {"c", "d"}} {
			label := fmt.Sprintf("t = %d", i)
			caption := fmt.Sprintf("Rotation %d beats rotation %d", i, i+4)
			card += `<div data-d-component="row" data-d-direction="row"><div data-d-component="box"><p>` + label + `</p></div><div data-d-component="box"><div data-d-component="row" data-d-direction="row"><p>` + pair[0] + `</p><p>&lt;</p><p>` + pair[1] + `</p></div><p>` + caption + `</p></div><svg data-d-component="icon"><path d="M5.636 5.636"></path></svg></div>`
		}
		card += `<p><strong>Four rotations eliminated after one comparison spanning four characters.</strong></p></div><div><p>Separate paragraph.</p><p>Still separate.</p></div>`
		got = probe(card)
		for i, comparison := range []string{"abac < abad", "bac < bad", "ac < ad", "c < d"} {
			want := fmt.Sprintf("- t = %d — %s Rotation %d beats rotation %d", i, comparison, i, i+4)
			if !strings.Contains(got, want) {
				t.Errorf("comparison row missing %q: %s", want, got)
			}
		}
		if !strings.Contains(got, "Separate paragraph.\n\nStill separate.") || strings.Contains(got, "[SVG:") {
			t.Fatalf("card grouping leaked icon or merged paragraphs: %s", got)
		}
	})
	sub("code whitespace and safe fences", func(t *testing.T, tab context.Context) {
		raw := "{\n  \"fence\": \"```\",\n\n\n  \"path\": \"a\\\\b\"\n}\n"
		got := probe(`<div data-markdown-copy="code-block"><span data-markdown-copy="exclude">JSON</span><pre><code class="language-json">` + raw + `</code></pre></div>`)
		if got != "````json\n"+raw+"````" {
			t.Fatalf("code changed: %q", got)
		}
		got = probe(`<ul><li>Example<pre><code class="language-c++">  a\n\n\n  b</code></pre></li></ul>`)
		if !strings.Contains(got, "  ```c++\n    a\\n\\n\\n  b\n  ```") {
			t.Fatalf("nested code layout: %q", got)
		}
	})
	sub("literal wire protocol and plain text", func(t *testing.T, tab context.Context) {
		raw := "<opencode_bridge_call>\n{\n  \"name\": \"echo\",\n  \"arguments\": {\"text\": \"a\\\\b_*`\"}\n}\n</opencode_bridge_call>"
		html := strings.ReplaceAll(strings.ReplaceAll(raw, "<", "&lt;"), ">", "&gt;")
		if got := probe(`<p>` + html + `</p>`); got != raw {
			t.Fatalf("wire protocol changed: %q", got)
		}
		for _, plain := range []string{"ACK", "A plain answer."} {
			if got := probe(`<p>` + plain + `</p>`); got != plain {
				t.Fatalf("plain answer changed: %q", got)
			}
		}
	})
	sub("exact collapsed user recovery and bound serialized source", func(t *testing.T, tab context.Context) {
		full := strings.Repeat("Long literal prompt_* with  two spaces\tand JSON \\\"quoted\\\".\n", 2200) + " … Show more"
		set(t, `<div data-turn-key="fixture-user"><h4>You said:</h4><div><span class="text">truncated prefix</span><button onclick="this.parentElement.querySelector('.text').textContent=window.__full;this.textContent='Show less'">Show more</button><button aria-label="Copy message">Copy</button></div></div><div data-talvt-turn-state="complete"><h4>ChatGPT said:</h4><div>finished</div></div>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__full = `+quoteJS(full), nil)); err != nil {
			t.Fatal(err)
		}
		var expanded bool
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryExpandJS, &expanded)); err != nil || !expanded {
			t.Fatal("scoped expansion failed", err)
		}
		var corr struct {
			User, UserID string
			Answers      int
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil || corr.User != strings.TrimSpace(full) || corr.UserID != "fixture-user" || corr.Answers != 1 {
			t.Fatal("full exact binding or literal UI suffix lost", err)
		}
		set(t, `<div><h4>You said:</h4><div>old matching prompt</div><h4>ChatGPT said:</h4><div>old answer</div><h4>You said:</h4><div>genuinely different prompt</div><h4>ChatGPT said:</h4><div>new answer</div></div>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil || corr.User != "genuinely different prompt" || corr.User == "old matching prompt" {
			t.Fatal("stale prompt binding", err)
		}
		// The fallback reads only the exact DOM identity, not the latest node.
		var messageID atomic.Value
		messageID.Store("fixture-user")
		var dilSource atomic.Value
		dilSource.Store(map[string]any{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/auth/session":
				_, _ = w.Write([]byte(`{"accessToken":"fixture-token"}`))
			case "/backend-api/conversation/fixture":
				_ = json.NewEncoder(w).Encode(map[string]any{"mapping": map[string]any{"fixture-user": map[string]any{"message": map[string]any{"id": messageID.Load(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{`\<wire\> \*literal\* [https://example.com](https://example.com) &#x20; \"quoted\"`}}, "metadata": map[string]any{"serialization_metadata": map[string]any{"render_format": "markdown"}}}}}})
			case "/backend-api/conversation/dil":
				_ = json.NewEncoder(w).Encode(dilSource.Load())
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		}))
		defer server.Close()
		if err := chromedp.Run(tab, chromedp.Navigate(server.URL+"/c/fixture")); err != nil {
			t.Fatal(err)
		}
		text, err := originalUserText(tab, "fixture-user")
		if err != nil || strings.Join(strings.Fields(text), " ") != `<wire> *literal* https://example.com "quoted"` {
			t.Fatal("declared markdown did not decode exactly", err)
		}
		messageID.Store("different-node")
		if _, err := originalUserText(tab, "fixture-user"); err == nil {
			t.Fatal("mismatched backend identity accepted")
		}
		// Captured DIL null renderer: headings and duplicate search IDs are
		// present, but the finished wire response was consumed as invalid tags.
		if err := chromedp.Run(tab, chromedp.Navigate(server.URL+"/c/dil")); err != nil {
			t.Fatal(err)
		}
		raw := "<opencode_bridge_call>\n{\"name\":\"echo\",\"arguments\":{\"text\":\"a\\\\b_*`\"}}\n</opencode_bridge_call>"
		m := map[string]any{"id": "dil-answer", "author": map[string]any{"role": "assistant"}, "channel": "final", "recipient": "all", "status": "finished_successfully", "end_turn": true, "content": map[string]any{"content_type": "text", "parts": []string{raw}}, "metadata": map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "DIL.render(__dil.jsx(()=>{return null;}));", "fallbackMarkdown": "", "recoveryDiagnostics": []any{map[string]any{"code": "invalid_tag_syntax", "action": "recovered_parse"}}}}}
		node := map[string]any{"parent": "fixture-user", "message": m}
		mapping := map[string]any{"dil-answer": node, "fixture-user": map[string]any{"message": map[string]any{"id": "fixture-user", "author": map[string]any{"role": "user"}}}}
		publish := func() { dilSource.Store(map[string]any{"mapping": mapping}) }
		publish()
		dilHTML := old + `<div data-talvt-turn-state="complete"><div><h4>You said:</h4><div data-chatgpt-search-message-ids="fixture-user"><p>request</p></div></div><div data-chatgpt-search-message-ids="dil-answer dil-answer"><h4 data-conversation-role="assistant">ChatGPT said:</h4><div><div class="MarkdownRoot-rZKhxa"><div hidden></div><div class="DilRenderer-tB76Jj"><div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"></div></div></div></div></div></div><h4>Latest response</h4><div>FOOTER</div>`
		set(t, dilHTML)
		// A pre-send baseline needs identities only. Prior protocol source may
		// be offline; capturing it must never fetch or validate the old answer.
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__baselineFetch=window.fetch;window.__baselineReads=0;window.fetch=()=>{window.__baselineReads++;return Promise.reject(new Error('offline'));}`, nil)); err != nil {
			t.Fatal(err)
		}
		ids, baselineErr := responseBaseline(tab)
		var baselineReads int
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__baselineReads`, &baselineReads), chromedp.Evaluate(`window.fetch=window.__baselineFetch;delete window.__baselineFetch`, nil)); err != nil {
			t.Fatal(err)
		}
		if baselineErr != nil || baselineReads != 0 || !strings.Contains(strings.Join(ids, ","), "dil-answer") {
			t.Fatalf("baseline fetched prior source: ids=%v reads=%d err=%v", ids, baselineReads, baselineErr)
		}
		got, err := responseProbe(tab, before.IDs)
		if err != nil || got.Last != raw || !got.Complete || got.LastID != "dil-answer" {
			t.Fatalf("bound null-DIL wire changed: %+v %v", got, err)
		}
		// Transient auth/network failures must retry exact source, never accept
		// the renderer's escaped tool arguments instead.
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__retryFetch=window.fetch;window.__retryReads=0;window.fetch=(...args)=>{if(String(args[0]).includes('/api/auth/session')&&window.__retryReads++===0)return Promise.reject(new Error('temporary'));return window.__retryFetch(...args);}`, nil)); err != nil {
			t.Fatal(err)
		}
		got, err = responseProbe(tab, before.IDs)
		var retryReads int
		if restoreErr := chromedp.Run(tab, chromedp.Evaluate(`window.__retryReads`, &retryReads), chromedp.Evaluate(`window.fetch=window.__retryFetch`, nil)); restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if err != nil || got.Last != raw || !got.SourceVerified || retryReads != 2 {
			t.Fatalf("transient exact-source recovery failed: verified=%v reads=%d err=%v", got.SourceVerified, retryReads, err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil || corr.UserID != "fixture-user" {
			t.Fatal("actual sibling user identity lost", corr, err)
		}
		got, err = responseProbe(tab, []string{"dil-answer"})
		if err != nil || got.Last != "" || got.Fresh {
			t.Fatal("baseline source answer leaked", got, err)
		}
		for field, wrong := range map[string]any{"id": "another-answer", "channel": "analysis", "recipient": "tool", "status": "in_progress", "end_turn": false} {
			valid := m[field]
			m[field] = wrong
			publish()
			got, err = responseProbe(tab, before.IDs)
			if err != nil || got.Last != "" {
				t.Fatalf("unsafe source %s accepted: %+v %v", field, got, err)
			}
			m[field] = valid
		}
		node["parent"] = "different-user"
		publish()
		got, err = responseProbe(tab, before.IDs)
		if err != nil || got.Last != "" {
			t.Fatal("unbound source adopted", got, err)
		}
		node["parent"] = "fixture-user"
		publish()
		for _, body := range []string{old + `<h4>You said:</h4><div>request</div>`, old + `<h4>You said:</h4><div>request</div><div data-talvt-turn-state="complete"><h4>ChatGPT said:</h4><div><div data-thinking>ONLY REASONING</div></div></div>`} {
			set(t, body)
			got, err = responseProbe(tab, before.IDs)
			if err != nil || got.Last != "" {
				t.Fatal("no assistant/reasoning became answer", got, err)
			}
		}
		set(t, dilHTML+`<button data-testid="stop-button">Stop</button>`)
		got, err = responseProbe(tab, before.IDs)
		if err != nil || got.Last != "" || !got.StopBtn {
			t.Fatal("generating source adopted", got, err)
		}
		set(t, strings.Replace(dilHTML, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"></div>`, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"><p>NEW COMPLETE ANSWER</p></div>`, 1))
		got, err = responseProbe(tab, before.IDs)
		if err != nil || got.Last != "NEW COMPLETE ANSWER" || !got.Complete {
			t.Fatal("actual rendered answer overridden", got, err)
		}
		// Real mixed-commentary DIL shape: rendered JSON has escaped Markdown
		// and collapsed whitespace; only the final source preserves arguments.
		mixed := "I will call the declared function.\n\n" + raw + "\n\nNo tools have run yet."
		m["content"] = map[string]any{"content_type": "text", "parts": []string{mixed}}
		m["metadata"] = map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "not executed", "fallbackMarkdown": mixed}}
		publish()
		visibleHTML := strings.Replace(dilHTML, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"></div>`, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"><p>I will call the declared function.</p><p>`+html.EscapeString(raw)+`</p><p>No tools have run yet.</p></div>`, 1)
		set(t, visibleHTML)
		got, err = responseProbe(tab, before.IDs)
		if err != nil || !got.SourceRequired || got.Last != mixed || got.LastID != "dil-answer" {
			t.Fatal("mixed DIL protocol source was transformed", err)
		}
		// Actual ec597 renderer: final assistant owner has a stable message ID
		// but no data-dil-message-id anywhere. Bind its full source, not the
		// Markdown-escaped visual JSON; ancestry/status validation still applies.
		plainHTML := strings.Replace(visibleHTML, ` data-dil-message-id="dil-answer"`, "", 1)
		m["metadata"] = map[string]any{"is_complete": true}
		publish()
		set(t, plainHTML)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != mixed || !got.SourceRequired {
			t.Fatal("non-DIL DOM-bound protocol source lost", err)
		}
		emptyPlain := strings.Replace(dilHTML, ` data-dil-message-id="dil-answer"`, "", 1)
		m["content"] = map[string]any{"content_type": "text", "parts": []string{"<escape>" + raw + "</escape>"}}
		publish()
		set(t, emptyPlain)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != "<escape>"+raw+"</escape>" {
			t.Fatal("completed non-DIL empty rendering lost exact protocol source", err)
		}
		// Real audit: hydration completed during the fourteen-second source
		// fetch. IDs/status/user ancestry stayed fixed but visual text changed.
		for _, change := range []struct {
			name, js string
			safe     bool
		}{
			{"hydrate same final", `document.querySelector('.DilResponseRoot-HfQrEh').textContent=` + quoteJS(raw), true},
			{"replace user identity", `document.querySelector('[data-chatgpt-search-message-ids="fixture-user"]').setAttribute('data-chatgpt-search-message-ids','different-user')`, false},
			{"replace assistant identity", `document.querySelector('[data-chatgpt-search-message-ids="dil-answer dil-answer"]').setAttribute('data-chatgpt-search-message-ids','different-answer')`, false},
		} {
			set(t, emptyPlain)
			js := `window.__auditFetch=window.fetch;window.fetch=async(...args)=>{const r=await window.__auditFetch(...args);if(String(args[0]).includes('/backend-api/conversation/')){` + change.js + `}return r;};`
			if err := chromedp.Run(tab, chromedp.Evaluate(js, nil)); err != nil {
				t.Fatal(err)
			}
			got, err := responseProbe(tab, before.IDs)
			if restoreErr := chromedp.Run(tab, chromedp.Evaluate(`window.fetch=window.__auditFetch;delete window.__auditFetch`, nil)); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if change.safe && (err != nil || got.Last != "<escape>"+raw+"</escape>") {
				t.Fatal("same final hydration rejected", err)
			}
			if !change.safe && got.Last != "" {
				t.Fatal("changed identity adopted during source read", change.name, err)
			}
		}
		m["content"] = map[string]any{"content_type": "text", "parts": []string{"ordinary prose"}}
		publish()
		set(t, emptyPlain)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != "" {
			t.Fatal("non-DIL empty rendering adopted unverified prose", err)
		}
		// Actual 2026-10-17 history import: completed final source contains
		// exactly ACK, but its non-DIL renderer shows an empty answer.
		m["content"] = map[string]any{"content_type": "text", "parts": []string{"ACK"}}
		publish()
		set(t, emptyPlain)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != "ACK" || !got.SourceVerified {
			t.Fatal("exact completed import acknowledgement was lost", err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__ackFetch=window.fetch;window.__ackReads=0;window.fetch=(...a)=>{if(String(a[0]).includes('/backend-api/conversation/'))window.__ackReads++;return window.__ackFetch(...a)}`, nil)); err != nil {
			t.Fatal(err)
		}
		text, _, waitErr := (&Client{}).waitForAnswer(context.Background(), tab, time.Second, time.Millisecond, nil, before.IDs, nil, false)
		var reads int
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__ackReads`, &reads), chromedp.Evaluate(`window.fetch=window.__ackFetch;delete window.__ackFetch;delete window.__ackReads`, nil)); err != nil {
			t.Fatal(err)
		}
		if waitErr != nil || text != "ACK" || reads != 1 {
			t.Fatal("completed source was redundantly fetched for stability polls", waitErr, text, reads)
		}
		// Captured race: the outer probe saw zero bytes; by the source
		// readback's recheck the SAME final ACK had hydrated. The former
		// nonempty-prose guard returned empty again, preserving stale state.
		hydratedHTML := strings.Replace(emptyPlain, `<div class="DilResponseRoot-HfQrEh"></div>`, `<div class="DilResponseRoot-HfQrEh">ACK</div>`, 1)
		set(t, hydratedHTML)
		var hydratedACK string
		baselineJSON, _ := json.Marshal(before.IDs)
		readACK := fmt.Sprintf(`(async()=>{const recheck=()=>%s(%s);const st=recheck();%s})()`, responseProbeJS, string(baselineJSON), nullDILWireJS)
		if err := chromedp.Run(tab, chromedp.Evaluate(readACK, &hydratedACK, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || hydratedACK != "ACK" {
			t.Fatal("empty-to-visible ACK readback race lost exact final", err, hydratedACK)
		}
		// The real hydration also mounts a nonempty DIL root whose fallback
		// is ACK, not the narrowly handled null-code protocol failure.
		set(t, strings.Replace(dilHTML, `<div class="DilResponseRoot-HfQrEh"></div>`, `<div class="DilResponseRoot-HfQrEh">ACK</div>`, 1))
		if err := chromedp.Run(tab, chromedp.Evaluate(readACK, &hydratedACK, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || hydratedACK != "ACK" {
			t.Fatal("hydrated DIL ACK incorrectly required null-render diagnostics", err, hydratedACK)
		}
		node["parent"] = "different-user"
		publish()
		if err := chromedp.Run(tab, chromedp.Evaluate(readACK, &hydratedACK, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || hydratedACK != "" {
			t.Fatal("hydrated ACK bypassed source ancestry", err)
		}
		set(t, emptyPlain)
		if got, _ := responseProbe(tab, before.IDs); got.Last != "" {
			t.Fatal("unrelated acknowledgement adopted")
		}
		node["parent"] = "fixture-user"
		m["content"] = map[string]any{"content_type": "text", "parts": []string{mixed}}
		publish()
		set(t, plainHTML)
		node["parent"] = "different-user"
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("non-DIL wrong user ancestry accepted")
		}
		node["parent"] = "fixture-user"
		m["status"] = "in_progress"
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("non-DIL unfinished source accepted")
		}
		m["status"] = "finished_successfully"
		m["metadata"] = map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "not executed", "fallbackMarkdown": mixed}}
		publish()
		set(t, visibleHTML)
		artifactSource := "<escape></escape>\n" + raw
		m["content"] = map[string]any{"content_type": "text", "parts": []string{artifactSource}}
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("source fallback reintroduced escape artifact")
		}
		literalSource := `<opencode_bridge_call>{"name":"literal","arguments":{"value":"<escape>"}}</opencode_bridge_call>`
		m["content"] = map[string]any{"content_type": "text", "parts": []string{literalSource}}
		publish()
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != literalSource {
			t.Fatal("source tool string altered", err)
		}
		m["content"] = map[string]any{"content_type": "text", "parts": []string{mixed}}
		publish()
		for field, wrong := range map[string]any{"id": "another-answer", "channel": "analysis", "recipient": "tool", "status": "in_progress", "end_turn": false} {
			valid := m[field]
			m[field] = wrong
			publish()
			if _, err := responseProbe(tab, before.IDs); err == nil {
				t.Fatalf("visible unsafe source %s fell back to rendered arguments", field)
			}
			m[field] = valid
		}
		node["parent"] = "different-user"
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("visible unbound source accepted")
		}
		node["parent"] = "fixture-user"
		publish()
		// Actual October 14 post-tool follow-up: prior call is valid but the
		// fresh final source is genuinely empty, with only a reasoning recap.
		m["content"] = map[string]any{"content_type": "text", "parts": []string{""}}
		m["metadata"] = map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "return null;", "fallbackMarkdown": ""}}
		mapping["recap"] = map[string]any{"parent": "fixture-user", "message": map[string]any{"id": "recap", "author": map[string]any{"role": "assistant"}, "content": map[string]any{"content_type": "reasoning_recap"}}}
		node["parent"] = "recap"
		mapping["prior-call"] = map[string]any{"message": map[string]any{"id": "prior-call", "content": map[string]any{"content_type": "text", "parts": []string{raw}}}}
		publish()
		set(t, dilHTML)
		emptyProof := func() bool {
			t.Helper()
			var empty bool
			js := "(async () => {const uid = 'fixture-user', id = 'dil-answer'; const recheck = () => " + responseProbeJS + "([]); " + completedEmptyJS + "})()"
			if err := chromedp.Run(tab, chromedp.Evaluate(js, &empty, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
				t.Fatal(err)
			}
			return empty
		}
		if !emptyProof() {
			t.Fatal("exact finished empty follow-up not proved")
		}
		for field, wrong := range map[string]any{"id": "prior-call", "channel": "analysis", "recipient": "tool", "status": "in_progress", "end_turn": false} {
			valid := m[field]
			m[field] = wrong
			publish()
			if emptyProof() {
				t.Fatalf("unsafe completed-empty %s accepted", field)
			}
			m[field] = valid
		}
		for field, wrong := range map[string]any{
			"author":   map[string]any{"role": "user"},
			"metadata": map[string]any{"is_complete": false},
			"content":  map[string]any{"content_type": "text", "parts": []string{raw}},
		} {
			valid := m[field]
			m[field] = wrong
			publish()
			if emptyProof() {
				t.Fatalf("wrong completed-empty %s accepted", field)
			}
			m[field] = valid
		}
		node["parent"] = "different-user"
		publish()
		if emptyProof() {
			t.Fatal("wrong empty ancestry accepted")
		}
		node["parent"] = "recap"
		publish()
		for _, html := range []string{dilHTML + `<button data-testid="stop-button">Stop</button>`, dilHTML + `<div role="alert">Something went wrong</div>`, strings.Replace(dilHTML, `fixture-user`, `different-user`, 1), old + `<h4>You said:</h4><div>new result</div>`} {
			set(t, html)
			if emptyProof() {
				t.Fatal("changed/generating/stale boundary accepted")
			}
		}
	})
	sub("stored markdown keeps every literal newline in the raw source", func(t *testing.T, tab context.Context) {
		// Byte-faithful shape of a live adoption failure (rendered 1B short,
		// single divergent span, full re-convergence): a `*` starting a line
		// mid-paragraph, two blank lines, then a `<`-led line with no closing
		// bracket (literal paragraph text, never a tag).
		stored := "lead line\n*\n\n\n<SUFFIX tail without bracket\ntrail line"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/auth/session":
				_, _ = w.Write([]byte(`{"accessToken":"fixture-token"}`))
			case "/backend-api/conversation/fixture":
				_ = json.NewEncoder(w).Encode(map[string]any{"mapping": map[string]any{"fixture-user": map[string]any{"message": map[string]any{"id": "fixture-user", "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{stored}}, "metadata": map[string]any{"serialization_metadata": map[string]any{"render_format": "markdown"}}}}}})
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		}))
		defer server.Close()
		if err := chromedp.Run(tab, chromedp.Navigate(server.URL+"/c/fixture")); err != nil {
			t.Fatal(err)
		}
		raw, markdown, err := fetchUserSource(tab, "fixture-user")
		if err != nil || !markdown || raw != stored {
			t.Fatalf("raw stored source lost bytes: err=%v markdown=%v len=%d want %d", err, markdown, len(raw), len(stored))
		}
		decoded, err := originalUserText(tab, "fixture-user")
		if err != nil {
			t.Fatal(err)
		}
		// Mechanism proof: the declared Markdown rendering drops exactly one
		// \n for the double blank line; everything after re-converges.
		want := normalizeRecoveryPrompt(stored)
		decodedNorm := normalizeRecoveryPrompt(decoded)
		if len(want)-len(decodedNorm) != 1 {
			t.Fatalf("expected exactly one dropped newline, decoded %dB from stored %dB", len(decoded), len(stored))
		}
		off := firstDiffByte(decodedNorm, want)
		if off >= len(want) || want[off] != '\n' || decodedNorm[off:] != want[off+1:] {
			t.Fatalf("decoded rendering did not lose exactly one interior newline at %d", off)
		}
		// The fixed comparison binds the turn via the raw bytes.
		if ok, provenance := matchUserSource(normalizeRecoveryPrompt(raw), decodedNorm, want); !ok || provenance != "raw" {
			t.Fatalf("raw-exact turn refused: ok=%v provenance=%q", ok, provenance)
		}
		// A turn with a different middle is still refused: no collapsing,
		// no tolerance, on either comparison path.
		tampered := strings.Replace(stored, "<SUFFIX", "<TUFFIX", 1)
		if ok, _ := matchUserSource(normalizeRecoveryPrompt(tampered), decodedNorm, want); ok {
			t.Fatal("tampered middle accepted")
		}
		// A turn with one more blank line than submitted is still refused.
		padded := strings.Replace(stored, "\n\n\n<", "\n\n\n\n<", 1)
		if ok, _ := matchUserSource(normalizeRecoveryPrompt(padded), normalizeRecoveryPrompt(padded), want); ok {
			t.Fatal("extra blank line accepted")
		}
	})
}

func TestFinishedEmptyBoundAndGenerationGrace(t *testing.T) {
	now := time.Unix(100, 0)
	w := emptyAnswerWatch{Started: now}
	if w.expired(responseState{StopBtn: true}, now) {
		t.Fatal("active generation failed")
	}
	if w.expired(responseState{}, now.Add(time.Second)) {
		t.Fatal("no rendering grace")
	}
	if !w.expired(responseState{}, now.Add(16*time.Second)) {
		t.Fatal("finished empty hung")
	}
	w = emptyAnswerWatch{Started: now}
	if w.expired(responseState{}, now.Add(29*time.Second)) {
		t.Fatal("startup grace missing")
	}
	w.expired(responseState{}, now.Add(30*time.Second))
	if !w.expired(responseState{}, now.Add(45*time.Second)) {
		t.Fatal("missing start hangs indefinitely")
	}
	w = emptyAnswerWatch{Started: now}
	w.expired(responseState{Complete: true}, now)
	if w.expired(responseState{Last: "answer"}, now.Add(16*time.Second)) {
		t.Fatal("completed answer failed")
	}
}

func quoteJS(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRasterImageValidation(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	if !ValidRasterImage(png) {
		t.Fatal("valid PNG data URL rejected")
	}
	gif := "data:image/gif;base64,R0lGODdhAQABAPAAAP8AAAAAACwAAAAAAQABAAACAkQBADs="
	if ValidRasterImage(gif) {
		t.Fatal("non-PNG media accepted")
	}
	if ValidRasterImage("data:image/png;base64,!!!not-base64!!!") {
		t.Fatal("undecodable base64 accepted")
	}
	if ValidRasterImage("data:image/png;base64,") {
		t.Fatal("empty payload accepted")
	}
	// JPEG bytes under a PNG prefix must not pass the signature check.
	jpeg := "data:image/png;base64,/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMv/wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAv/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/8QAFQEBAQAAAAAAAAAAAAAAAAAAAAX/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIRAxEAPwCwAA8A/9k="
	if ValidRasterImage(jpeg) {
		t.Fatal("non-PNG bytes accepted")
	}
	big := "data:image/png;base64," + strings.Repeat("iVBORw0KGgo", 700000)
	if ValidRasterImage(big) {
		t.Fatal("oversized payload accepted")
	}
}

func TestRecoveredSnapshotThreadCarriesImages(t *testing.T) {
	// gateThreadAnswer is what ReadThreadTurn returns; the images captured from
	// the recovered DOM must survive into the returned TurnResult, otherwise the
	// adoption/auto-resync paths serve the "[SVG: ...]" text fallback while a
	// live turn of the same answer serves a PNG.
	img := RasterImage{Alt: "diagram", DataURL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="}
	snap := threadSnapshot{
		answerNext: true, answers: 1,
		visible: responseState{Last: "here is the diagram", Complete: true},
		images:  []RasterImage{img},
	}
	got, err := gateThreadAnswer("https://chatgpt.com/c/x", snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 1 || got.Images[0].DataURL != img.DataURL {
		t.Fatalf("adopted snapshot dropped images: %+v", got)
	}
}

func TestSanitizeRasterAlt(t *testing.T) {
	if got := sanitizeRasterAlt("Elimination; rotation 4\neliminated"); got != "Elimination; rotation 4 eliminated" {
		t.Fatalf("newline not folded: %q", got)
	}
	if got := sanitizeRasterAlt("a]b[c"); got != "abc" {
		t.Fatalf("brackets leaked: %q", got)
	}
	if got := sanitizeRasterAlt("   "); got != "diagram" {
		t.Fatalf("empty alt has no fallback: %q", got)
	}
	if got := sanitizeRasterAlt(strings.Repeat("x", 200)); len([]rune(got)) != 100 {
		t.Fatalf("alt not capped: %d runes", len([]rune(got)))
	}
}

// Draft ownership decides whether the bridge clears leftover composer text or
// refuses. Only empty text, this exact prompt, or a character prefix of it is
// ours. A composer holding our own stale first line was reported as foreign
// text, which left every later attempt on that thread permanently refused.
// TestPreSendFailureClassification pins what askLocked's defer does with a
// pre-send error: a typed draft refusal keeps its own class, because retrying
// cannot help and must never look like a safe resubmission. A transient
// pre-submit failure stays retryable.
func TestPreSendFailureClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		attempted bool
		retryable bool
	}{
		{"foreign draft is not retryable", fmt.Errorf("%w: text present", ErrForeignDraft), false, false},
		{"owned draft is not retryable", fmt.Errorf("%w: turn may exist", ErrDraftOwned), false, false},
		{"no composer is not retryable", fmt.Errorf("%w: %w", ErrNotSubmitted, ErrNoComposer), false, true},
		{"transport failure stays retryable", errors.New("cdp blip"), false, true},
		{"attempted turn is never reclassified", fmt.Errorf("%w: click", ErrForeignDraft), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPreSendFailure(tc.err, tc.attempted)
			if errors.Is(got, ErrNotSubmitted) != tc.retryable {
				t.Fatalf("retryable=%v want %v: %v", errors.Is(got, ErrNotSubmitted), tc.retryable, got)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("original error lost: %v", got)
			}
		})
	}
}

// A truncated digest must never authorize an overwrite. Ownership is decided on
// the full digest (and, for ChatGPT, on exact logical equality), so the
// log-sized form must stay a different, shorter value.
func TestDraftDigestsAreNotTruncated(t *testing.T) {
	doc := "some document text"
	if got := draftFingerprint(doc); len(got) != 64 {
		t.Fatalf("ownership digest is %d hex chars, want 64", len(got))
	}
	if got := draftFingerprintShort(doc); len(got) != 8 {
		t.Fatalf("log digest is %d hex chars, want 8", len(got))
	}
	if draftFingerprintShort(doc) == draftFingerprint(doc) {
		t.Fatal("short and ownership digests must not be interchangeable")
	}
}

// The insertion mechanism follows prompt content, not only size: one native
// insert lands only the text before the first newline, so a multi-line prompt
// must never ride it. Size still decides when to keep the legacy paste path.
func TestComposerInsertModeFollowsContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want insertMode
	}{
		{"single line", "one line", insertNative},
		{"two lines", "one\ntwo", insertLinewise},
		{"trailing newline only", "one\n", insertLinewise},
		{"blank first line", "\none", insertLinewise},
		{"oversized single line", strings.Repeat("x", composerInlineLimit+1), insertLegacy},
		{"oversized multi line", strings.Repeat("x", composerInlineLimit) + "\n" + strings.Repeat("y", composerInlineLimit), insertLegacy},
	} {
		if got := insertModeFor(tc.text); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// A composer-less project page must be classified before anything touches
// the browser again. Only a blank loaded page (document ready, empty body,
// no composer, no login control, no challenge) may see a single reload:
// nothing was typed yet, so no submit and no draft is at risk. Auth,
// challenge, loading and unknown shapes must never trigger hidden
// navigation.
func TestClassifyMissingComposer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ps          pageLoadState
		cause       missingComposerCause
		recoverable bool
	}{
		{"blank loaded page may reload once", pageLoadState{ReadyComplete: true}, missingComposerBlank, true},
		{"rendered body without composer is unknown", pageLoadState{ReadyComplete: true, BodyTextLen: 1200}, missingComposerUnknown, false},
		{"loading page is unavailable", pageLoadState{}, missingComposerLoading, false},
		{"ready page still hydrating is unavailable", pageLoadState{BodyTextLen: 300}, missingComposerLoading, false},
		{"login button is auth, never reload", pageLoadState{ReadyComplete: true, Login: true}, missingComposerAuth, false},
		{"challenge is never reloaded", pageLoadState{ReadyComplete: true, Challenge: true}, missingComposerChallenge, false},
		{"login wins over challenge", pageLoadState{ReadyComplete: true, Login: true, Challenge: true}, missingComposerAuth, false},
		{"blank body with composer is not this path", pageLoadState{ReadyComplete: true, Composer: true}, missingComposerUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause, recoverable := classifyMissingComposer(tc.ps)
			if cause != tc.cause || recoverable != tc.recoverable {
				t.Fatalf("got (%v,%v) want (%v,%v)", cause, recoverable, tc.cause, tc.recoverable)
			}
		})
	}
}

// A single pre-submit reload must not risk a draft: only a missing record
// or an already-fenced (submitted) one allows it. An unsubmitted own draft
// refuses, so its page text can never be wiped by recovery.
func TestBlankReloadNeedsNoUnsubmittedDraft(t *testing.T) {
	if !blankReloadAllowed(nil) {
		t.Fatal("missing draft record must allow the reload")
	}
	if !blankReloadAllowed(&draftRecord{Submitted: true}) {
		t.Fatal("fenced submitted draft must allow the reload")
	}
	if blankReloadAllowed(&draftRecord{Doc: "typed", Hash: draftFingerprint("typed")}) {
		t.Fatal("unsubmitted draft must refuse the reload")
	}
}

// The missing-composer errors keep their pre-submit identity through
// askLocked's defer: a challenge is pre-submit but must never look like a
// safe retry, while a blank-page failure stays safe-retryable. Either way
// the original error survives, and a post-submit turn is never reclassified.
func TestMissingComposerErrorsStayPreSubmit(t *testing.T) {
	if !IsPreSubmit(ErrPageChallenge) {
		t.Fatal("challenge must be pre-submit")
	}
	if got := classifyPreSendFailure(fmt.Errorf("%w: captcha", ErrPageChallenge), false); errors.Is(got, ErrNotSubmitted) {
		t.Fatalf("challenge must not look like a safe retry: %v", got)
	}
	if got := classifyPreSendFailure(ErrBlankProjectPage, false); !errors.Is(got, ErrNotSubmitted) {
		t.Fatalf("blank-page failure must stay safe-retryable pre-submit: %v", got)
	}
	for _, err := range []error{ErrPageChallenge, ErrBlankProjectPage} {
		if got := classifyPreSendFailure(err, true); !errors.Is(got, err) {
			t.Fatalf("post-submit turn reclassified: %v", got)
		}
	}
}

// A canceled caller context surfaces without probing or reloading: no CDP,
// no navigation, no duplicate send.
func TestBlankRecoveryHonorsCanceledContext(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.recoverBlankProjectPage(ctx, nil, ErrNoComposer); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context must surface, got %v", err)
	}
}

func TestProbePageLoadStateReadsOnlyShape(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("Chrome unavailable for page-state probe fixture")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	root, cancelRoot := chromedp.NewContext(alloc)
	defer cancelRoot()
	ctx, cancelTab := context.WithTimeout(root, 60*time.Second)
	defer cancelTab()
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	ps, err := c.probePageLoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.ReadyComplete || ps.BodyTextLen != 0 || ps.Composer || ps.Login || ps.Challenge {
		t.Fatalf("blank fixture misread: %+v", ps)
	}
	if cause, ok := classifyMissingComposer(ps); cause != missingComposerBlank || !ok {
		t.Fatalf("blank fixture not recoverable: %v %v", cause, ok)
	}
	set := `<div data-testid="login-button">Log in</div><div id="prompt-textarea"></div>`
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML = `+quoteJS(set), nil)); err != nil {
		t.Fatal(err)
	}
	ps, err = c.probePageLoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Login || !ps.Composer {
		t.Fatalf("populated fixture misread: %+v", ps)
	}
	if cause, ok := classifyMissingComposer(ps); cause != missingComposerAuth || ok {
		t.Fatalf("login fixture must be auth without recovery: %v %v", cause, ok)
	}
	// In-thread verification error label (observed live as an error bubble
	// reading "reason: verify_challenge" instead of an assistant reply).
	// The mid-turn empty-poll probe must recognize it as a challenge.
	set = `<div>Something went wrong<br>reason: verify_challenge<br>Retry</div>`
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML = `+quoteJS(set), nil)); err != nil {
		t.Fatal(err)
	}
	ps, err = c.probePageLoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Challenge {
		t.Fatalf("verify_challenge label fixture misread: %+v", ps)
	}
	if cause, ok := classifyMissingComposer(ps); cause != missingComposerChallenge || ok {
		t.Fatalf("challenge-label fixture must be challenge without recovery: %v %v", cause, ok)
	}
	// In-thread error-bubble variant observed live (automation profile,
	// manual temporary chat): a bare `cloudflare_challenge` error with a
	// Retry button, plus 403s on backend-api fetches. Same gate, third
	// wording — must classify identically.
	set = `<div><span>cloudflare_challenge</span><button>Retry</button></div>`
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML = `+quoteJS(set), nil)); err != nil {
		t.Fatal(err)
	}
	ps, err = c.probePageLoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Challenge {
		t.Fatalf("cloudflare_challenge bubble fixture misread: %+v", ps)
	}
	// Interstitial-title variant: blank body, no widget DOM yet, but the
	// document title already reads "Just a moment...". Must classify as
	// challenge without waiting on body text.
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML = ''; document.title = 'Just a moment...'`, nil)); err != nil {
		t.Fatal(err)
	}
	ps, err = c.probePageLoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Challenge {
		t.Fatalf("interstitial-title fixture misread: %+v", ps)
	}
	if cause, ok := classifyMissingComposer(ps); cause != missingComposerChallenge || ok {
		t.Fatalf("interstitial-title fixture must be challenge without recovery: %v %v", cause, ok)
	}
}

// composerFixtureJS reproduces the measured behaviour of ChatGPT's ProseMirror
// composer on a dedicated bridge tab: the deprecated execCommand wipe pair does
// not clear it, a trusted deletion key event does (when deletesHonoured), one
// native insert lands only the text up to the FIRST newline, a trusted
// Shift+Enter breaks the line, and the logical document — not innerText — is
// authoritative.
func composerFixtureJS(initial string, deletesHonoured bool, capUnits int) string {
	return `(() => {
  const e = document.querySelector('.ProseMirror');
  let model = ` + quoteJS(initial) + `;
  window.__composerCap = ` + fmt.Sprintf("%d", capUnits) + `;
  window.__composerCapOneShot = false;
  const cap = () => { if (window.__composerCap > 0 && model.length > window.__composerCap) { model = model.slice(0, window.__composerCap); if (window.__composerCapOneShot) window.__composerCap = 0; } };
  const sync = () => { e.pmViewDesc.node.content.size = model.length; };
  e.pmViewDesc = { node: { type: { name: 'doc' }, content: { size: 0 }, textBetween() {
    const text = model;
    if (window.__composerReadsUntilEdit > 0 && --window.__composerReadsUntilEdit === 0) {
      queueMicrotask(() => window.__composerEdit('human replacement'));
    }
    return text;
  } } };
  e.pmViewDesc.view = {
    get state() { return { doc: e.pmViewDesc.node, tr: { delete() { return { clear: true }; } } }; },
    dispatch(tr) {
      if (!tr.clear) return;
      window.__composerDeletes++;
      if (` + fmt.Sprintf("%v", deletesHonoured) + `) { model = ''; e.textContent = ''; sync(); }
    }
  };
  e.textContent = model;
  sync();
  window.__composerModel = () => model;
  window.__composerEdit = text => { model = text; e.textContent = text; sync(); };
  window.__composerDeletes = 0;
  e.addEventListener('input', ev => {
    const t = ev.data == null ? '' : String(ev.data);
    if (ev.inputType === 'insertText') {
      const nl = t.indexOf('\n');
      model += nl < 0 ? t : t.slice(0, nl);
    } else if (ev.inputType === 'insertLineBreak' || ev.inputType === 'insertParagraph') {
      model += '\n';
    } else if (ev.inputType.indexOf('delete') === 0) {
      window.__composerDeletes++;
      if (` + fmt.Sprintf("%v", deletesHonoured) + `) model = '';
    }
    cap();
    sync();
  });
  const exec = document.execCommand.bind(document);
  document.execCommand = (cmd, ...rest) => {
    if (cmd === 'selectAll' || cmd === 'delete') return false;
    if (cmd === 'insertText' && rest.length > 1 && rest[1] != null) {
      // Deterministic legacy insertion: the editor model absorbs exactly what
      // the command carried here (a real oversized execCommand insertion is
      // unreliable), then the composer limit truncates it exactly as the
      // reported 180955-byte prompt was truncated.
      model += String(rest[1]);
      cap();
      sync();
      e.textContent = model;
      return true;
    }
    return exec(cmd, ...rest);
  };
})()`
}

func TestComposerInsertionAndDraftOwnership(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("Chrome unavailable for composer fixtures")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	tab, cancelTab := chromedp.NewContext(alloc)
	defer cancelTab()
	tab, timeout := context.WithTimeout(tab, 120*time.Second)
	defer timeout()
	setCap := func(initial string, deletesHonoured bool, capUnits int) {
		t.Helper()
		if err := chromedp.Run(tab, chromedp.Evaluate(`document.body.innerHTML = '<div class="ProseMirror" contenteditable="true" role="textbox"></div>'`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(composerFixtureJS(initial, deletesHonoured, capUnits), nil)); err != nil {
			t.Fatal(err)
		}
	}
	set := func(initial string, deletesHonoured bool) { setCap(initial, deletesHonoured, 0) }
	model := func(when string) string {
		t.Helper()
		var got string
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__composerModel()`, &got)); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		return got
	}
	deletes := func() int {
		t.Helper()
		var got int
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__composerDeletes`, &got)); err != nil {
			t.Fatal(err)
		}
		return got
	}
	c := &Client{}
	multi := "line one\nline two\n\nline four"
	t.Run("edit between ownership check and wipe is preserved", func(t *testing.T) {
		set("", true)
		if err := c.typePrompt(tab, tab, "owned residual"); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__composerEdit('human replacement')`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := c.wipeComposer(tab, composerSelector); !errors.Is(err, ErrForeignDraft) {
			t.Fatalf("changed draft was not refused: %v", err)
		}
		if model(t.Name()) != "human replacement" || deletes() != 0 {
			t.Fatal("the intervening edit was erased")
		}
	})
	t.Run("multi-line prompt lands complete", func(t *testing.T) {
		set("", true)
		if err := c.typePrompt(tab, tab, multi); err != nil {
			t.Fatalf("multi-line prompt not delivered: %v", err)
		}
		if got := model(t.Name()); got != multi {
			t.Fatalf("composer holds %q, want %q", got, multi)
		}
	})
	t.Run("unicode urls markdown and punctuation survive exactly", func(t *testing.T) {
		set("", true)
		rich := "Ünïcode ✅ 日本語 — em dash\nmarkdown **bold** and `code` and [link](https://example.com/a)\nsee <https://example.com/x?y=1> and (https://example.com/z)\ntrailing: !?.,;: % $"
		if err := c.typePrompt(tab, tab, rich); err != nil {
			t.Fatalf("rich prompt not delivered: %v", err)
		}
		if got := model(t.Name()); got != rich {
			t.Fatalf("composer holds %q, want %q", got, rich)
		}
	})
	t.Run("single line prompt still lands", func(t *testing.T) {
		set("", true)
		if err := c.typePrompt(tab, tab, "one line only"); err != nil {
			t.Fatalf("single line prompt not delivered: %v", err)
		}
		if got := model(t.Name()); got != "one line only" {
			t.Fatalf("composer holds %q", got)
		}
	})
	t.Run("typing never refuses and never deletes", func(t *testing.T) {
		set("notes the user typed earlier", true)
		if err := (&Client{}).typePrompt(tab, tab, multi); err != nil {
			t.Fatalf("fire-and-forget typing refused: %v", err)
		}
		if got := model(t.Name()); !strings.HasSuffix(got, multi) {
			t.Fatalf("typed prompt did not land after existing text: %q", got)
		}
		if got := deletes(); got != 0 {
			t.Fatalf("typing deleted composer text: %d deletions", got)
		}
	})
}

// TestDecideDraftOwnership pins the ownership table without a browser: every
// branch that can erase or preserve text is decided here.
func TestDecideDraftOwnership(t *testing.T) {
	const prompt = "the prompt we typed"
	rec := &draftRecord{
		Doc:    prompt,
		Prompt: prompt,
		Hash:   draftFingerprint(prompt),
		Units:  utf16Len(prompt),
		Target: "T1",
		Thread: "https://chatgpt.com/c/abc",
	}
	// A residual from a FAILED insertion: what landed is not what was meant.
	residual := "the prompt"
	residualRec := &draftRecord{
		Doc:    residual,
		Prompt: prompt,
		Hash:   draftFingerprint(residual),
		Target: "T1",
		Thread: rec.Thread,
	}
	own := composerDoc{Found: true, Logical: true, Text: prompt}
	cases := []struct {
		name   string
		doc    composerDoc
		rec    *draftRecord
		prompt string
		target string
		thread string
		want   draftAction
	}{
		{"empty composer types", composerDoc{Found: true, Logical: true}, rec, prompt, "T1", rec.Thread, draftTypeComposer},
		{"missing composer refuses", composerDoc{}, rec, prompt, "T1", rec.Thread, draftRefuseForeign},
		{"no record refuses", own, nil, prompt, "T1", rec.Thread, draftRefuseForeign},
		{"non-logical read refuses", composerDoc{Found: true, Text: prompt}, rec, prompt, "T1", rec.Thread, draftRefuseForeign},
		{"same turn exact reuses", own, rec, prompt, "T1", rec.Thread, draftReuse},
		{"superseded same thread replaces", own, rec, "a different request", "T1", rec.Thread, draftReplace},
		{"different target refuses", own, rec, "a different request", "T2", rec.Thread, draftRefuseForeign},
		{"different thread refuses", own, rec, "a different request", "T1", "https://chatgpt.com/c/other", draftRefuseForeign},
		{"attempted send refuses", own, &draftRecord{Doc: prompt, Prompt: prompt, Hash: rec.Hash, Target: "T1", Thread: rec.Thread, Submitted: true}, "a different request", "T1", rec.Thread, draftRefuseOwned},
		{"user edit refuses", composerDoc{Found: true, Logical: true, Text: prompt + " plus"}, rec, "a different request", "T1", rec.Thread, draftRefuseForeign},
		// Deleting a once-owned draft down to a PREFIX is a user edit: the
		// document no longer equals the observed snapshot, so it is preserved.
		{"user shortened to prefix refuses", composerDoc{Found: true, Logical: true, Text: "the prompt"}, rec, prompt, "T1", rec.Thread, draftRefuseForeign},
		// The real user bug: insertion A failed, leaving a residual that is not
		// the intended prompt; a later DIFFERENT request B may replace exactly
		// that observed residual.
		{"failed insertion residual is replaceable by a different request", composerDoc{Found: true, Logical: true, Text: residual}, residualRec, "a different request", "T1", rec.Thread, draftReplace},
		{"failed insertion residual retries the same request", composerDoc{Found: true, Logical: true, Text: residual}, residualRec, prompt, "T1", rec.Thread, draftReplace},
		// A truncated digest is not ownership: a record carrying one is rejected
		// even when Doc and the document agree.
		{"short hash is not ownership", composerDoc{Found: true, Logical: true, Text: residual}, &draftRecord{Doc: residual, Prompt: prompt, Hash: draftFingerprint(residual)[:8], Target: "T1", Thread: rec.Thread}, "a different request", "T1", rec.Thread, draftRefuseForeign},
		// A record whose snapshot and digest disagree is not evidence.
		{"inconsistent record refuses", composerDoc{Found: true, Logical: true, Text: residual}, &draftRecord{Doc: residual, Prompt: prompt, Hash: draftFingerprint(prompt), Target: "T1", Thread: rec.Thread}, "a different request", "T1", rec.Thread, draftRefuseForeign},
		{"prefix of a different prompt is not ownership", composerDoc{Found: true, Logical: true, Text: "the prompt"}, rec, "the prompt we typed at length", "T1", rec.Thread, draftRefuseForeign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideDraft(tc.doc, tc.rec, tc.prompt, tc.target, tc.thread); got != tc.want {
				t.Fatalf("decideDraft = %v, want %v", got, tc.want)
			}
		})
	}
}

// Two sessions must never share a target, a composer or a draft record. Here one
// session's oversized insertion truncates and fails while its peer types a small
// prompt: the peer must land untouched, must not wait for the slow failure, and
// the failed session's residual must stay bound to its OWN target so a later
// retry can safely replace it.
func TestConcurrentComposerIsolation(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("Chrome unavailable for composer fixtures")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	// One browser, two tabs: a second allocator context would try to launch a
	// second Chrome against the same profile and fail.
	root, cancelRoot := chromedp.NewContext(alloc)
	defer cancelRoot()
	// Start the browser once, on the root context, before any child context can
	// try to launch its own instance against the same profile.
	if err := chromedp.Run(root, chromedp.Evaluate(`1`, nil)); err != nil {
		t.Fatal(err)
	}
	open := func() (context.Context, context.CancelFunc) {
		ctx, cancel := chromedp.NewContext(root)
		ctx, timeout := context.WithTimeout(ctx, 120*time.Second)
		return ctx, func() { timeout(); cancel() }
	}
	tabA, closeA := open()
	defer closeA()
	tabB, closeB := open()
	defer closeB()
	// Distinct conversation identity, as two real chats have. Two tabs of one
	// chat still differ by target, but the record binds both.
	if err := chromedp.Run(tabB, chromedp.Navigate("about:blank#peer")); err != nil {
		t.Fatal(err)
	}

	install := func(ctx context.Context, initial string, capUnits int, deletesHonoured bool) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML = '<div class="ProseMirror" contenteditable="true" role="textbox"></div>'`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(composerFixtureJS(initial, deletesHonoured, capUnits), nil)); err != nil {
			t.Fatal(err)
		}
	}
	model := func(ctx context.Context, when string) string {
		t.Helper()
		var got string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__composerModel()`, &got)); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		return got
	}
	deletes := func(ctx context.Context) int {
		t.Helper()
		var got int
		if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__composerDeletes`, &got)); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The exact reported shape: a 180955-byte single-line prompt, above the
	// inline limit, so it takes the legacy one-shot insertion path; the composer
	// keeps only the first 12 units.
	huge := strings.Repeat("q", 180955)
	install(tabA, "", 12, true)
	install(tabB, "", 0, true)
	// Target identity only exists once each context has created its tab.
	if string(chromedp.FromContext(tabA).Target.TargetID) == string(chromedp.FromContext(tabB).Target.TargetID) {
		t.Fatal("independent sessions were given the same browser target")
	}

	cA, cB := &Client{}, &Client{}
	start := time.Now()
	errA := make(chan error, 1)
	go func() { errA <- cA.typePrompt(context.Background(), tabA, huge) }()
	errB := cB.typePrompt(context.Background(), tabB, "peer prompt")
	elapsedB := time.Since(start)
	var gotA error
	select {
	case gotA = <-errA:
	case <-time.After(90 * time.Second):
		t.Fatal("oversized legacy insertion never returned")
	}
	elapsedA := time.Since(start)

	// The peer succeeded and was never serialized behind the slow failure.
	if errB != nil {
		t.Fatalf("peer insertion failed while a different session was failing: %v", errB)
	}
	if elapsedB > 10*time.Second || elapsedB >= elapsedA {
		t.Fatalf("peer waited for the failing peer: peer=%v failing=%v", elapsedB, elapsedA)
	}
	if got := model(tabB, "peer model"); got != "peer prompt" {
		t.Fatalf("peer composer = %q, want %q", got, "peer prompt")
	}
	// Fire and forget: a partial insertion is still a completed typing step.
	if gotA != nil {
		t.Fatalf("oversized insertion = %v, want nil", gotA)
	}
	if got := model(tabA, "residual model"); got != strings.Repeat("q", 12) {
		t.Fatalf("residual = %d units, want 12", utf16Len(got))
	}
	if n := deletes(tabA) + deletes(tabB); n != 0 {
		t.Fatalf("failed insertion wiped a composer (deletes=%d)", n)
	}

	// The peer now types a LARGE prompt on the very path that just failed for
	// session A. The legacy one-shot insertion must succeed on an uncapped
	// composer, and it must not disturb session A's residual.
	large := strings.Repeat("z", 120000)
	if err := cB.typePrompt(context.Background(), tabB, large); err != nil {
		t.Fatalf("peer large legacy insertion failed: %v", err)
	}
	if got := model(tabB, "peer large"); !strings.HasSuffix(got, large) {
		t.Fatalf("peer large composer does not end with the new prompt: %d units", utf16Len(got))
	}
	if got := model(tabA, "residual after peer large"); got != strings.Repeat("q", 12) {
		t.Fatalf("peer activity changed the failing session's residual: %d units", utf16Len(got))
	}
	if n := deletes(tabA); n != 0 {
		t.Fatalf("peer activity wiped the failing session's composer (deletes=%d)", n)
	}

	// Existing composer text is not verified, cleared, or refused: the new
	// prompt lands after it.
	install(tabB, "small stale", 0, true)
	if err := cB.typePrompt(context.Background(), tabB, "peer prompt two"); err != nil {
		t.Fatalf("typing after existing text refused: %v", err)
	}
	if got := model(tabB, "stale model"); !strings.HasSuffix(got, "peer prompt two") {
		t.Fatalf("new prompt did not land after existing text: %q", got)
	}
	if n := deletes(tabB); n != 0 {
		t.Fatalf("existing text was wiped (deletes=%d)", n)
	}
}

func TestComposerTextKeyAndBridgeEnvelope(t *testing.T) {
	// The editor may add, merge or drop line breaks; the same words must still
	// compare equal, while any changed word must not.
	if a, b := composerTextKey("alpha.\n\nbeta"), composerTextKey("alpha.beta"); a != b {
		t.Fatalf("line-break reshape was not treated as equal: %q != %q", a, b)
	}
	if a, b := composerTextKey("alpha beta"), composerTextKey("alpha gamma"); a == b {
		t.Fatal("different words compared equal")
	}
	envelope := "CURRENT REQUEST\nConversation messages for this turn (JSON):\n[]"
	if !looksLikeBridgeDraft(envelope) {
		t.Fatal("bridge envelope not recognised")
	}
	if looksLikeBridgeDraft("please summarise this thread") ||
		looksLikeBridgeDraft("CURRENT REQUEST only") ||
		looksLikeBridgeDraft("Conversation messages for this turn only") {
		t.Fatal("ordinary draft misclassified as bridge envelope")
	}
}

// TestBubbleOnlyDivergenceWaitsForExactSource pins the adoption classification:
// a rendered bubble without its DOM message ID cannot prove a prompt mismatch,
// while the authoritative ID-bound source still can.
func TestBubbleOnlyDivergenceWaitsForExactSource(t *testing.T) {
	prompt := "the 26543 byte pending prompt"
	cases := []struct {
		name     string
		srcPath  string
		userText string
		expected []string
		want     bool
	}{
		{"unhydrated empty bubble", "bubble", "", []string{prompt}, true},
		{"collapsed bubble", "bubble", "the 26543", []string{prompt}, true},
		{"matching bubble", "bubble", prompt, []string{prompt}, false},
		{"authoritative mapping mismatch", "mapping", "edited", []string{prompt}, false},
		{"latest-turn read has nothing to compare", "bubble", "anything", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bubbleOnlyDivergence(tc.srcPath, tc.userText, tc.expected); got != tc.want {
				t.Fatalf("bubbleOnlyDivergence(%q, %q) = %v, want %v", tc.srcPath, tc.userText, got, tc.want)
			}
		})
	}
}

func TestMatchUserSourceExactBinding(t *testing.T) {
	want := "lead line\n*\n\n\n<SUFFIX tail without bracket\ntrail line"
	decodedDrop := "lead line\n*\n\n<SUFFIX tail without bracket\ntrail line"
	escapedWant := "literal * keep"
	cases := []struct {
		name       string
		raw        string
		decoded    string
		against    string
		ok         bool
		provenance string
	}{
		{"raw exact despite decoded newline loss", want, decodedDrop, want, true, "raw"},
		{"decoded exact with escaped raw store", `literal \* keep`, escapedWant, escapedWant, true, "decoded"},
		{"unmapped bubble with decoded match", "", want, want, true, "decoded"},
		{"both empty never matches", "", "", want, false, ""},
		{"unmapped bubble mismatch", "", "other", want, false, ""},
		{"tampered middle refused", strings.Replace(want, "<SUFFIX", "<TUFFIX", 1), decodedDrop, want, false, ""},
		{"extra blank line refused", strings.Replace(want, "\n\n\n<", "\n\n\n\n<", 1), decodedDrop, want, false, ""},
		{"missing blank line refused", decodedDrop, decodedDrop, want, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, provenance := matchUserSource(
				normalizeRecoveryPrompt(tc.raw),
				normalizeRecoveryPrompt(tc.decoded),
				normalizeRecoveryPrompt(tc.against),
			)
			if ok != tc.ok || provenance != tc.provenance {
				t.Fatalf("matchUserSource = (%v, %q), want (%v, %q)", ok, provenance, tc.ok, tc.provenance)
			}
		})
	}
}

// shadowFixture builds the measured /backend-api/f/conversation frame shapes
// with fabricated values. Field paths and operations mirror the live capture;
// only the scalars are invented.
func shadowFixture(prompt, answer string) []string {
	msg := func(id, role, parent, text string) string {
		return fmt.Sprintf(`{"p":"","o":"add","v":{"message":{"id":%q,"author":{"role":%q},"content":{"content_type":"text","parts":[%q]},"status":"finished_successfully","conversation_id":"conv-1","metadata":{"parent_id":%q}},"conversation_id":"conv-1","error":null}}`,
			id, role, text, parent)
	}
	return []string{
		msg("m-user", "user", "root", prompt),
		// Continuation frames omit p/o and re-send the whole mapping.
		fmt.Sprintf(`{"v":{"message":{"id":"m-rebase","author":{"role":"system"},"content":{"content_type":"text","parts":[""]},"conversation_id":"conv-1","metadata":{"parent_id":"m-user"}}},"c":1}`),
		msg("m-rebase2", "system", "m-rebase", ""),
		msg("m-answer", "assistant", "m-rebase2", answer),
		// The measured completion patch is a grouped list of operations.
		`{"o":"patch","v":[{"p":"/message/status","o":"replace","v":"finished_successfully"},{"p":"/message/metadata/is_complete","o":"replace","v":true},{"p":"/message/content/parts/0","o":"append","v":""}]}`,
		`{"type":"message_stream_complete","conversation_id":"conv-1"}`,
	}
}

func shadowStream(frames []string) string {
	blocks := make([]string, 0, len(frames))
	for _, f := range frames {
		blocks = append(blocks, "event: delta\ndata: "+f+"\n\n")
	}
	return strings.Join(blocks, "") + "data: [DONE]\n\n"
}

func shadowFeed(t *testing.T, s *shadowCapture, raw string, chunk int) {
	t.Helper()
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		s.feed([]byte(raw[i:end]))
	}
}

// TestShadowStreamReconstruction pins the measured protocol: the answer is
// rebuilt from whole-mapping frames plus grouped patches, and correlation
// accepts only a prompt-anchored user message whose lineage reaches the
// assistant. Off by default, and never a tool source.
func TestShadowCombinedPromptCorrelation(t *testing.T) {
	const prompt = "External client instructions\n{\"role\":\"system\",\"content\":\"context\"}\n{\"role\":\"user\",\"content\":\"latest question\"}\nProduce the next reply."
	const answer = `<opencode_bridge_call>{"name":"run","arguments":{"command":"pwd"}}</opencode_bridge_call>`
	s := newShadowCapture(prompt)
	shadowFeed(t, s, shadowStream(shadowFixture(prompt, answer)), 17)
	if ok, text, reason := s.result(); !ok || text != answer {
		t.Fatalf("submitted combined prompt did not correlate: ok=%v bytes=%d reason=%q", ok, len(text), reason)
	}
	// An identical final user line in a different submitted transcript is
	// not proof that this response belongs to the current browser turn.
	other := newShadowCapture(strings.Replace(prompt, "context", "different context", 1))
	shadowFeed(t, other, shadowStream(shadowFixture(prompt, answer)), 17)
	if ok, _, _ := other.result(); ok {
		t.Fatal("answer from a different combined prompt accepted")
	}
}

func TestShadowStreamReconstruction(t *testing.T) {
	const prompt = "PROMPT-TEXT"
	const answer = "exact answer \u00e9\u4e16"
	cases := []struct {
		name       string
		raw        string
		wantOK     bool
		wantText   string
		wantReason string
	}{
		{
			name:     "full reconstruction matches byte for byte",
			raw:      shadowStream(shadowFixture(prompt, answer)),
			wantOK:   true,
			wantText: answer,
		},
		{
			name: "typed and scalar frames are skipped, not fatal",
			// Measured live: the stream opens with `event: delta_encoding`
			// carrying the bare string "v1", and typed event frames carry no
			// p/o/v. Neither is protocol state, and neither may abort the read.
			raw: `event: delta_encoding` + "\n" +
				`data: "v1"` + "\n\n" +
				`event: message` + "\n" +
				`data: {"type":"resume_conversation_token","conversation_id":"conv-1"}` + "\n\n" +
				shadowStream(shadowFixture(prompt, answer)),
			wantOK:   true,
			wantText: answer,
		},
		{
			name:     "UTF-8 and frames split across chunks",
			raw:      shadowStream(shadowFixture(prompt, answer)),
			wantOK:   true,
			wantText: answer,
		},
		{
			name:       "no explicit completion is inconclusive even at EOF",
			raw:        strings.Join(strings.Split(shadowStream(shadowFixture(prompt, answer)), "message_stream_complete"), "x"),
			wantReason: "no explicit completion",
		},
		{
			name:       "different prompt cannot anchor",
			raw:        shadowStream(shadowFixture("SOMETHING-ELSE", answer)),
			wantReason: "no matching user message",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Alternate one-byte feeding to exercise rune and frame splits.
			chunk := 1
			if i%2 == 1 {
				chunk = 7
			}
			s := newShadowCapture(prompt)
			shadowFeed(t, s, tc.raw, chunk)
			ok, text, reason := s.result()
			if ok != tc.wantOK {
				t.Fatalf("correlated=%v reason=%q, want %v", ok, reason, tc.wantOK)
			}
			if tc.wantOK {
				if text != tc.wantText {
					t.Fatalf("reconstructed %q, want %q", text, tc.wantText)
				}
				return
			}
			if reason != tc.wantReason {
				t.Fatalf("reason=%q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestShadowStreamLineageAndLimits proves a single-hop parent check is not
// enough, that an unknown patch operation is refused rather than guessed, and
// that the capture stays bounded.
func TestShadowStreamLineageAndLimits(t *testing.T) {
	t.Run("lineage crosses rebase messages", func(t *testing.T) {
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(shadowFixture("P", "A")), 64)
		if ok, text, reason := s.result(); !ok || text != "A" {
			t.Fatalf("expected lineage through rebase messages, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("composer whitespace reshape still anchors", func(t *testing.T) {
		// The typed prompt used blank lines and indentation; the server stores
		// one line. Same words, same order, so the turn still correlates.
		s := newShadowCapture("P1\n\n   P2")
		shadowFeed(t, s, shadowStream(shadowFixture("P1 P2", "A")), 64)
		if ok, text, reason := s.result(); !ok || text != "A" {
			t.Fatalf("reshape broke the anchor: ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("a changed word does not anchor", func(t *testing.T) {
		s := newShadowCapture("P1\n\nP2")
		shadowFeed(t, s, shadowStream(shadowFixture("P1 P3", "A")), 64)
		if ok, _, reason := s.result(); ok || reason != "no matching user message" {
			t.Fatalf("ok=%v reason=%q, want no matching user message", ok, reason)
		}
	})

	t.Run("separate words never merge", func(t *testing.T) {
		if samePromptKey("a b") == samePromptKey("ab") {
			t.Fatal("whitespace normalization must not merge separate words")
		}
	})

	t.Run("late transport failure cannot downgrade a completed capture", func(t *testing.T) {
		// The browser aborts the SSE connection at teardown, after the server
		// has declared the stream complete.
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(shadowFixture("P", "A")), 64)
		s.fail("stream aborted")
		if ok, text, reason := s.result(); !ok || text != "A" {
			t.Fatalf("completed capture was downgraded: ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("turn exposes identity only for a confirmed turn", func(t *testing.T) {
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(shadowFixture("P", "A")), 64)
		thread, id, ok := s.turn()
		if !ok || thread != "conv-1" || id != "m-answer" {
			t.Fatalf("thread=%q id=%q ok=%v, want conv-1/m-answer", thread, id, ok)
		}
		other := newShadowCapture("SOMETHING-ELSE")
		shadowFeed(t, other, shadowStream(shadowFixture("P", "A")), 64)
		if _, _, ok := other.turn(); ok {
			t.Fatal("an uncorrelated capture must expose no identity")
		}
	})

	t.Run("delivery refuses a capture with no bridge call", func(t *testing.T) {
		s := newShadowCapture("PROMPT-TEXT")
		shadowFeed(t, s, shadowStream(shadowFixture("PROMPT-TEXT", "just prose")), 64)
		if text, ok := s.answer(); ok {
			t.Fatalf("delivered %q from a capture without an envelope", text)
		}
	})

	t.Run("delivery returns the reconstructed server text", func(t *testing.T) {
		const envelope = `<opencode_bridge_call>{"name":"shell"}</opencode_bridge_call>`
		s := newShadowCapture("PROMPT-TEXT")
		shadowFeed(t, s, shadowStream(shadowFixture("PROMPT-TEXT", envelope)), 64)
		text, ok := s.answer()
		if !ok || text != envelope {
			t.Fatalf("ok=%v text=%q, want the captured envelope", ok, text)
		}
	})

	t.Run("delivery refuses an incomplete capture", func(t *testing.T) {
		s := newShadowCapture("PROMPT-TEXT")
		frames := shadowFixture("PROMPT-TEXT", `<opencode_bridge_call>{"name":"shell"}</opencode_bridge_call>`)
		shadowFeed(t, s, shadowStream(frames[:len(frames)-1]), 64)
		if text, ok := s.answer(); ok {
			t.Fatalf("delivered %q without an explicit completion", text)
		}
	})

	t.Run("stream delivery keeps the protocol gates", func(t *testing.T) {
		if streamAnswerAcceptable("just prose, no envelope") {
			t.Fatal("prose must never satisfy the stream delivery gate")
		}
		if !streamAnswerAcceptable("<opencode_bridge_call>{\"name\":\"x\"}</opencode_bridge_call>") {
			t.Fatal("a bridge call envelope must pass")
		}
		if !streamAnswerAcceptable("prefix\n<opencode\\_bridge\\_call>{}") {
			t.Fatal("the escaped envelope spelling must pass")
		}
		if streamAnswerAcceptable("ok\n<escape></escape>\n") {
			t.Fatal("standalone escape artifacts must be refused")
		}
	})

	t.Run("answer comparison ignores only surrounding whitespace", func(t *testing.T) {
		if !sameAnswerText("code()\n", "code()") {
			t.Fatal("a trailing newline must not break the comparison")
		}
		if sameAnswerText("a  b", "a b") {
			t.Fatal("inner whitespace is part of the answer")
		}
		if sameAnswerText("A", "B") {
			t.Fatal("different answers compared equal")
		}
		if p, s := sharedEdges("SHADOW_OK", "SHADOW_OK\n"); p != 9 || s != 0 {
			t.Fatalf("prefix=%d suffix=%d, want 9/0", p, s)
		}
		if p, s := sharedEdges("xAB", "yAB"); p != 0 || s != 2 {
			t.Fatalf("prefix=%d suffix=%d, want 0/2", p, s)
		}
		if countNonASCII("aé") != 2 {
			t.Fatalf("nonascii count = %d, want 2 bytes", countNonASCII("aé"))
		}
	})

	t.Run("conversation falls back to the reconstructed root", func(t *testing.T) {
		// Live assistant messages do not always repeat conversation_id, so the
		// binding falls back to this turn's own messages and then the root.
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(shadowFixture("P", "A")), 64)
		for _, m := range s.messages {
			delete(m, "conversation_id")
		}
		s.state["conversation_id"] = "conv-1"
		if ok, text, reason := s.result(); !ok || text != "A" {
			t.Fatalf("root fallback failed: ok=%v reason=%q", ok, reason)
		}
		s.state["conversation_id"] = "other"
		if ok, _, reason := s.result(); ok || reason != "completion conversation mismatch" {
			t.Fatalf("ok=%v reason=%q, want completion conversation mismatch", ok, reason)
		}
	})

	t.Run("broken lineage is not correlated", func(t *testing.T) {
		frames := shadowFixture("P", "A")
		// Point the assistant at a message that never appears.
		frames[3] = strings.Replace(frames[3], `"parent_id":"m-rebase2"`, `"parent_id":"missing"`, 1)
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(frames), 64)
		if ok, _, reason := s.result(); ok || reason != "assistant lineage" {
			t.Fatalf("ok=%v reason=%q, want assistant lineage refusal", ok, reason)
		}
	})

	t.Run("unknown operation on the answer is unsupported", func(t *testing.T) {
		frames := shadowFixture("P", "A")
		frames = append(frames[:len(frames)-1],
			`{"p":"/message/content/parts/0","o":"move","v":"finished_successfully"}`,
			frames[len(frames)-1])
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(frames), 64)
		if ok, _, reason := s.result(); ok || reason != "unsupported patch" {
			t.Fatalf("ok=%v reason=%q, want unsupported patch", ok, reason)
		}
	})

	t.Run("remove of an inert metadata key is accepted", func(t *testing.T) {
		// Measured live: the server removes a metadata field that carries
		// no answer text, author, message ID or parent link. The
		// reconstruction accepts the removal and still correlates.
		frames := shadowFixture("P", "A")
		frames = append(frames[:len(frames)-1],
			`{"o":"patch","v":[{"p":"/message/metadata/tmp_flag","o":"add","v":true},{"p":"/message/metadata/tmp_flag","o":"remove"}]}`,
			frames[len(frames)-1])
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(frames), 64)
		ok, text, reason := s.result()
		if !ok || text != "A" {
			t.Fatalf("ok=%v text=%q reason=%q, want clean reconstruction", ok, text, reason)
		}
	})

	// One measured patch appends an object onto /message/metadata. That cannot
	// change the answer or its lineage, so it is skipped and counted rather
	// than refusing an otherwise exact reconstruction.
	t.Run("inert metadata patch is skipped", func(t *testing.T) {
		frames := shadowFixture("P", "A")
		frames = append(frames[:len(frames)-1],
			`{"p":"/message/metadata","o":"append","v":{"search_result_groups":[]}}`,
			frames[len(frames)-1])
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(frames), 64)
		ok, text, reason := s.result()
		if !ok || text != "A" {
			t.Fatalf("ok=%v text=%q reason=%q, want clean reconstruction", ok, text, reason)
		}
		if s.ignoredPatches != 1 {
			t.Fatalf("ignoredPatches=%d, want 1", s.ignoredPatches)
		}
	})

	t.Run("oversized stream stops at the byte limit", func(t *testing.T) {
		s := newShadowCapture("P")
		s.feed(bytes.Repeat([]byte("x"), shadowMaxBytes+1))
		if ok, _, reason := s.result(); ok || reason != "byte limit" {
			t.Fatalf("ok=%v reason=%q, want byte limit", ok, reason)
		}
	})

	t.Run("invalid frame is refused", func(t *testing.T) {
		s := newShadowCapture("P")
		s.feed([]byte("data: not-json\n\n"))
		if ok, _, reason := s.result(); ok || reason != "invalid frame" {
			t.Fatalf("ok=%v reason=%q, want invalid frame", ok, reason)
		}
	})

	t.Run("append grows the streamed part", func(t *testing.T) {
		frames := shadowFixture("P", "first")
		// Replace the no-op append with a real continuation of the same part.
		frames[len(frames)-2] = `{"o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":" second"}]}`
		s := newShadowCapture("P")
		shadowFeed(t, s, shadowStream(frames), 5)
		ok, text, reason := s.result()
		if !ok || text != "first second" {
			t.Fatalf("ok=%v text=%q reason=%q, want appended text", ok, text, reason)
		}
	})

	t.Run("concurrent captures stay isolated", func(t *testing.T) {
		a := newShadowCapture("PROMPT-A")
		b := newShadowCapture("PROMPT-B")
		ra := shadowStream(shadowFixture("PROMPT-A", "answer-a"))
		rb := shadowStream(shadowFixture("PROMPT-B", "answer-b"))
		for i := 0; i < len(ra) || i < len(rb); i += 3 {
			if i < len(ra) {
				end := min(i+3, len(ra))
				a.feed([]byte(ra[i:end]))
			}
			if i < len(rb) {
				end := min(i+3, len(rb))
				b.feed([]byte(rb[i:end]))
			}
		}
		if ok, text, _ := a.result(); !ok || text != "answer-a" {
			t.Fatalf("capture A got ok=%v text=%q", ok, text)
		}
		if ok, text, _ := b.result(); !ok || text != "answer-b" {
			t.Fatalf("capture B got ok=%v text=%q", ok, text)
		}
	})
}

// TestStreamCaptureDefault pins the switch: the capture is the proof of a turn,
// so it runs unless it was explicitly turned off.
func TestStreamCaptureDefault(t *testing.T) {
	t.Setenv("CHATGPT_BRIDGE_SHADOW_STREAM", "")
	if !streamCaptureEnabled() {
		t.Fatal("stream capture must be on by default")
	}
	for _, v := range []string{"0", "false", "off", "no", " OFF "} {
		t.Setenv("CHATGPT_BRIDGE_SHADOW_STREAM", v)
		if streamCaptureEnabled() {
			t.Fatalf("%q did not turn the capture off", v)
		}
	}
}

func TestHasMermaidFence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  bool
	}{
		{"flowchart block", "Here:\n```mermaid\nflowchart TD\nA --> B\n```", true},
		{"sequence block", "```mermaid\nsequenceDiagram\nA->>B: hi\n```\nDone.", true},
		{"other fence", "```go\nfmt.Println(1)\n```", false},
		{"inline ticks", "use `mermaid` inline", false},
		{"indented fence", "  ```mermaid\nflowchart TD\n```", false},
		{"plain prose", "no code here", false},
	} {
		if got := hasMermaidFence(tc.reply); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// A long prompt is stored as the rendered message, not the typed bytes: the
// middle can change (markdown, links). Both ends must still identify it, or
// every long turn becomes unverifiable.
func TestShadowCorrelatesRenderedLongPrompt(t *testing.T) {
	answer := "the answer"
	head := strings.Repeat("A", 400)
	tail := strings.Repeat("B", 400)
	typed := head + "LINK" + tail
	stored := head + "LINK (docs at example dot com)" + tail

	s := newShadowCapture(typed)
	shadowFeed(t, s, shadowStream(shadowFixture(stored, answer)), 7)
	ok, text, reason := s.result()
	if !ok || text != answer {
		t.Fatalf("correlated=%v reason=%q text=%q", ok, reason, text)
	}
	if s.identity != identityWindow {
		t.Fatalf("identity=%q, want window", s.identity)
	}
}

// Tolerant matching still refuses a different message: same length, other tail.
func TestShadowWindowToleratesRenderingNotDifferentPrompt(t *testing.T) {
	head := strings.Repeat("A", 400)
	typed := head + "LINK" + strings.Repeat("B", 400)
	other := head + "LINK" + strings.Repeat("C", 400)

	s := newShadowCapture(typed)
	shadowFeed(t, s, shadowStream(shadowFixture(other, "the answer")), 7)
	if ok, _, reason := s.result(); ok || reason != "no matching user message" {
		t.Fatalf("correlated=%v reason=%q", ok, reason)
	}
}

// A disabled Send with the exact prompt still in the composer is a pre-send
// refusal, not an uncertain turn: recording it as uncertain wedged the chat
// after ChatGPT itself refused a 149635-byte paste.
func TestProvablyUnsubmitted(t *testing.T) {
	base := sendProof{Readable: true, ComposerExact: true, SendDisabled: true}
	if !provablyUnsubmitted(base) {
		t.Fatal("exact composer, no turn, disabled send must count as unsubmitted")
	}
	for name, p := range map[string]sendProof{
		"bubble appeared":  {Readable: true, ComposerExact: true, SendDisabled: true, UserBubble: true},
		"generating":       {Readable: true, ComposerExact: true, SendDisabled: true, Generating: true},
		"composer differs": {Readable: true, SendDisabled: true},
		"unreadable":       {ComposerExact: true, SendDisabled: true},
		"send enabled":     {Readable: true, ComposerExact: true},
	} {
		if provablyUnsubmitted(p) {
			t.Fatalf("%s must not count as unsubmitted", name)
		}
	}
}

// A mid-turn challenge names the verification, keeps the pending binding
// (never pre-submit), and never looks like a safe retry post-submit.
func TestChallengeMidTurnKeepsUncertainty(t *testing.T) {
	if IsPreSubmit(ErrChallengeMidTurn) {
		t.Fatal("mid-turn challenge after submission must not be pre-submit")
	}
	if got := classifyPreSendFailure(ErrChallengeMidTurn, true); !errors.Is(got, ErrChallengeMidTurn) || errors.Is(got, ErrNotSubmitted) {
		t.Fatalf("post-submit challenge reclassified: %v", got)
	}
	msg := ErrChallengeMidTurn.Error()
	if !strings.Contains(msg, "human-verification") || !strings.Contains(msg, "do not resend") {
		t.Fatalf("challenge error must name the gate and forbid resend: %q", msg)
	}
}

// A failed stored-source lookup names its step, stays content-free, and
// survives the handler's wrap for errors.As classification.
func TestUserSourceErrorNamesStep(t *testing.T) {
	wrapped := fmt.Errorf("recovery user source: %w", &UserSourceError{Reason: "message-shape"})
	var src *UserSourceError
	if !errors.As(wrapped, &src) || src.Reason != "message-shape" {
		t.Fatalf("typed reason lost through wrap: %v", wrapped)
	}
	if strings.Contains(wrapped.Error(), "0123456789") || strings.Contains(wrapped.Error(), "parts") {
		t.Fatalf("source error must not leak content: %q", wrapped.Error())
	}
}
