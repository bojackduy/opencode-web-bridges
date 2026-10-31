package gemini

import (
	"strings"
	"testing"
)

func TestConvertEmpty(t *testing.T) {
	if got := ConvertResponseHTML(""); got != "" {
		t.Fatalf("empty fragment: got %q", got)
	}
	if got := ConvertResponseHTML("<model-response></model-response>"); got != "" {
		t.Fatalf("empty model-response: got %q", got)
	}
}

func TestCodeSourceAngleBracketsSurviveProseCleanup(t *testing.T) {
	for _, fragment := range []string{
		`<pre><code class="language-html"><span>&lt;div&gt;</span>source&lt;/div&gt;</code></pre>`,
		`<pre><code><span>&lt;div&gt;</span>source&lt;/div&gt;</code></pre>`,
		`<p>Use <code>&lt;div&gt;source&lt;/div&gt;</code>.</p>`,
	} {
		if got := ConvertResponseHTML(fragment); !strings.Contains(got, "<div>source</div>") {
			t.Fatalf("source markup was lost: %q", got)
		}
	}
}

func TestLiveGeminiCodeWrapperAfterDOMCleanup(t *testing.T) {
	// Live XML code-block content after clone removal of decoration/toolbars.
	fragment := `<div class="markdown"><code-block><pre><code><span>&lt;<span>opencode_bridge_call</span>&gt;</span>{"name":"lookup_marker","arguments":{"marker":"TEST-ONLY"}}<span>&lt;/<span>opencode_bridge_call</span>&gt;</span>` + "\n" + `</code></pre></code-block></div>`
	want := `<opencode_bridge_call>{"name":"lookup_marker","arguments":{"marker":"TEST-ONLY"}}</opencode_bridge_call>`
	if got := ConvertResponseHTML(fragment); got != want {
		t.Fatalf("protocol changed: %q", got)
	}
}

func TestConvertParagraphsAndHeadings(t *testing.T) {
	html := `<model-response><div class="model-response-text"><h2>Title</h2><p>Hello <strong>world</strong> and <em>moon</em>.</p></div></model-response>`
	got := ConvertResponseHTML(html)
	if !strings.Contains(got, "## Title") {
		t.Fatalf("missing heading: %q", got)
	}
	if !strings.Contains(got, "Hello **world** and *moon*.") {
		t.Fatalf("missing formatted paragraph: %q", got)
	}
}

func TestConvertCodeBlockWithLanguage(t *testing.T) {
	html := `<model-response><div class="markdown-container"><pre><code class="language-go">package main
</code></pre></div></model-response>`
	got := ConvertResponseHTML(html)
	if !strings.Contains(got, "```go") || !strings.Contains(got, "package main") {
		t.Fatalf("code block not converted: %q", got)
	}
}

func TestConvertListTableQuoteLink(t *testing.T) {
	html := `<model-response><div class="markdown-container"><ul><li>one</li><li>two</li></ul>` +
		`<blockquote><p>cite</p></blockquote>` +
		`<p>see <a href="https://example.com">docs</a></p>` +
		`<table><tr><th>H1</th><th>H2</th></tr><tr><td>a|b</td><td>c</td></tr></table>` +
		`</div></model-response>`
	got := ConvertResponseHTML(html)
	for _, want := range []string{"- one", "- two", "> cite", "[docs](https://example.com)", "| H1 | H2 |", `a\|b`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestConvertStripsButtons(t *testing.T) {
	html := `<model-response><div class="model-response-text"><p>answer</p><button>Copy code</button></div></model-response>`
	got := ConvertResponseHTML(html)
	if strings.Contains(got, "Copy code") {
		t.Fatalf("button text leaked: %q", got)
	}
	if !strings.Contains(got, "answer") {
		t.Fatalf("answer lost: %q", got)
	}
}

func TestWireProtocolPreserved(t *testing.T) {
	// In the real DOM the model's angle brackets arrive entity-escaped
	// inside outerHTML; stripTags must not eat them.
	raw := "&lt;opencode_bridge_call&gt;\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n&lt;/opencode_bridge_call&gt;"
	html := "<model-response><div class=\"model-response-text\"><p>" + raw + "</p></div></model-response>"
	got := ConvertResponseHTML(html)
	if !strings.Contains(got, "<opencode_bridge_call>") {
		t.Fatalf("wrapper not preserved: %q", got)
	}
	if strings.Contains(got, "**") || strings.Contains(got, "```") {
		t.Fatalf("wrapper was markdown-processed: %q", got)
	}
}

func TestACKPassthrough(t *testing.T) {
	html := `<model-response><div class="model-response-text"><p>ACK</p></div></model-response>`
	if got := ConvertResponseHTML(html); got != "ACK" {
		t.Fatalf("ACK not verbatim: %q", got)
	}
}

func TestStripTags(t *testing.T) {
	if got := stripTags(`<div>a &amp; b</div>`); got != "a & b" {
		t.Fatalf("stripTags: %q", got)
	}
}

func TestNormalizePromptCanonical(t *testing.T) {
	if NormalizePrompt("  hello\r\nworld\r ") != "hello\nworld" {
		t.Fatal("CRLF/CR normalize to LF and edges trim")
	}
	if NormalizePrompt("a  b") != "a  b" {
		t.Fatal("interior spacing is significant")
	}
}

func TestPromptHashLineage(t *testing.T) {
	// The sent-log and the adoption comparison share this hash: any two
	// strings equal after normalization must share lineage.
	if PromptHash("hello\r\nworld") != PromptHash("hello\nworld") {
		t.Fatal("normalization-equal prompts must share hash lineage")
	}
	if PromptHash("hello") == PromptHash("world") {
		t.Fatal("distinct prompts must hash distinctly")
	}
	if AnswerHash("done") == AnswerHash("done!") {
		t.Fatal("answer hashes must distinguish outcomes")
	}
}

func TestDiffSpansBoundedVsStructural(t *testing.T) {
	if got := DiffSpans("same text", "same text"); got != "" {
		t.Fatalf("identical inputs must have no spans: %q", got)
	}
	shared := strings.Repeat("0123456789abcdef", 4) // 64B anchor either side
	bounded := DiffSpans(shared+"SUFFIX"+shared, shared+"DIFFER"+shared)
	if bounded == "" || strings.Contains(bounded, "+rest") || strings.Contains(bounded, ",") {
		t.Fatalf("small difference must be one re-converging span: %q", bounded)
	}
	huge := DiffSpans(strings.Repeat("a", 9000), strings.Repeat("b", 9000))
	if !strings.Contains(huge, "+rest") {
		t.Fatalf("multi-KB divergence with no anchor must never re-converge: %q", huge)
	}
}

// ---------------------------------------------------------------------------
// Output image handling: detection, extraction, fallback, caps, and the
// text-only byte-identical invariant.
// ---------------------------------------------------------------------------

const testImagePayload = "data:image/png;base64," + "QUJDQUJDQUJDQUJDQUJDQUJD"

func TestTextOnlyByteIdentical(t *testing.T) {
	// Happy-path goldens: the image path must not alter text-only output.
	// (The heading/prose separator is the legacy single newline — valid
	// ATX, never run-together on one line — preserved byte-identically.)
	html := `<model-response><div class="model-response-text"><h2>Title</h2><p>Hello <strong>world</strong> and <em>moon</em>.</p></div></model-response>`
	if got := ConvertResponseHTML(html); got != "## Title\nHello **world** and *moon*." {
		t.Fatalf("text-only output changed: %q", got)
	}
	code := `<model-response><div class="markdown-container"><pre><code class="language-go">package main
</code></pre></div></model-response>`
	if got := ConvertResponseHTML(code); got != "```go\npackage main\n```" {
		t.Fatalf("code output changed: %q", got)
	}
	md, parts := ConvertResponseWithImages(html)
	if md != "## Title\nHello **world** and *moon*." || len(parts) != 0 {
		t.Fatalf("rich path diverged on text-only input: %q %+v", md, parts)
	}
	// No fixture markers may appear without image markup behind them.
	for _, f := range []string{html, code, `<p>plain [brackets] and *stars*</p>`} {
		if got := ConvertResponseHTML(f); strings.Contains(got, "[Image:") || strings.Contains(got, "[Canvas:") || strings.Contains(got, "[SVG:") {
			t.Fatalf("fixture leaked into text-only output: %q", got)
		}
	}
}

func TestImageTextReturnsBothParts(t *testing.T) {
	html := `<model-response><div class="markdown-container"><p>Here is your chart:</p><img alt="sales chart" src="` + testImagePayload + `"></div></model-response>`
	md, parts := ConvertResponseWithImages(html)
	if !strings.Contains(md, "Here is your chart:") {
		t.Fatalf("prose lost: %q", md)
	}
	if !strings.Contains(md, "[Image: sales chart]") {
		t.Fatalf("ChatGPT-consistent fixture missing: %q", md)
	}
	if len(parts) != 1 || parts[0].URL != testImagePayload || parts[0].Alt != "sales chart" {
		t.Fatalf("image part missing or wrong: %+v", parts)
	}
	// Legacy entry point keeps the fixture in the text.
	if got := ConvertResponseHTML(html); !strings.Contains(got, "[Image: sales chart]") {
		t.Fatalf("legacy text lost the fixture: %q", got)
	}
}

func TestImageOnlyAnswerSurvives(t *testing.T) {
	html := `<model-response><img alt="cat" src="` + testImagePayload + `"></model-response>`
	md, parts := ConvertResponseWithImages(html)
	if strings.TrimSpace(md) == "" {
		t.Fatal("image-only answer must not collapse to empty text")
	}
	if !strings.Contains(md, "[Image: cat]") {
		t.Fatalf("fixture missing: %q", md)
	}
	if len(parts) != 1 {
		t.Fatalf("image-only answer must carry its part: %+v", parts)
	}
}

func TestUnretrievableImageNeverFailsTurn(t *testing.T) {
	for _, html := range []string{
		// Model-emitted dangling reference, no image node at all.
		`<model-response><div class="markdown-container"><p>See [Image 1] above.</p></div></model-response>`,
		// Expired blob URL.
		`<model-response><p>Done</p><img alt="gen" src="blob:https://gemini.google.com/9f2c"></model-response>`,
		// Missing src entirely.
		`<model-response><p>Done</p><img alt="gen"></model-response>`,
	} {
		md, parts := ConvertResponseWithImages(html)
		if strings.TrimSpace(md) == "" {
			t.Fatalf("text must survive unretrievable images: %q", html)
		}
		if len(parts) != 0 {
			t.Fatalf("unretrievable images must yield no parts: %q %+v", html, parts)
		}
		if !strings.Contains(md, "not retrievable from web UI") {
			t.Fatalf("placeholder must be explained, never bare: %q", md)
		}
	}
	// The live symptom: a bare "[Image 1]" stub must never pass through.
	got := ConvertResponseHTML(`<model-response><p>[Image 1]</p></model-response>`)
	if got != "[Image 1: visual content not retrievable from web UI]" {
		t.Fatalf("dangling stub not annotated: %q", got)
	}
	// Real fixtures are not rewritten.
	keep := ConvertResponseHTML(`<model-response><p>Look:</p><img alt="cat" src="` + testImagePayload + `"></model-response>`)
	if strings.Contains(keep, "not retrievable") {
		t.Fatalf("retrievable fixture wrongly annotated: %q", keep)
	}
}

func TestRemoteAndPrivateImageSources(t *testing.T) {
	html := `<model-response><img alt="Comparison diagram" src="https://example.com/diagram.png"><img src="https://chatgpt.com/backend-api/files/x?token=PRIVATE"></model-response>`
	md, parts := ConvertResponseWithImages(html)
	if !strings.Contains(md, "[Image: Comparison diagram] [source](https://example.com/diagram.png)") {
		t.Fatalf("public source link missing: %q", md)
	}
	if strings.Contains(md, "PRIVATE") {
		t.Fatalf("authenticated visual URL leaked: %q", md)
	}
	if len(parts) != 0 {
		t.Fatalf("remote URLs are fixtures, not parts: %+v", parts)
	}
	// First-party session hosts never become portable links.
	for _, host := range []string{"https://gemini.google.com/app/img.png", "https://lh3.googleusercontent.com/x"} {
		got := ConvertResponseHTML(`<model-response><img alt="g" src="` + host + `"></model-response>`)
		if strings.Contains(got, "[source](") {
			t.Fatalf("session-host URL linked: %q", got)
		}
	}
}

func TestCanvasAndSVGFixtures(t *testing.T) {
	html := `<model-response><p>Sketch:</p><canvas></canvas><svg role="img"><title>Elimination</title><text>rotation 4 eliminated</text></svg></model-response>`
	md, parts := ConvertResponseWithImages(html)
	for _, want := range []string{
		"[Canvas: no accessible description; visual content not extracted]",
		"[SVG: Elimination; rotation 4 eliminated]",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in %q", want, md)
		}
	}
	if len(parts) != 0 {
		t.Fatalf("canvas/svg illustrations stay text-only like ChatGPT: %+v", parts)
	}
}

func TestOversizedImageSkippedWithNote(t *testing.T) {
	oldMax := MaxResponseImageBytes
	MaxResponseImageBytes = 10
	defer func() { MaxResponseImageBytes = oldMax }()
	html := `<model-response><p>Big:</p><img alt="big" src="` + testImagePayload + `"></model-response>`
	md, parts := ConvertResponseWithImages(html)
	if len(parts) != 0 {
		t.Fatalf("oversized image must not become a part: %+v", parts)
	}
	if !strings.Contains(md, "[Image: big]") || !strings.Contains(md, "1 image omitted: exceeds per-turn image budget") {
		t.Fatalf("oversized image needs fixture + omit-note: %q", md)
	}
}

func TestImageCountCapSkipsWithNote(t *testing.T) {
	oldMax := MaxResponseImages
	MaxResponseImages = 1
	defer func() { MaxResponseImages = oldMax }()
	html := `<model-response><img alt="one" src="` + testImagePayload + `"><img alt="two" src="` + testImagePayload + `"></model-response>`
	md, parts := ConvertResponseWithImages(html)
	if len(parts) != 1 || parts[0].Alt != "one" {
		t.Fatalf("first image wins the cap: %+v", parts)
	}
	if !strings.Contains(md, "1 image omitted: exceeds per-turn image budget") {
		t.Fatalf("over-count needs an omit-note: %q", md)
	}
}

func TestLiveOverridesResolveBlob(t *testing.T) {
	html := `<model-response><p>Done</p><img alt="gen" src="blob:https://gemini.google.com/9f2c"></model-response>`
	live := []liveImageSlot{{Alt: "gen", Src: "blob:https://gemini.google.com/9f2c", DataURL: testImagePayload}}
	md, parts := ConvertResponseWithLiveImages(html, live, nil)
	if len(parts) != 1 || parts[0].URL != testImagePayload {
		t.Fatalf("live blob resolution must yield a part: %+v", parts)
	}
	if strings.Contains(md, "not retrievable") {
		t.Fatalf("resolved image must drop the unretrievable stub: %q", md)
	}
	// Nil live slices behave exactly like the pure path.
	pure, pureParts := ConvertResponseWithImages(html)
	nilLive, nilParts := ConvertResponseWithLiveImages(html, nil, nil)
	if pure != nilLive || len(pureParts) != len(nilParts) {
		t.Fatalf("nil live must equal pure path: %q vs %q", pure, nilLive)
	}
}

func TestCollectorJSIsReadOnly(t *testing.T) {
	// The image collector runs inside the owned turn tab: it must only read
	// (attribute reads, canvas readback, same-document blob GET). Any
	// navigation, submission, or session-state write would violate the
	// read-only discipline the adapter promises.
	for _, forbidden := range []string{".click(", "Navigate", "submit(", "setText", "insertText", "location.href", "reload(", "querySelectorAll('button')"} {
		if strings.Contains(responseImagesJS, forbidden) {
			t.Fatalf("collector JS must stay read-only, found %q", forbidden)
		}
	}
	for _, want := range []string{"querySelectorAll('img')", "toDataURL", "currentSrc"} {
		if !strings.Contains(responseImagesJS, want) {
			t.Fatalf("collector must cover %q", want)
		}
	}
}

func TestDanglingRefsCaseInsensitive(t *testing.T) {
	// The model capitalizes inconsistently; every bare [Image N] shape must
	// resolve to the canonical inert note, never pass through bare.
	for _, in := range []string{`[Image 2]`, `[image 2]`, `[IMAGE 2]`} {
		got := ConvertResponseHTML(`<model-response><p>See ` + in + ` above.</p></model-response>`)
		if got != "See [Image 2: visual content not retrievable from web UI] above." {
			t.Fatalf("input %q gave %q", in, got)
		}
	}
	// Near-misses stay literal: plurals, lists, and our own fixtures.
	for _, in := range []string{`[images 12]`, `[Image: cat]`, `[Image 1: visual content not retrievable from web UI]`} {
		got := ConvertResponseHTML(`<model-response><p>` + in + `</p></model-response>`)
		if got != in {
			t.Fatalf("literal %q was rewritten to %q", in, got)
		}
	}
}

func TestLiteralMarkdownImageFallback(t *testing.T) {
	// Literal ![alt](blob:...) / ![alt]() surviving to the text names a
	// target the downstream client can never resolve: note, don't pass the
	// dangling URL through.
	got := ConvertResponseHTML(`<model-response><p>Chart: ![sales](blob:https://gemini.google.com/x) and ![empty]() done.</p></model-response>`)
	for _, want := range []string{
		"[Image: sales] [visual content not retrievable from web UI]",
		"[Image: empty] [visual content not retrievable from web UI]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "blob:") {
		t.Fatalf("unresolvable blob URL leaked: %q", got)
	}
	// Usable targets, ordinary links, and code examples pass through
	// byte-identically (no behavior change on the happy path).
	keep := ConvertResponseHTML(`<model-response><p>See ![pic](https://example.com/a.png) and [docs](https://example.com) plus ` + "`![x](blob:y)`" + `.</p></model-response>`)
	for _, want := range []string{`![pic](https://example.com/a.png)`, `[docs](https://example.com)`, "`![x](blob:y)`"} {
		if !strings.Contains(keep, want) {
			t.Fatalf("happy-path markdown altered, missing %q in %q", want, keep)
		}
	}
	fenced := ConvertResponseHTML("<model-response><pre><code class=\"language-md\">![a](blob:b)\n[Image 9]\n</code></pre></model-response>")
	if !strings.Contains(fenced, "![a](blob:b)") || !strings.Contains(fenced, "[Image 9]") {
		t.Fatalf("code examples must stay literal: %q", fenced)
	}
}

func TestMarkdownParityFencesAndHeadings(t *testing.T) {	// A code source containing its own fencing must not break the fence:
	// the fence grows (ChatGPT ticks parity) instead of terminating early.
	html := `<model-response><div class="markdown-container"><pre><code class="language-md">outer
` + "```" + `
inner
` + "```" + `
</code></pre></div></model-response>`
	got := ConvertResponseHTML(html)
	if !strings.Contains(got, "````md") || strings.Count(got, "````") < 2 {
		t.Fatalf("fence did not grow around nested fencing: %q", got)
	}
	// Literal double backticks in inline code grow the span fence too.
	inline := ConvertResponseHTML("<model-response><p>Use <code>a``b</code> here.</p></model-response>")
	if !strings.Contains(inline, "```a``b```") {
		t.Fatalf("inline fence did not grow: %q", inline)
	}
	// Single-backtick prose keeps the legacy shape byte-identically (no
	// over-escaping introduced by the extractor): literal backticks pass
	// through, so doubled backticks in a live turn are model-emitted, not
	// extractor artifacts.
	single := ConvertResponseHTML(`<model-response><p>Use <code>x` + "`" + `y</code> here.</p></model-response>`)
	if !strings.Contains(single, "`` x`y ``") {
		t.Fatalf("single-backtick span changed: %q", single)
	}
	// Headings terminate at the newline (valid ATX, never run-together on
	// one line); the legacy single-newline separator is preserved, not
	// reflowed, so Gemini output needs no ChatGPT-style heading rewrite.
	headed := ConvertResponseHTML(`<model-response><h1>Title</h1><p>Body text.</p></model-response>`)
	if headed != "# Title\n\nBody text." {
		t.Fatalf("heading/prose separation changed: %q", headed)
	}
}
