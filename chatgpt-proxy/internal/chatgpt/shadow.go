package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// The shadow reader is a measurement tool, not an answer source. It passively
// decodes the browser's own SSE response for one generation request and
// compares that reconstruction with the answer the DOM reader already
// produced. It never decides what is delivered, never submits, never retries,
// and never touches recovery state. Every log line is content-free.
const (
	shadowMaxFrames = 1000
	shadowMaxBytes  = 2 << 20
	shadowMaxHops   = 64
	// streamResourceContent is required: an SSE response never reports
	// loadingFinished here, and without it Chrome withholds the body.
	shadowStreamTimeout = 15 * time.Second
)

// streamCaptureEnabled reports whether the adapter reads the generation
// response stream. It is on by default: the capture is the only proof of a
// turn that does not depend on the page settling, and "a previous reply could
// not be confirmed" is exactly that dependency failing. Setting
// CHATGPT_BRIDGE_SHADOW_STREAM to 0/false/off/no returns to the DOM-only path.
func streamCaptureEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CHATGPT_BRIDGE_SHADOW_STREAM"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// shadowCapture decodes one stream. Fields are written under mu because CDP
// events arrive on the tab's event loop while the request goroutine feeds data.
type shadowCapture struct {
	mu           sync.Mutex
	prompt       string
	conversation string
	requestID    network.RequestID

	buf    []byte
	frames int
	bytes  int

	// Sticky patch cursor: the capture's frames omit p/o on continuation
	// frames, exactly like the measured protocol.
	lastOp   string
	lastPath string

	state    map[string]any
	message  map[string]any
	identity string // how the submitted prompt was matched, never its text
	// messages indexes every message object seen, and seen preserves arrival
	// order so "newest matching message" is deterministic.
	messages map[string]map[string]any
	seen     []string

	userID      string
	userMatched bool
	completion  string
	done        bool

	unsupported    bool
	ignoredPatches int
	reason         string
}

func newShadowCapture(prompt string) *shadowCapture {
	return &shadowCapture{
		prompt:   prompt,
		state:    map[string]any{},
		messages: map[string]map[string]any{},
	}
}

// startShadowCapture subscribes to network events on the tab and returns a
// capture already listening. It must run before the Send click.
func startShadowCapture(tab context.Context, prompt string) (*shadowCapture, func()) {
	s := newShadowCapture(prompt)
	if err := chromedp.Run(tab, network.Enable()); err != nil {
		log.Printf("[chatgpt-shadow] network enable failed; shadow disabled")
		return nil, func() {}
	}
	// A listener is unregistered by cancelling the context it was bound to.
	lctx, lcancel := context.WithCancel(tab)
	chromedp.ListenTarget(lctx, func(ev any) { s.onEvent(tab, ev) })
	return s, lcancel
}

func (s *shadowCapture) onEvent(tab context.Context, ev any) {
	// A listener runs on the CDP event loop: a panic here would take the whole
	// adapter down, so an opt-in measurement path fails closed instead.
	defer func() {
		if r := recover(); r != nil {
			s.fail("listener panic")
		}
	}()
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		if e.Request == nil || e.Request.Method != "POST" {
			return
		}
		u := e.Request.URL
		if !strings.HasPrefix(u, "https://chatgpt.com/backend-api/f/conversation") {
			return
		}
		s.mu.Lock()
		if s.requestID == "" {
			s.requestID = e.RequestID
		}
		s.mu.Unlock()
	case *network.EventResponseReceived:
		s.mu.Lock()
		match := s.requestID != "" && e.RequestID == s.requestID
		s.mu.Unlock()
		if !match || !strings.Contains(e.Response.MimeType, "text/event-stream") {
			return
		}
		// The body is buffered until it is streamed; fetch the prefix off the
		// event goroutine so CDP delivery is never blocked.
		go func(id network.RequestID) {
			ctx, cancel := context.WithTimeout(tab, shadowStreamTimeout)
			defer cancel()
			// The request body carries the conversation this turn continues.
			// Only that one field is read; the prompt text is never logged.
			_ = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				post, err := network.GetRequestPostData(id).Do(ctx)
				if err != nil {
					return nil
				}
				var body struct {
					ConversationID string `json:"conversation_id"`
				}
				if json.Unmarshal([]byte(post), &body) == nil && body.ConversationID != "" {
					s.mu.Lock()
					s.conversation = body.ConversationID
					s.mu.Unlock()
				}
				return nil
			}))
			var buf []byte
			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				buf, err = network.StreamResourceContent(id).Do(ctx)
				return err
			})); err != nil {
				s.fail("stream unavailable")
				return
			}
			s.feed(buf)
		}(e.RequestID)
	case *network.EventDataReceived:
		s.mu.Lock()
		match := s.requestID != "" && e.RequestID == s.requestID
		s.mu.Unlock()
		if !match {
			return
		}
		raw, err := base64.StdEncoding.DecodeString(e.Data)
		if err != nil {
			s.fail("invalid chunk")
			return
		}
		s.feed(raw)
	case *network.EventLoadingFailed:
		s.mu.Lock()
		match := s.requestID != "" && e.RequestID == s.requestID
		s.mu.Unlock()
		if match {
			s.fail("stream aborted")
		}
	}
}

func (s *shadowCapture) fail(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Teardown aborts the SSE connection after the server has already declared
	// the stream complete. A capture that holds an explicit completion is
	// final and must not be downgraded by a late transport error.
	if s.reason == "" && !s.done {
		s.reason = reason
	}
}

// feed appends raw bytes and decodes every complete frame. Byte-level
// accumulation is deliberate: a UTF-8 rune split across chunks must not be
// decoded early, and frame boundaries are byte boundaries.
func (s *shadowCapture) feed(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" || s.done {
		return
	}
	s.bytes += len(chunk)
	if s.bytes > shadowMaxBytes {
		s.reason = "byte limit"
		return
	}
	s.buf = append(s.buf, chunk...)
	for {
		i := bytes.Index(s.buf, []byte("\n\n"))
		if i < 0 {
			// Tolerate CRLF separators as well.
			if j := bytes.Index(s.buf, []byte("\r\n\r\n")); j >= 0 {
				s.block(string(s.buf[:j]))
				s.buf = s.buf[j+4:]
				continue
			}
			break
		}
		s.block(string(s.buf[:i]))
		s.buf = s.buf[i+2:]
		if s.done || s.reason != "" {
			return
		}
	}
	if len(s.buf) > shadowMaxBytes {
		// A single SSE frame can be large: the answer is written as one
		// whole-mapping frame. Only the total byte budget bounds the capture.
		s.reason = "frame limit"
	}
}

func (s *shadowCapture) block(raw string) {
	if s.frames++; s.frames > shadowMaxFrames {
		s.reason = "frame limit"
		return
	}
	var data []string
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(v))
		}
	}
	if len(data) == 0 {
		return
	}
	text := strings.Join(data, "\n")
	if text == "[DONE]" {
		s.done = true
		return
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(text), &frame); err != nil {
		// The first measured frame is `event: delta_encoding` with a bare
		// string payload; it carries no protocol state. Skip any well-formed
		// JSON that is not an object, and only call genuinely broken JSON a
		// failure.
		if json.Valid([]byte(text)) {
			return
		}
		s.reason = "invalid frame"
		return
	}
	s.frame(frame)
}

// frame mirrors the measured protocol: a frame either patches the message map
// (p/o/v, with p and o sticky across frames) or carries a typed event.
func (s *shadowCapture) frame(frame map[string]any) {
	if _, ok := frame["o"].(string); ok {
		s.lastOp, _ = frame["o"].(string)
	}
	if _, ok := frame["p"].(string); ok {
		s.lastPath, _ = frame["p"].(string)
	}
	if _, hasV := frame["v"]; hasV {
		s.patch(frame["v"])
		s.index()
		return
	}
	s.inspect(frame)
}

func (s *shadowCapture) patch(v any) {
	// A grouped patch carries an ordered list of operations.
	if s.lastOp == "patch" {
		if list, ok := v.([]any); ok {
			savedOp, savedPath := s.lastOp, s.lastPath
			for _, item := range list {
				op, ok := item.(map[string]any)
				if !ok {
					s.unsupported = true
					continue
				}
				if o, ok := op["o"].(string); ok {
					s.lastOp = o
				}
				if p, ok := op["p"].(string); ok {
					s.lastPath = p
				}
				s.patchValue(s.lastPath, s.lastOp, op["v"])
			}
			s.lastOp, s.lastPath = savedOp, savedPath
			return
		}
		s.unsupported = true
		return
	}
	s.patchValue(s.lastPath, s.lastOp, v)
}

func (s *shadowCapture) patchValue(path, op string, v any) {
	// The measured full-mapping frame replaces the whole state.
	if path == "" || path == "/" {
		switch op {
		case "add", "replace":
			if obj, ok := v.(map[string]any); ok {
				s.state = obj
				return
			}
		}
		s.unsupported = true
		return
	}
	keys := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !setPath(s.state, keys, op, v) {
		// One measured patch appends an object onto /message/metadata, which
		// this reconstructor does not merge. That is safe to skip only when the
		// path cannot change the answer body, the author, the message ID, or
		// the parent link the lineage check depends on.
		if patchIsInert(path) {
			s.ignoredPatches++
			return
		}
		s.unsupported = true
	}
}

// patchIsInert reports whether an unapplied patch is unable to affect the
// answer text or the identity chain used to verify it.
func patchIsInert(path string) bool {
	p := strings.TrimPrefix(path, "/")
	switch {
	case p == "message", p == "conversation_id":
		return false
	case strings.HasPrefix(p, "message/content"),
		strings.HasPrefix(p, "message/author"),
		strings.HasPrefix(p, "message/metadata/parent_id"),
		strings.HasPrefix(p, "message/metadata/streaming_parent"),
		strings.HasPrefix(p, "message/id"):
		return false
	}
	return strings.HasPrefix(p, "message/")
}

// setPath applies one patch to the reconstructed state. Only the operations
// actually observed are accepted; anything else is reported as unsupported
// rather than guessed at.
func setPath(root map[string]any, keys []string, op string, val any) bool {
	if len(keys) == 0 {
		return false
	}
	var node any = root
	for _, key := range keys[:len(keys)-1] {
		next, ok := childOf(node, key)
		if !ok {
			return false
		}
		node = next
	}
	last := keys[len(keys)-1]
	switch op {
	case "append":
		cur, ok := childOf(node, last)
		if !ok {
			return false
		}
		cs, ok1 := cur.(string)
		vs, ok2 := val.(string)
		if !ok1 || !ok2 {
			return false
		}
		return setChild(node, last, cs+vs)
	case "replace", "add":
		// A trailing "-" addresses the end of an array. The reconstructor
		// holds slices by value, so an append would not persist into the
		// parent without a larger refactor; report it rather than guess.
		if last == "-" {
			return false
		}
		return setChild(node, last, val)
	case "remove":
		return removeChild(node, last)
	}
	return false
}

func childOf(node any, key string) (any, bool) {
	switch t := node.(type) {
	case map[string]any:
		v, ok := t[key]
		return v, ok
	case []any:
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(t) {
			return nil, false
		}
		return t[i], true
	}
	return nil, false
}

func setChild(node any, key string, val any) bool {
	switch t := node.(type) {
	case map[string]any:
		t[key] = val
		return true
	case []any:
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(t) {
			return false
		}
		t[i] = val
		return true
	}
	return false
}

// removeChild deletes one patch target. Map keys persist because maps are
// references; array reslicing would not persist into the parent through the
// value-held traversal, so only map removal is accepted.
func removeChild(node any, key string) bool {
	if m, ok := node.(map[string]any); ok {
		if _, ok := m[key]; !ok {
			return false
		}
		delete(m, key)
		return true
	}
	return false
}

// index records the reconstructed mapping and the typed frames the correlation
// depends on.
func (s *shadowCapture) index() {
	if m, ok := s.state["message"].(map[string]any); ok {
		s.message = m
	}
	s.inspect(s.state)
}

func (s *shadowCapture) inspect(v map[string]any) {
	if m, ok := v["message"].(map[string]any); ok {
		s.remember(m)
		s.message = m
	}
	if m, ok := v["input_message"].(map[string]any); ok {
		s.remember(m)
	}
	if v["type"] == "message_stream_complete" {
		if cid, ok := v["conversation_id"].(string); ok {
			s.completion = cid
		}
		s.done = true
	}
}

func (s *shadowCapture) remember(m map[string]any) {
	id := idOf(m)
	if id == "" {
		return
	}
	if _, ok := s.messages[id]; !ok {
		s.seen = append(s.seen, id)
	}
	s.messages[id] = m
}

func idOf(m map[string]any) string {
	id, _ := m["id"].(string)
	return id
}

func roleOf(m map[string]any) string {
	author, ok := m["author"].(map[string]any)
	if !ok {
		return ""
	}
	role, _ := author["role"].(string)
	return role
}

// partsText returns the message body only when every part is a string and the
// content is a text content type. Any other shape is not comparable.
func partsText(m map[string]any) (string, bool) {
	content, ok := m["content"].(map[string]any)
	if !ok {
		return "", false
	}
	parts, ok := content["parts"].([]any)
	if !ok || len(parts) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, p := range parts {
		str, ok := p.(string)
		if !ok {
			return "", false
		}
		b.WriteString(str)
	}
	return b.String(), true
}

func parentOf(m map[string]any) string {
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	if p, ok := meta["parent_id"].(string); ok {
		return p
	}
	return ""
}

// result reports what this capture established. Correlation requires a
// prompt-anchored user message, an assistant message that reaches it through
// the parent chain, and an explicit completion for the same conversation.
func (s *shadowCapture) result() (correlated bool, text string, reason string) {
	_, _, _, text, reason = s.resolve()
	return reason == "", text, reason
}

// turn reports the server-assigned identity of a completed, correlated turn:
// the conversation and the assistant message that answered this prompt. It is
// the only thing delivery may take from this capture, and only when the stream
// itself declared completion.
func (s *shadowCapture) turn() (thread, assistantID string, ok bool) {
	thread, _, assistantID, _, reason := s.resolve()
	return thread, assistantID, reason == ""
}

// answer returns the reply this capture proved, and only when that reply is
// safe to deliver. A capture that reached an explicit completion carries the
// server's own final message: status, completeness, lineage and conversation
// were all asserted on that message, not read back from the page. The DOM path
// refuses a final message without a bridge call envelope, so this applies the
// same rule rather than delivering arbitrary prose.
func (s *shadowCapture) answer() (text string, ok bool) {
	_, body, reason := s.result()
	if reason != "" || !streamAnswerAcceptable(body) {
		return "", false
	}
	return body, true
}

func (s *shadowCapture) resolve() (thread, userID, assistantID, text, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" {
		return "", "", "", "", s.reason
	}
	if s.unsupported {
		return "", "", "", "", "unsupported patch"
	}
	if s.completion == "" {
		return "", "", "", "", "no explicit completion"
	}
	userID = s.promptUser()
	if userID == "" {
		// Sizes only: the difference is either a missing message or a reshape
		// between the typed prompt and the stored user message.
		var lens []int
		var keys []int
		for _, id := range s.seen {
			m := s.messages[id]
			if roleOf(m) != "user" {
				continue
			}
			if t, ok := partsText(m); ok {
				lens = append(lens, len(t))
				keys = append(keys, len(samePromptKey(t)))
			}
		}
		log.Printf("[chatgpt-shadow] user_candidates=%d lens=%v keys=%v prompt_bytes=%d prompt_key_bytes=%d", len(lens), lens, keys, len(s.prompt), len(samePromptKey(s.prompt)))
		return "", "", "", "", "no matching user message"
	}
	assistant := s.assistantFor(userID)
	if assistant == nil {
		return "", "", "", "", "assistant lineage"
	}
	// The streamed assistant message does not always carry conversation_id at
	// the top level. Bind through whatever this turn's own messages do carry,
	// then through the reconstructed root; an unknown conversation is refused.
	conv, _ := assistant["conversation_id"].(string)
	if conv == "" {
		conv, _ = s.messages[userID]["conversation_id"].(string)
	}
	if conv == "" {
		conv, _ = s.state["conversation_id"].(string)
	}
	if conv != s.completion {
		log.Printf("[chatgpt-shadow] conversation unknown assistant=%t user=%t root=%t", assistant["conversation_id"] != nil, s.messages[userID]["conversation_id"] != nil, s.state["conversation_id"] != nil)
		return "", "", "", "", "completion conversation mismatch"
	}
	if s.conversation != "" && conv != s.conversation {
		return "", "", "", "", "conversation mismatch"
	}
	body, ok := partsText(assistant)
	if !ok || body == "" {
		return "", "", "", "", "answer not text"
	}
	return conv, userID, idOf(assistant), body, ""
}

// promptUser returns the newest user message whose text is the prompt. The
// composer reshapes line breaks and indentation on the way to the server, so
// the comparison collapses whitespace runs; every word must still match.
// A history replay carries older user messages too, so arrival order decides.
func (s *shadowCapture) promptUser() string {
	want := samePromptKey(s.prompt)
	for i := len(s.seen) - 1; i >= 0; i-- {
		m := s.messages[s.seen[i]]
		if roleOf(m) != "user" {
			continue
		}
		if t, ok := partsText(m); ok && samePromptKey(t) == want {
			s.identity = identityExact
			return s.seen[i]
		}
	}
	// A long prompt can render differently from what was typed (markdown in
	// the message rewrites the middle), which used to make every long turn
	// unverifiable. Both ends plus a bounded length still identify the
	// message this capture submitted.
	for i := len(s.seen) - 1; i >= 0; i-- {
		m := s.messages[s.seen[i]]
		if roleOf(m) != "user" {
			continue
		}
		if t, ok := partsText(m); ok && samePromptWindow(t, s.prompt) {
			s.identity = identityWindow
			return s.seen[i]
		}
	}
	return ""
}

const (
	identityExact  = "exact"
	identityWindow = "window"
)

// samePromptWindow accepts a stored user message whose leading and trailing
// runes match the submitted prompt while the length stays within an eighth.
// Interior re-rendering is tolerated; a different message is not.
func samePromptWindow(have, want string) bool {
	h := []rune(samePromptKey(have))
	w := []rune(samePromptKey(want))
	const edge = 200
	if len(w) < 2*edge || len(h) < 2*edge {
		return false
	}
	if d := len(h) - len(w); d > len(w)/8 || d < -len(w)/8 {
		return false
	}
	return string(h[:edge]) == string(w[:edge]) && string(h[len(h)-edge:]) == string(w[len(w)-edge:])
}

// samePromptKey is the prompt identity a textarea round trip preserves: words
// and their order, with whitespace runs treated as a single separator.
func samePromptKey(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// assistantFor walks the parent chain from each assistant message back to the
// prompt-anchored user message. Single-hop equality does not hold: rebase
// system messages sit between them. Newest wins.
func (s *shadowCapture) assistantFor(userID string) map[string]any {
	for i := len(s.seen) - 1; i >= 0; i-- {
		m := s.messages[s.seen[i]]
		if roleOf(m) != "assistant" {
			continue
		}
		if text, ok := partsText(m); !ok || text == "" {
			continue
		}
		if s.reaches(m, userID) {
			return m
		}
	}
	return nil
}

func (s *shadowCapture) reaches(m map[string]any, userID string) bool {
	seen := map[string]bool{}
	cur := m
	for hop := 0; hop < shadowMaxHops; hop++ {
		parent := parentOf(cur)
		if parent == "" {
			return false
		}
		if parent == userID {
			return true
		}
		if seen[parent] {
			return false
		}
		seen[parent] = true
		next, ok := s.messages[parent]
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// reportShadow emits one content-free line. It runs after the turn either way,
// so a disabled, failed or unsupported capture is visible without affecting
// the result.
func reportShadow(s *shadowCapture, id, answer string) {
	if s == nil {
		return
	}
	correlated, text, reason := s.result()
	matched := correlated && sameAnswerText(text, answer)
	s.mu.Lock()
	frames, bytes, completed, ignored := s.frames, s.bytes, s.done, s.ignoredPatches
	identity := s.identity
	s.mu.Unlock()
	prefix, suffix := sharedEdges(text, answer)
	// Content-free shape only: sizes, shared edges and non-ASCII byte counts,
	// which is enough to name the class of a difference without printing it.
	log.Printf("[chatgpt-shadow] id=%s frames=%d bytes=%d completed=%v correlated=%v identity=%q match=%v ignored=%d shadow_bytes=%d answer_bytes=%d trim_shadow_bytes=%d trim_answer_bytes=%d prefix=%d suffix=%d nonascii=%d/%d reason=%q",
		id, frames, bytes, completed, correlated, identity, matched, ignored,
		len(text), len(answer), len(strings.TrimSpace(text)), len(strings.TrimSpace(answer)),
		prefix, suffix, countNonASCII(text), countNonASCII(answer), reason)
}

// sharedEdges reports how many leading and trailing bytes two strings share.
func sharedEdges(a, b string) (int, int) {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	return prefix, suffix
}

func countNonASCII(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			n++
		}
	}
	return n
}

// sameAnswerText compares the reconstructed answer with the delivered one.
// Only surrounding whitespace may differ: the renderer keeps a trailing
// newline the stream does not. Inner text must match exactly, because the
// callers that matter deliver code.
func sameAnswerText(shadow, delivered string) bool {
	return strings.TrimSpace(shadow) == strings.TrimSpace(delivered)
}

// The DOM path refuses to deliver a final message that carries no bridge call
// envelope, and refuses standalone escape artifacts. The stream path delivers
// the same server text, so it applies the same rules rather than accepting any
// final prose.
var (
	bridgeCallRE     = regexp.MustCompile(`<opencode(?:\\)?_bridge(?:\\)?_call\b`)
	escapeArtifactRE = regexp.MustCompile(`(?m)^[ \t]*(?:<escape>\s*</escape>[ \t]*)+[ \t]*$`)
)

func streamAnswerAcceptable(text string) bool {
	if text == "" || escapeArtifactRE.MatchString(text) {
		return false
	}
	return bridgeCallRE.MatchString(text)
}
