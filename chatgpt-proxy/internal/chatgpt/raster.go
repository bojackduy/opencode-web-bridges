package chatgpt

import (
	"context"
	_ "embed"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

//go:embed raster.js
var rasterSVGJS string

// RasterImage is one model SVG rasterized bridge-side to a PNG data URL. The
// TUI demotes SVG to plain text and only raster image parts reach terminal
// graphics, so the bridge converts substantive answer SVGs to PNG and serves
// them as Markdown image attachments (data:image/png) in the assistant text.
type RasterImage struct {
	Alt     string
	DataURL string // data:image/png;base64,...
}

const (
	maxRasterImages  = 4
	maxRasterDataURL = 6 << 20 // ~4.5MB decoded PNG; diagrams land in the KBs
)

// mermaidFence matches a fenced Mermaid source block. The client renders
// this subset itself, so the bridge returns the code as text and skips the
// raster screenshot for the turn.
var mermaidFence = regexp.MustCompile("(?m)^```mermaid")

// hasMermaidFence reports whether the answer carries Mermaid source the
// client can render, making a raster screenshot redundant.
func hasMermaidFence(answer string) bool {
	return mermaidFence.MatchString(answer)
}

// ValidRasterImage rejects anything that is not a bounded PNG data URL:
// wrong media type, oversized payloads, undecodable base64, or bytes without
// the PNG signature. Never trust renderer output on the wire.
func ValidRasterImage(dataURL string) bool {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(dataURL, prefix) {
		return false
	}
	if len(dataURL) > maxRasterDataURL {
		return false
	}
	raw := dataURL[len(prefix):]
	if raw == "" {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) < 8 {
		return false
	}
	return b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G'
}

// sanitizeRasterAlt keeps alt text to one Markdown-safe line for the served
// image reference. Brackets would break the ![alt](url) envelope.
func sanitizeRasterAlt(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "[", "")
	s = strings.ReplaceAll(s, "]", "")
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	r := []rune(s)
	if len(r) > 100 {
		r = r[:100]
	}
	if strings.TrimSpace(string(r)) == "" {
		return "diagram"
	}
	return string(r)
}

// rasterizeAnswerSVG renders the final answer's substantive inline SVGs to PNG
// data URLs inside the page (canvas 2D, no new dependencies). baseline is the
// pre-submit DOM identity from responseProbe; answer gates protocol/empty
// outputs that carry no rendered SVG. Best-effort: any failure yields nil and
// the "[SVG: ...]" text fallback stands.
func (c *Client) rasterizeAnswerSVG(tab context.Context, answer string, baseline []string) []RasterImage {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" || strings.HasPrefix(trimmed, "<opencode") || strings.HasPrefix(trimmed, "<escape>") {
		return nil
	}
	st, err := responseProbe(tab, baseline)
	if err != nil || st.LastID == "" {
		return nil
	}
	return c.rasterizeIDs(tab, st.LastID)
}

// rasterizeIDs renders the substantive SVGs rendered under one assistant
// message id to PNG data URLs. Shared by the normal path (rasterizeAnswerSVG)
// and the recovery/adoption path (snapshotThread), so a recovered turn carries
// the same image parts as a live one. Best-effort: any failure yields nil and
// the "[SVG: ...]" text fallback stands.
func (c *Client) rasterizeIDs(tab context.Context, lastID string) []RasterImage {
	if lastID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(tab, 45*time.Second)
	defer cancel()
	var raw []struct {
		Alt     string `json:"alt"`
		DataURL string `json:"dataURL"`
	}
	js := "(" + rasterSVGJS + ")(" + strconv.Quote(lastID) + ")"
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &raw,
		chromedp.EvalAsValue,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) },
	)); err != nil || len(raw) == 0 {
		return nil
	}
	var out []RasterImage
	for _, r := range raw {
		if !ValidRasterImage(r.DataURL) {
			continue
		}
		out = append(out, RasterImage{Alt: sanitizeRasterAlt(r.Alt), DataURL: r.DataURL})
		if len(out) >= maxRasterImages {
			break
		}
	}
	return out
}
