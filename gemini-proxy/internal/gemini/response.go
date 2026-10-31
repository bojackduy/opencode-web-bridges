// HTML-to-Markdown extraction for Gemini Web model answers.
//
// Gemini renders assistant answers as HTML inside <model-response> elements.
// This file converts that rendered DOM to GitHub-flavored Markdown, mirroring
// the contract of chatgpt-proxy's response extractor (same supported
// constructs, same wire-protocol preservation) while matching Gemini's DOM:
//
//   - primary scope: the newest <model-response> outerHTML (passed in by the
//     CDP client, which already scoped it past the last user message)
//   - rendered text: div.model-response-text / div.markdown-container
//   - code: <pre><code class="language-x"> plus Gemini's copy-code buttons
//     (removed before serialization, like ChatGPT's copy controls)
//
// Wire protocol preservation: if the extracted answer consists ONLY of one or
// more <opencode_bridge_call>{...}</opencode_bridge_call> wrappers, the raw
// text is returned unchanged — no Markdown processing, JSON whitespace and
// backslashes preserved byte-for-byte. The historical ACK acknowledgement is
// also returned verbatim.
package gemini

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// bridgeCallRe matches <opencode_bridge_call>...</opencode_bridge_call>
// wrappers (non-greedy, dot-all emulated with [\s\S]).
var bridgeCallRe = regexp.MustCompile(`(?s)<opencode_bridge_call>[\s\S]*?</opencode_bridge_call>`)

// incompleteBridgeRe spots a dangling bridge-call tag — an opener that
// never became a valid wrapper (broken JSON, mismatched close tag).
// Mirrors the chatgpt-proxy toolMarker tripwire.
var incompleteBridgeRe = regexp.MustCompile(`</?opencode_bridge_call`)

// tagRe strips any HTML tag.
var tagRe = regexp.MustCompile(`<[^>]*>`)

// wsRe collapses 3+ newlines to two.
var wsRe = regexp.MustCompile(`\n{3,}`)

// stripTags removes all HTML tags and unescapes entities. Used for
// emptiness checks and for plain-text fallbacks.
func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// ---------------------------------------------------------------------------
// Output image handling.
//
// A Gemini turn whose response contains a model-generated image must not
// arrive downstream as a bare "[Image 1]" placeholder: the reference dangles
// and the TUI shows an unexplained stub. The convention mirrors
// chatgpt-proxy's response extractor ([Image: ...] / [Canvas: ...] / [SVG: ...]
// alt-text fixtures with a portable [source](url) link only), extended one
// step: when the image bytes are retrievable from the DOM (data: URLs inline
// in outerHTML, or live-resolved by the CDP collector in gemini.go), they are
// also returned as ResponseImage parts so the adapter can emit proper OpenAI
// image_url content parts alongside the text.
//
// Caps (configurable, documented defaults): at most MaxResponseImages image
// parts per turn and at most MaxResponseImageBytes decoded bytes per image.
// Oversized or over-count images are skipped as parts but keep a fixture plus
// an omit-note in the text — the turn never fails for image reasons.
// ---------------------------------------------------------------------------

// MaxResponseImages caps the image_url parts emitted for one turn.
var MaxResponseImages = 4

// MaxResponseImageBytes caps the decoded bytes accepted for one image part.
var MaxResponseImageBytes = 750000

// ResponseImage is one retrievable output image: URL is a data: URL (inline
// bytes) or an https: URL suitable for an OpenAI image_url content part.
type ResponseImage struct {
	Alt  string `json:"alt,omitempty"`
	URL  string `json:"url"`
	Note string `json:"note,omitempty"`
}

// imageNode is one parsed image-bearing DOM node in document order.
type imageNode struct {
	kind string // "img" | "canvas" | "svg" | "background"
	alt  string
	src  string // raw src/href/data URL ("" when absent)
}

// imageMarkupRe gates the image path: fragments without any image marker take
// the legacy byte-identical extraction path.
var imageMarkupRe = regexp.MustCompile(`(?i)<img|<canvas|<svg|background-image|\[image`)

// danglingImageRefRe matches model-emitted bare references such as
// "[Image 1]", "[image 2]". Already-annotated fixtures ("[Image: ...]",
// "[Image 1: ...]") do not match: the closing bracket must follow the
// digits. Case-insensitive: the renderer/model capitalizes inconsistently,
// and the note is always emitted in canonical "[Image N: ...]" form.
var danglingImageRefRe = regexp.MustCompile(`(?i)\[image\s+(\d+)\]`)

// mdImageRe matches literal Markdown image syntax that survived to the
// extracted text (the renderer left it unconverted, e.g. inside prose the
// model emitted raw).
var mdImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]*)\)`)

// inlineCodeRe guards single-backtick spans during text-level rewrites.
var inlineCodeRe = regexp.MustCompile("`[^`\n]+`")

// codeGuard shields fenced blocks and inline code spans from text-level
// rewrites (fixtures/notes must never alter code examples).
type codeGuard struct {
	slots []string
}

func (g *codeGuard) protect(md string) string {
	g.slots = nil
	put := func(s string) string {
		g.slots = append(g.slots, s)
		return "\x00GD" + itoa(len(g.slots)-1) + "\x00"
	}
	s := fenceRe.ReplaceAllStringFunc(md, put)
	return inlineCodeRe.ReplaceAllStringFunc(s, put)
}

func (g *codeGuard) restore(s string) string {
	for i, p := range g.slots {
		s = strings.ReplaceAll(s, "\x00GD"+itoa(i)+"\x00", p)
	}
	return s
}

// unretrievableNote explains a dangling image reference that carries no
// retrievable bytes. It is inert text, never a new navigation or fetch.
func unretrievableNote(n string) string {
	return "[Image " + n + ": visual content not retrievable from web UI]"
}

// imgTagRe finds <img ...> elements (paired or void).
var imgTagRe = regexp.MustCompile(`(?i)<img[^>]*>`)

// canvasTagRe finds <canvas>...</canvas> and self-closed <canvas .../> nodes.
var canvasTagRe = regexp.MustCompile(`(?is)<canvas[^>]*(?:/>|>.*?</canvas>)`)

// svgTagRe finds inline <svg>...</svg> illustrations.
var svgTagRe = regexp.MustCompile(`(?is)<svg[^>]*>.*?</svg>`)

// backgroundImageRe finds style="...background-image:url(...)..." paint.
var backgroundImageRe = regexp.MustCompile(`(?i)background-image\s*:\s*url\(\s*["']?([^"')]+)["']?\s*\)`)

// attrRe parses one quoted HTML attribute.
var attrRe = regexp.MustCompile(`(?i)([\w-]+)\s*=\s*("[^"]*"|'[^']*')`)

// markdownSpecialRe escapes ChatGPT-style prose specials in alt text.
var markdownSpecialRe = regexp.MustCompile("[\\\\`*_\\[\\]~]")

// escapeAlt renders alt text inert inside an [Image: ...] fixture.
func escapeAlt(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	return markdownSpecialRe.ReplaceAllString(s, "\\$0")
}

// parseAttrs lower-cases attribute names; values are unquoted raw.
func parseAttrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		name := strings.ToLower(m[1])
		v := m[2]
		if len(v) >= 2 {
			v = v[1 : len(v)-1]
		}
		if _, ok := out[name]; !ok {
			out[name] = v
		}
	}
	return out
}

// imgAlt picks the accessible description, ChatGPT order: alt,
// aria-label, aria-labelledby targets are DOM-bound (unavailable in this
// pure pass, so skipped), then title.
func imgAlt(attrs map[string]string) string {
	if v := strings.TrimSpace(attrs["alt"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(attrs["aria-label"]); v != "" {
		return v
	}
	return strings.TrimSpace(attrs["title"])
}

// publicSourceLink returns a portable [source](url) suffix, or "" when the
// URL is not a safe portable artifact. Mirrors the ChatGPT extractor: only
// bare public https URLs (no credentials, query, or fragment, registrable
// host, never first-party hosts that require the viewer's session).
func publicSourceLink(src string) string {
	u, err := url.Parse(strings.TrimSpace(src))
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !strings.Contains(host, ".") || strings.ContainsAny(host, " <>()") {
		return ""
	}
	if isIPLiteral(host) || isLocalHost(host) {
		return ""
	}
	for _, first := range []string{"chatgpt.com", "openai.com", "oaiusercontent.com",
		"gemini.google.com", "google.com", "googleusercontent.com"} {
		if host == first || strings.HasSuffix(host, "."+first) {
			return ""
		}
	}
	if strings.ContainsAny(u.String(), " <>()") {
		return ""
	}
	return " [source](" + u.String() + ")"
}

func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func isLocalHost(host string) bool {
	return host == "localhost" || host == "local" || host == "internal" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal")
}

// dataURLByteSize returns the decoded byte size of a base64 data: URL, or
// -1 when src is not a base64 data URL.
func dataURLByteSize(src string) int {
	lower := strings.ToLower(src)
	if !strings.HasPrefix(lower, "data:") {
		return -1
	}
	semi := strings.Index(src, ";base64,")
	if semi < 0 {
		return -1
	}
	payload := src[semi+len(";base64,"):]
	if i := strings.IndexAny(payload, " \t\r\n\"'<>"); i >= 0 {
		payload = payload[:i]
	}
	// Length arithmetic avoids a full decode on the hot path.
	n := len(payload)
	if strings.HasSuffix(payload, "==") {
		n -= 2
	} else if strings.HasSuffix(payload, "=") {
		n -= 1
	}
	return n * 3 / 4
}

// isImageDataURL reports an inline image payload the bridge can carry.
func isImageDataURL(src string) bool {
	lower := strings.ToLower(strings.TrimSpace(src))
	return strings.HasPrefix(lower, "data:image/") && strings.Contains(lower, ";base64,")
}

// parseImageNodes lists image-bearing nodes in document order. Pure:
// attribute parsing only, no fetching.
func parseImageNodes(fragment string) []imageNode {
	var out []imageNode
	for _, m := range imgTagRe.FindAllString(fragment, -1) {
		attrs := parseAttrs(m)
		src := attrs["src"]
		if src == "" {
			src = attrs["data-src"]
		}
		out = append(out, imageNode{kind: "img", alt: imgAlt(attrs), src: strings.TrimSpace(src)})
	}
	for _, m := range canvasTagRe.FindAllString(fragment, -1) {
		attrs := parseAttrs(m)
		out = append(out, imageNode{kind: "canvas", alt: imgAlt(attrs)})
	}
	for _, m := range svgTagRe.FindAllString(fragment, -1) {
		desc := svgDescription(m)
		out = append(out, imageNode{kind: "svg", alt: desc})
	}
	for _, m := range backgroundImageRe.FindAllStringSubmatch(fragment, -1) {
		out = append(out, imageNode{kind: "background", src: strings.TrimSpace(m[1])})
	}
	return out
}

// svgDescription extracts title/desc/text captions like the ChatGPT pass,
// which joins them ("Elimination; rotation 4 eliminated").
func svgDescription(svg string) string {
	var parts []string
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`),
		regexp.MustCompile(`(?is)<desc[^>]*>(.*?)</desc>`),
	} {
		for _, m := range re.FindAllStringSubmatch(svg, -1) {
			if t := strings.TrimSpace(stripTags(m[1])); t != "" {
				parts = append(parts, t)
			}
		}
	}
	textRe := regexp.MustCompile(`(?is)<text[^>]*>(.*?)</text>`)
	for _, m := range textRe.FindAllStringSubmatch(svg, -1) {
		if t := strings.TrimSpace(stripTags(m[1])); t != "" {
			parts = append(parts, t)
		}
		// Cap: illustration captions, not full canvas dumps.
		if len(parts) >= 4 {
			break
		}
	}
	return strings.Join(parts, "; ")
}

// imageFixture renders one node's ChatGPT-consistent markdown fixture.
func imageFixture(n imageNode) string {
	switch n.kind {
	case "canvas":
		alt := escapeAlt(n.alt)
		if alt == "" {
			alt = "no accessible description; visual content not extracted"
		}
		return "[Canvas: " + alt + "]"
	case "svg":
		alt := escapeAlt(n.alt)
		if alt == "" {
			alt = "no accessible description; visual content not extracted"
		}
		return "[SVG: " + alt + "]"
	default: // img + background
		alt := escapeAlt(n.alt)
		if alt == "" {
			if n.kind == "background" {
				alt = "background image"
			} else {
				alt = "no accessible description; visual content not extracted"
			}
		}
		fix := "[Image: " + alt + "]"
		if n.src != "" && !isImageDataURL(n.src) {
			fix += publicSourceLink(n.src)
		}
		if n.src == "" || strings.HasPrefix(strings.ToLower(n.src), "blob:") {
			fix += " [visual content not retrievable from web UI]"
		}
		return fix
	}
}

// substituteImageFixtures replaces image-bearing DOM nodes with their inert
// markdown fixtures so the generic tag-stripping path below carries them as
// ordinary paragraphs. Fragments without image markup pass through
// byte-identically (plain stringContains fast path first).
func substituteImageFixtures(s string) string {
	return substituteImageFixturesWithLive(s, nil, nil)
}

// substituteImageFixturesWithLive applies read-only collected live payloads
// (same-document blob: resolution, canvas readback) before fixture rendering,
// so a live-resolved node renders its bytes-backed fixture instead of the
// unretrievable stub.
func substituteImageFixturesWithLive(s string, imgLive, canvasLive []liveImageSlot) string {
	if !imageMarkupRe.MatchString(s) {
		return s
	}
	nodes := applyLiveOverrides(parseImageNodes(s), imgLive, canvasLive)
	var imgs, canvases []imageNode
	for _, n := range nodes {
		switch n.kind {
		case "img":
			imgs = append(imgs, n)
		case "canvas":
			canvases = append(canvases, n)
		}
	}
	s = svgTagRe.ReplaceAllStringFunc(s, func(m string) string {
		return "\n\n" + imageFixture(imageNode{kind: "svg", alt: svgDescription(m)}) + "\n\n"
	})
	s = canvasTagRe.ReplaceAllStringFunc(s, func(m string) string {
		n := imageNode{kind: "canvas", alt: imgAlt(parseAttrs(m))}
		if len(canvases) > 0 {
			live := canvases[0]
			canvases = canvases[1:]
			if live.alt != "" && n.alt == "" {
				n.alt = live.alt
			}
			n.src = live.src
		}
		_ = m
		return "\n\n" + imageFixture(n) + "\n\n"
	})
	s = imgTagRe.ReplaceAllStringFunc(s, func(m string) string {
		attrs := parseAttrs(m)
		src := attrs["src"]
		if src == "" {
			src = attrs["data-src"]
		}
		n := imageNode{kind: "img", alt: imgAlt(attrs), src: strings.TrimSpace(src)}
		if len(imgs) > 0 {
			live := imgs[0]
			imgs = imgs[1:]
			if live.alt != "" && n.alt == "" {
				n.alt = live.alt
			}
			if isImageDataURL(live.src) {
				n.src = live.src
			}
		}
		return "\n\n" + imageFixture(n) + "\n\n"
	})
	s = backgroundImageRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := backgroundImageRe.FindStringSubmatch(m)
		src := ""
		if len(sub) == 2 {
			src = strings.TrimSpace(sub[1])
		}
		return " background-image:none " + "\n\n" + imageFixture(imageNode{kind: "background", src: src}) + "\n\n"
	})
	return s
}

// applyLiveOverrides resolves parsed nodes against read-only collected live
// payloads, aligned per kind in document order. Resolved data: URLs replace
// blob:/empty sources; live alt fills only an empty node alt.
func applyLiveOverrides(nodes []imageNode, imgLive, canvasLive []liveImageSlot) []imageNode {
	out := make([]imageNode, len(nodes))
	copy(out, nodes)
	ii, ci := 0, 0
	for i, n := range out {
		switch n.kind {
		case "img":
			if ii < len(imgLive) {
				live := imgLive[ii]
				ii++
				if out[i].alt == "" && strings.TrimSpace(live.Alt) != "" {
					out[i].alt = strings.TrimSpace(live.Alt)
				}
				if isImageDataURL(live.DataURL) {
					out[i].src = live.DataURL
				} else if out[i].src == "" && strings.TrimSpace(live.Src) != "" {
					out[i].src = strings.TrimSpace(live.Src)
				}
			}
		case "canvas":
			if ci < len(canvasLive) {
				live := canvasLive[ci]
				ci++
				if out[i].alt == "" && strings.TrimSpace(live.Alt) != "" {
					out[i].alt = strings.TrimSpace(live.Alt)
				}
				if isImageDataURL(live.DataURL) {
					out[i].src = live.DataURL
				}
			}
		}
	}
	return out
}

// ConvertResponseWithLiveImages converts one fragment with read-only
// collected live payloads applied first. Nil live slices behave exactly like
// ConvertResponseWithImages.
func ConvertResponseWithLiveImages(fragment string, imgLive, canvasLive []liveImageSlot) (string, []ResponseImage) {
	nodes := applyLiveOverrides(parseImageNodes(fragment), imgLive, canvasLive)
	text := strings.TrimSpace(stripTags(fragment))
	marker := imageMarkupRe.MatchString(fragment)
	if text == "" && len(nodes) == 0 && !marker {
		return "", nil
	}
	// Wire-protocol and ACK fast paths apply only without image nodes or
	// dangling markers (see ConvertResponseWithImages).
	if len(nodes) == 0 && !marker {
		without := bridgeCallRe.ReplaceAllString(text, "")
		if strings.TrimSpace(without) == "" {
			return text, nil
		}
		if text == "ACK" {
			return text, nil
		}
		return geminiHTMLToMarkdown(fragment), nil
	}
	md := htmlToMarkdownWithNodes(fragment, nodes)
	images := extractImagesFromNodes(nodes)
	if n := omittedNodesCount(nodes, len(images)); n > 0 {
		md += "\n\n[" + pluralImages(n) + " omitted: exceeds per-turn image budget]"
	}
	md = annotateDanglingImageRefs(md)
	return md, images
}

// htmlToMarkdownWithNodes runs the generic extraction with pre-resolved
// nodes, so live payloads and fixtures agree.
func htmlToMarkdownWithNodes(fragment string, nodes []imageNode) string {
	s := fragment
	s = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<button[^>]*>.*?</button>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<[^>]*role="toolbar"[^>]*>.*?</[^>]+>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<[^>]*(aria-hidden="true"|data-thought|data-thinking)[^>]*>.*?</[^>]+>`).ReplaceAllString(s, "")
	s = substituteNodesForFixtures(s, nodes)
	return htmlToMarkdownFrom(s)
}

// substituteNodesForFixtures renders pre-resolved nodes as fixtures,
// consuming per-kind queues in document order (same alignment as the live
// merge).
func substituteNodesForFixtures(s string, nodes []imageNode) string {
	var imgs, canvases []imageNode
	for _, n := range nodes {
		switch n.kind {
		case "img":
			imgs = append(imgs, n)
		case "canvas":
			canvases = append(canvases, n)
		}
	}
	s = svgTagRe.ReplaceAllStringFunc(s, func(m string) string {
		return "\n\n" + imageFixture(imageNode{kind: "svg", alt: svgDescription(m)}) + "\n\n"
	})
	s = canvasTagRe.ReplaceAllStringFunc(s, func(m string) string {
		n := imageNode{kind: "canvas"}
		if len(canvases) > 0 {
			n = canvases[0]
			canvases = canvases[1:]
		}
		return "\n\n" + imageFixture(n) + "\n\n"
	})
	s = imgTagRe.ReplaceAllStringFunc(s, func(m string) string {
		n := imageNode{kind: "img"}
		if len(imgs) > 0 {
			n = imgs[0]
			imgs = imgs[1:]
		}
		return "\n\n" + imageFixture(n) + "\n\n"
	})
	s = backgroundImageRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := backgroundImageRe.FindStringSubmatch(m)
		src := ""
		if len(sub) == 2 {
			src = strings.TrimSpace(sub[1])
		}
		return " background-image:none " + "\n\n" + imageFixture(imageNode{kind: "background", src: src}) + "\n\n"
	})
	return s
}

// ExtractResponseImages returns the image_url-capable parts for one
// model-response fragment: inline data: payloads within count/size caps.
// Oversized, over-count, remote, blob, and missing-src nodes are NOT parts —
// their fixtures (plus omit-notes) already carry the meaning in the text, so
// the turn degrades instead of failing.
func ExtractResponseImages(fragment string) []ResponseImage {
	return extractImagesFromNodes(parseImageNodes(fragment))
}

// extractImagesFromNodes returns the capped parts for pre-resolved nodes.
func extractImagesFromNodes(nodes []imageNode) []ResponseImage {
	var out []ResponseImage
	for _, n := range nodes {
		if n.kind != "img" && n.kind != "canvas" {
			continue
		}
		if !isImageDataURL(n.src) {
			continue
		}
		if len(out) >= MaxResponseImages {
			continue
		}
		if sz := dataURLByteSize(n.src); sz < 0 || sz > MaxResponseImageBytes {
			continue
		}
		alt := strings.Join(strings.Fields(n.alt), " ")
		out = append(out, ResponseImage{Alt: alt, URL: n.src})
	}
	return out
}

// omittedImageNotes counts image nodes that carry no part (oversized,
// over-count, or unresolvable) so the text can note the omission.
func omittedImageCount(fragment string, kept int) int {
	return omittedNodesCount(parseImageNodes(fragment), kept)
}

// omittedNodesCount counts pre-resolved data: nodes beyond the kept parts.
func omittedNodesCount(nodes []imageNode, kept int) int {
	need := 0
	for _, n := range nodes {
		if (n.kind == "img" || n.kind == "canvas") && isImageDataURL(n.src) {
			need++
		}
	}
	if need <= kept {
		return 0
	}
	return need - kept
}

// annotateDanglingImageRefs rewrites bare model-emitted "[Image N]" stubs
// (no retrievable image behind them) into inert explained notes. Fixtures
// the extractor itself produced ("[Image: ...]") never match. Code examples
// are shielded: a documented "[Image 1]" inside fences stays literal.
func annotateDanglingImageRefs(md string) string {
	var g codeGuard
	s := g.protect(md)
	s = danglingImageRefRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := danglingImageRefRe.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		return unretrievableNote(sub[1])
	})
	return g.restore(s)
}

// rewriteUnusableMarkdownImages replaces literal ![alt](blob:...) and
// ![alt]() image syntax with inert notes: a blob: URL is a same-document
// capability the downstream client can never resolve, and an empty target
// renders nothing. Usable targets (https:, data:) pass through untouched,
// as do ordinary [text](url) links. Code examples are shielded.
func rewriteUnusableMarkdownImages(md string) string {
	var g codeGuard
	s := g.protect(md)
	s = mdImageRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdImageRe.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		alt, src := strings.TrimSpace(sub[1]), strings.TrimSpace(sub[2])
		lower := strings.ToLower(src)
		if lower == "" || strings.HasPrefix(lower, "blob:") {
			if alt == "" {
				alt = "untitled"
			}
			return "[Image: " + escapeAlt(alt) + "] [visual content not retrievable from web UI]"
		}
		return m
	})
	return g.restore(s)
}

// ticks returns the backtick run that safely fences raw (ChatGPT parity:
// longest run + 1). Callers use it only when raw contains fencing, so the
// common triple-backtick path stays byte-identical.
func ticks(raw string) string {
	best := 0
	run := 0
	for _, c := range raw {
		if c == '`' {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	n := best + 1
	if n < 1 {
		n = 1
	}
	return strings.Repeat("`", n)
}

// ConvertResponseHTML converts one model-response outerHTML fragment to
// Markdown. Pure function — unit-tested with fixture HTML, no browser needed.
func ConvertResponseHTML(fragment string) string {
	md, _ := ConvertResponseWithImages(fragment)
	return md
}

// ConvertResponseWithImages converts one fragment to Markdown plus the
// image_url-capable parts (inline data: payloads within caps). Text-only
// fragments take the legacy path byte-identically: no behavior change on the
// happy path. Image-bearing fragments gain ChatGPT-consistent fixtures, an
// omit-note when caps drop a payload, and inert notes for dangling
// model-emitted "[Image N]" stubs. Neither path ever fails: worst case the
// text (with notes) survives and parts are empty.
func ConvertResponseWithImages(fragment string) (string, []ResponseImage) {
	nodes := parseImageNodes(fragment)
	text := strings.TrimSpace(stripTags(fragment))
	marker := imageMarkupRe.MatchString(fragment)
	if text == "" && len(nodes) == 0 && !marker {
		return "", nil
	}
	if len(nodes) == 0 && !marker {
		// Wire-protocol preservation: only bridge wrappers (and
		// whitespace) means the model's raw text is the protocol —
		// return it untouched.
		without := bridgeCallRe.ReplaceAllString(text, "")
		if strings.TrimSpace(without) == "" {
			return text, nil
		}
		// Historical ACK acknowledgement passes through verbatim.
		if text == "ACK" {
			return text, nil
		}
		return geminiHTMLToMarkdown(fragment), nil
	}
	// Image path: parsed nodes, dangling model-emitted "[Image N]" markers,
	// or both. The wire fast path is intentionally skipped here: an
	// image-only answer must yield its fixtures (not ""), and wrapper text
	// mixed with image content must reach validation as markdown, never
	// execute from raw.
	md := geminiHTMLToMarkdown(fragment)
	images := ExtractResponseImages(fragment)
	if n := omittedImageCount(fragment, len(images)); n > 0 {
		md += "\n\n[" + pluralImages(n) + " omitted: exceeds per-turn image budget]"
	}
	md = annotateDanglingImageRefs(md)
	return md, images
}

func pluralImages(n int) string {
	if n == 1 {
		return "1 image"
	}
	return strings.TrimSpace(strings.Join([]string{itoa(n), "images"}, " "))
}

// htmlToMarkdown converts rendered answer HTML to GitHub-flavored Markdown.
// It handles the constructs Gemini's renderer emits; anything unrecognized
// degrades to its text content (never dropped silently, never raw HTML kept
// except inside fenced code spans which are text by definition).
func geminiHTMLToMarkdown(fragment string) string {
	s := fragment

	// Remove non-content first: scripts, styles, buttons, toolbars, hidden
	// nodes, copy controls, thought blocks.
	s = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<button[^>]*>.*?</button>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<[^>]*role="toolbar"[^>]*>.*?</[^>]+>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<[^>]*(aria-hidden="true"|data-thought|data-thinking)[^>]*>.*?</[^>]+>`).ReplaceAllString(s, "")

	// Image-bearing nodes become inert markdown fixtures before the generic
	// path runs, so they survive as ordinary paragraphs (ChatGPT parity:
	// [Image: ...] / [Canvas: ...] / [SVG: ...]). No-ops byte-identically
	// when the fragment carries no image markup.
	s = substituteImageFixtures(s)

	return htmlToMarkdownFrom(s)
}

// htmlToMarkdownFrom runs the generic extraction after non-content removal
// and image-fixture substitution. Split out so the live-override path shares
// it exactly.
func htmlToMarkdownFrom(s string) string {
	// Code blocks first (whitespace-sensitive): <pre><code class="language-x">
	// or Gemini's copy container. Language from class="language-x". Fences
	// stay triple-backtick unless the source itself contains fencing, in
	// which case the fence grows (ticks parity) instead of breaking.
	s = regexp.MustCompile(`(?s)<pre[^>]*>\s*<code[^>]*class="[^"]*language-([a-zA-Z0-9+#-]+)[^"]*"[^>]*>(.*?)</code>\s*</pre>`).
		ReplaceAllStringFunc(s, func(m string) string {
			parts := regexp.MustCompile(`(?s)<pre[^>]*>\s*<code[^>]*class="[^"]*language-([a-zA-Z0-9+#-]+)[^"]*"[^>]*>(.*?)</code>\s*</pre>`).FindStringSubmatch(m)
			// Strip syntax-highlight spans BEFORE decoding source entities, then
			// keep source escaped until the final prose-tag cleanup has run.
			lang, code := parts[1], escapeCodeSource(parts[2])
			fence := "```"
			if strings.Contains(html.UnescapeString(code), "```") {
				fence = ticks(html.UnescapeString(code))
				if len(fence) < 3 {
					fence = "```"
				}
			}
			return "\n" + fence + lang + "\n" + strings.Trim(code, "\n") + "\n" + fence + "\n"
		})
	s = regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`).ReplaceAllStringFunc(s, func(m string) string {
		code := escapeCodeSource(regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`).FindStringSubmatch(m)[1])
		fence := "```"
		if strings.Contains(html.UnescapeString(code), "```") {
			fence = ticks(html.UnescapeString(code))
			if len(fence) < 3 {
				fence = "```"
			}
		}
		return "\n" + fence + "\n" + strings.Trim(code, "\n") + "\n" + fence + "\n"
	})

	// Headings h1..h6.
	for i := 6; i >= 1; i-- {
		tag := string(rune('0' + i))
		re := regexp.MustCompile(`(?s)<h` + tag + `[^>]*>(.*?)</h` + tag + `>`)
		prefix := strings.Repeat("#", i) + " "
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			inner := stripTags(re.FindStringSubmatch(m)[1])
			return "\n" + prefix + inner + "\n"
		})
	}

	// Bold / italic / strikethrough / inline code.
	s = regexp.MustCompile(`(?s)<(strong|b)[^>]*>(.*?)</(strong|b)>`).ReplaceAllString(s, "**$2**")
	s = regexp.MustCompile(`(?s)<(em|i)[^>]*>(.*?)</(em|i)>`).ReplaceAllString(s, "*$2*")
	s = regexp.MustCompile(`(?s)<(del|s|strike)[^>]*>(.*?)</(del|s|strike)>`).ReplaceAllString(s, "~~$2~~")
	s = regexp.MustCompile(`(?s)<code[^>]*>(.*?)</code>`).ReplaceAllStringFunc(s, func(m string) string {
		inner := html.EscapeString(stripTags(regexp.MustCompile(`(?s)<code[^>]*>(.*?)</code>`).FindStringSubmatch(m)[1]))
		if strings.Contains(inner, "``") {
			// Source already carries double fencing: grow the fence
			// (ticks parity) instead of emitting an ambiguous span.
			// Single-backtick sources keep the legacy "`` x ``" shape
			// byte-identically (happy path unchanged).
			raw := html.UnescapeString(inner)
			fence := ticks(raw)
			pad := ""
			if strings.HasPrefix(raw, "`") || strings.HasSuffix(raw, "`") ||
				(strings.HasPrefix(raw, " ") && strings.HasSuffix(raw, " ") && strings.TrimSpace(raw) != "") {
				pad = " "
			}
			return fence + pad + raw + pad + fence
		}
		if strings.Contains(inner, "`") {
			return "`` " + inner + " ``"
		}
		return "`" + inner + "`"
	})

	// Links: [text](url "title").
	s = regexp.MustCompile(`<a[^>]*href="([^"]*)"[^>]*>(.*?)</a>`).ReplaceAllStringFunc(s, func(m string) string {
		parts := regexp.MustCompile(`<a[^>]*href="([^"]*)"[^>]*>(.*?)</a>`).FindStringSubmatch(m)
		text := strings.TrimSpace(stripTags(parts[2]))
		url := strings.TrimSpace(parts[1])
		if text == "" {
			return url
		}
		return "[" + text + "](" + url + ")"
	})

	// Lists: li items get "- " (ul) or "1. " (ol); nested depth by ancestor
	// count is approximated — Gemini flattens to one level in practice.
	s = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`).ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(stripTags(regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`).FindStringSubmatch(m)[1]))
		return "\n- " + inner
	})
	s = regexp.MustCompile(`(?s)<(ul|ol)[^>]*>(.*?)</(ul|ol)>`).ReplaceAllString(s, "\n$2\n")

	// Blockquotes.
	s = regexp.MustCompile(`(?s)<blockquote[^>]*>(.*?)</blockquote>`).ReplaceAllStringFunc(s, func(m string) string {
		inner := stripTags(regexp.MustCompile(`(?s)<blockquote[^>]*>(.*?)</blockquote>`).FindStringSubmatch(m)[1])
		lines := strings.Split(strings.TrimSpace(html.UnescapeString(inner)), "\n")
		for i, l := range lines {
			lines[i] = "> " + strings.TrimSpace(l)
		}
		return "\n" + strings.Join(lines, "\n") + "\n"
	})

	// Tables: header row + delimiter + body rows.
	s = regexp.MustCompile(`(?s)<table[^>]*>(.*?)</table>`).ReplaceAllStringFunc(s, func(m string) string {
		body := regexp.MustCompile(`(?s)<table[^>]*>(.*?)</table>`).FindStringSubmatch(m)[1]
		rowRe := regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
		cellRe := regexp.MustCompile(`(?s)<t[hd][^>]*>(.*?)</t[hd]>`)
		var rows [][]string
		for _, row := range rowRe.FindAllStringSubmatch(body, -1) {
			var cells []string
			for _, cell := range cellRe.FindAllStringSubmatch(row[1], -1) {
				c := strings.ReplaceAll(stripTags(cell[1]), "|", `\|`)
				c = strings.ReplaceAll(strings.TrimSpace(html.UnescapeString(c)), "\n", "<br>")
				cells = append(cells, c)
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
		}
		if len(rows) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("\n| " + strings.Join(rows[0], " | ") + " |\n|")
		for range rows[0] {
			b.WriteString(" --- |")
		}
		b.WriteString("\n")
		for _, r := range rows[1:] {
			b.WriteString("| " + strings.Join(r, " | ") + " |\n")
		}
		return b.String()
	})

	// Horizontal rules, line breaks, paragraphs/divs.
	s = regexp.MustCompile(`<hr[^>]*/?>`).ReplaceAllString(s, "\n---\n")
	s = regexp.MustCompile(`(?i)<br[^>]*/?>`).ReplaceAllString(s, "  \n")
	s = regexp.MustCompile(`(?s)<(p|div|section|article)[^>]*>(.*?)</(p|div|section|article)>`).ReplaceAllString(s, "\n$2\n")

	// Final: strip leftovers, unescape, normalize blank lines.
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.Join(lines, "\n")
	s = wsRe.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	// Last: literal Markdown images with unusable targets (blob:, empty)
	// become inert notes. Usable targets, links, and code pass through
	// untouched, so text-only output without such syntax is byte-identical.
	return rewriteUnusableMarkdownImages(s)
}

func escapeCodeSource(fragment string) string {
	return html.EscapeString(html.UnescapeString(tagRe.ReplaceAllString(fragment, "")))
}
