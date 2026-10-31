package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"chatgpt-proxy/internal/chatgpt"
)

// Project-per-session (flag-gated, default off): 1 OpenCode session gets 1
// dedicated ChatGPT project, and prefix mismatches ROTATE (fresh working chat
// in the session's project seeded with a compact context card + the last few
// turns verbatim) instead of reset-replaying the full transcript. Old chats
// stay in the project for project-level retrieval. Any project/chat/creation
// failure falls back to today's shared-project thread behavior for that turn.
//
// The flag is CHATGPT_BRIDGE_PROJECT_PER_SESSION=1. When it is unset (or any
// other value), every path below is skipped and behavior is byte-identical to
// the legacy shared-project flow.

// projectPerSessionEnabled reports whether per-session projects are opted in.
func projectPerSessionEnabled() bool {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION"))); v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sessionShort8 is the first 8 alphanumeric runes of a session ID, used in
// project display names. Stateless requests ("") yield "".
func sessionShort8(id string) string {
	var b strings.Builder
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			if utf8.RuneCountInString(b.String()) >= 8 {
				break
			}
		}
	}
	return b.String()
}

var absPathRe = regexp.MustCompile(`/[A-Za-z0-9._~+~-]+(?:/[A-Za-z0-9._~+~-]+)+`)

// junkPathBases are last path segments that carry no repo identity (system
// temp dirs, home roots, the proxy's own state). Best effort only: this feeds
// a display name, never routing.
var junkPathBases = map[string]bool{
	"tmp": true, "temp": true, "var": true, "folders": true, "private": true,
	"Users": true, "home": true, "root": true, "etc": true, "usr": true,
	"chatgpt-proxy": true, ".chatgpt-proxy": true, "T": true,
}

// detectRepoBasename scans system messages for the most frequent absolute
// path and returns its sanitized last segment. Returns "" when nothing
// usable is found; the caller then names the project from the session ID.
func detectRepoBasename(msgs []message) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		for _, p := range absPathRe.FindAllString(contentText(m), -1) {
			counts[p]++
			if counts[p] > bestN {
				best, bestN = p, counts[p]
			}
		}
	}
	if best == "" {
		return ""
	}
	base := path.Base(best)
	if junkPathBases[base] {
		return ""
	}
	var b strings.Builder
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// sessionProjectName derives the dedicated project display name:
// `OpenCode Bridge — <repo-basename>/<ses-short8>`, or with the session
// short ID alone when no repo is detectable in the request context.
func sessionProjectName(id string, req completionsRequest) string {
	short := sessionShort8(id)
	if short == "" {
		short = "bridge"
	}
	if repo := detectRepoBasename(req.Messages); repo != "" {
		return "OpenCode Bridge — " + repo + "/" + short
	}
	return "OpenCode Bridge — " + short
}

// truncateRunes shortens s to at most n runes, marking the cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}

// rotationRecentN is how many trailing transcript messages go into the new
// working chat verbatim alongside the context card (bottom ladder rung).
const rotationRecentN = 6

// rotationLevel is one rung of the rotation-payload ladder, richest first.
// The bottom rung reproduces the legacy card byte-for-byte (recent 6,
// 200-char results, 3000B digest). Richer rungs carry full tool results and
// wider verbatim windows so the new thread inherits evidence, not just
// summaries — directly reducing post-rotation hallucination.
type rotationLevel struct {
	recentN   int // verbatim trailing turns
	resultCap int // per-tool-result chars
	digestMax int // total digest bytes
	notesN    int // assistant conclusions kept
	noteCap   int // chars per conclusion
}

var rotationLadder = []rotationLevel{
	{16, 8000, 12000, 8, 400},
	{12, 8000, 12000, 8, 400},
	{8, 2000, 6000, 5, 300},
	{rotationRecentN, 200, 3000, 5, 300},
}

// rotationCapped is the shrink rung used when the ladder's pick exceeds the
// single-paste budget: goal plus a short digest and only the last 2 turns
// verbatim. It is tried before falling back to a full chunked replay, so an
// over-budget card shrinks instead of re-sending the whole history.
var rotationCapped = rotationLevel{recentN: 2, resultCap: 500, digestMax: 1500, notesN: 3, noteCap: 200}

const rotationIntro = "BRIDGE SESSION ROTATION\nThis turn starts a fresh working chat inside this session's dedicated project. What follows is a compact context card written locally from stored adapter state (no model was consulted) plus the most recent turns verbatim. Older history remains in this project's retired chats and may be referenced from there. Reply to the current request below; do not replay history.\n"

// buildRotationPrompt composes the single first-turn prompt for a rotated
// working chat deterministically from the adapter's own Messages state: the
// session goal (first stored user message), recent assistant conclusions,
// digested tool results, and the last ~6 incoming turns verbatim. No model
// calls are involved. This is the bottom ladder rung (legacy shape); the
// ranged builder below carries richer payloads when budget allows.
func buildRotationPrompt(req completionsRequest, s *session, head string) string {
	p, _ := buildRotationPromptLevel(req, s, head, rotationLadder[len(rotationLadder)-1])
	return p
}

// buildRotationPromptLevel is the parametrized rotation builder. It also
// returns the verbatim window used, so callers attach files from exactly the
// turns the model sees.
func buildRotationPromptLevel(req completionsRequest, s *session, head string, lv rotationLevel) (string, []message) {
	goal := ""
	for _, m := range s.Messages {
		if m.Role == "user" {
			goal = truncateRunes(strings.TrimSpace(contentText(m)), 800)
			break
		}
	}
	if goal == "" {
		for _, m := range req.Messages {
			if m.Role == "user" {
				goal = truncateRunes(strings.TrimSpace(contentText(m)), 800)
				break
			}
		}
	}
	var notes []string
	for i := len(s.Messages) - 1; i >= 0 && len(notes) < lv.notesN; i-- {
		m := s.Messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(contentText(m)) != "" {
			notes = append([]string{"- " + truncateRunes(strings.TrimSpace(contentText(m)), lv.noteCap)}, notes...)
		}
	}
	var digest []string
	results := map[string]string{}
	for _, m := range s.Messages {
		if m.Role == "tool" && m.ToolCallID != "" && results[m.ToolCallID] == "" {
			results[m.ToolCallID] = strings.TrimSpace(contentText(m))
		}
	}
	for _, m := range s.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			if len(digest) >= 12 {
				break
			}
			line := fmt.Sprintf("- call %s (args %d bytes)", call.Function.Name, len(call.Function.Arguments))
			if res, ok := results[call.ID]; ok {
				line += fmt.Sprintf(" -> result %d bytes: %s", len(res), truncateRunes(res, lv.resultCap))
			}
			if len(strings.Join(digest, "\n"))+len(line) > lv.digestMax {
				break
			}
			digest = append(digest, line)
		}
	}
	recent := req.Messages
	if len(recent) > lv.recentN {
		recent = recent[len(recent)-lv.recentN:]
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(rotationIntro)
	b.WriteString("CONTEXT CARD\nGoal: ")
	if goal == "" {
		b.WriteString("(unknown)")
	} else {
		b.WriteString(goal)
	}
	b.WriteString("\n")
	if len(notes) > 0 {
		b.WriteString("Recent assistant conclusions:\n" + strings.Join(notes, "\n") + "\n")
	}
	if len(digest) > 0 {
		b.WriteString("Tool digest:\n" + strings.Join(digest, "\n") + "\n")
	}
	b.WriteString("END CONTEXT CARD\n\nRECENT TURNS (verbatim, last few):\n")
	for _, m := range recent {
		b.WriteString(renderMsg(m) + "\n")
	}
	b.WriteString("END RECENT TURNS\n")
	b.WriteString(answerInstruction(req))
	return b.String(), recent
}

// serveRotation attempts a project-per-session rotation for a prefix-mismatched
// stateful session: a fresh working chat in the session's dedicated project
// seeded with one context-card turn. The rewrite classifier runs BEFORE this
// (planCleanReseed, or the dirty branch's armed plan) and travels in via plan:
// carried executed results are submitted as inert ACK record imports ahead of
// the rotation card, and a fresh ReseedEvidence/linkage is always committed
// on success so evidence never goes stale. A nil plan (incoherent rewrite)
// rotates as before but still stamps fresh evidence recording the ambiguity.
// It returns true when it wrote the HTTP response (success or terminal
// model-protocol failure). On ANY project/chat creation or submission failure
// it restores the session untouched and returns false so the caller falls
// back to the legacy shared-project reset-replay for that turn — never
// fail-closed, never 409 where the old path would have proceeded.
func (h *handler) serveRotation(w http.ResponseWriter, r *http.Request, id, lane string, req completionsRequest, s *session, plan *rewritePlan) bool {
	snapshot := *s
	name := sessionProjectName(id, req)
	projectURL := s.Project
	toolsFP := toolsFingerprint(req.Tools)
	head := toolPrompt(req)
	// Richest-fitting rung wins: full tool results and wide verbatim
	// windows when budget allows, degrading rung by rung to the legacy
	// card. Nothing fits at all → existing card_over_budget fallback.
	var prompt string
	var recent []message
	leveled := false
	lastLen := 0
	for _, lv := range rotationLadder {
		p, r := buildRotationPromptLevel(req, s, head, lv)
		lastLen = len(p)
		if h.maxChars <= 0 || len(p) <= h.maxChars {
			prompt, recent, leveled = p, r, true
			break
		}
	}
	if !leveled {
		log.Printf("[openai] session=%s lane=%s rotation_fallback reason=card_over_budget bytes=%d budget=%d", id, lane, lastLen, h.maxChars)
		return false
	}
	// The rotation card is ONE composer paste: shrink it to the capped rung
	// when its new content alone exceeds the paste budget, before the journal
	// is dirtied or anything reaches the composer. Oversized pastes stall the
	// page for ~10 minutes with zero answer bytes, so submitting anyway only
	// buys an uncertain turn. Only when even the capped card does not fit
	// does the caller fall back to the capped chunked reset-replay/reseed
	// path. The fixed head cannot be chunked, so only content beyond it
	// counts — same rule as the main choke point.
	if cap := h.pasteLimit(); cap > 0 && len(prompt)-len(head) > cap {
		if p, r := buildRotationPromptLevel(req, s, head, rotationCapped); len(p)-len(head) <= cap {
			log.Printf("[openai] session=%s lane=%s rotation_capped bytes=%d paste_limit=%d", id, lane, len(p), cap)
			prompt, recent = p, r
		} else {
			log.Printf("[openai] session=%s lane=%s rotation_fallback reason=paste_over_budget bytes=%d paste_limit=%d", id, lane, len(prompt), cap)
			return false
		}
	}
	var atts []chatgpt.Attachment
	for _, m := range recent {
		atts = append(atts, attachmentsOf(m)...)
	}
	// The prior-execution record travels first when the classifier carried
	// executed results, built exactly like the shared-project reseed path
	// (inert ACK imports, never pending calls). Nil when nothing executed
	// is missing from the rewritten history: legacy single-card rotation.
	var recordTurns []turn
	if plan != nil {
		recordTurns = h.buildReseedRecordTurns(plan.carried)
	}
	// Fresh linkage for THIS rotation, never the previous event's entry. A
	// nil plan means the rewrite was incoherent: record the ambiguity
	// truthfully with empty item lists rather than leaving stale evidence.
	evidencePlan := plan
	if evidencePlan == nil {
		kind, mm := classifyHistoryRewrite(req.Messages, snapshot.Messages)
		idx, field := -1, "unknown"
		if mm != nil {
			idx, field = mm.Index, mm.Field
		}
		if kind == "" {
			kind = "rotation"
		}
		evidencePlan = &rewritePlan{classification: kind, mismatchIndex: idx, mismatchField: field, oldURL: snapshot.URL}
	}
	restore := func() {
		*s = snapshot
		_ = h.save()
	}
	// Journal uncertainty before submission, exactly like the normal path.
	h.sessions[id] = s
	s.Dirty = true
	s.Phase = "final"
	s.PendingRequest, s.PendingPrompt = &req, prompt
	s.RepairPendingPrompt = ""
	if err := h.save(); err != nil {
		h.backendError(w, err)
		return true
	}
	thinking := h.forceThinking || (req.ReasoningEffort != nil && req.ReasoningEffort != "none") || (req.Reasoning != nil && req.Reasoning != false)
	log.Printf("[openai] session=%s lane=%s decision=rotation project=%q thread=%s record_turns=%d card_bytes=%d", id, lane, name, projectURL, len(recordTurns), len(prompt))
	// Seed the new project thread record-first, then the rotation card. The
	// first submission mints the project chat (empty URL + project
	// targeting); every later turn continues the minted thread. Record turns
	// are inert history imports: each must ACK, exactly like the normal path.
	threadURL, newProjectURL := "", ""
	seedTurns, seedBytes := 0, 0
	for i, rt := range recordTurns {
		opt := chatgpt.TurnOptions{Prompt: rt.prompt, URL: threadURL, Thinking: thinking}
		if threadURL == "" {
			opt.ProjectURL, opt.ProjectName = projectURL, name
		}
		h.appendSent(sentEntry{Event: "submitted", Session: id, URL: threadURL, PromptHash: promptHash(rt.prompt), Bytes: len(rt.prompt), Chunk: i + 1, Chunks: len(recordTurns) + 1, Note: "rotation-import intent"})
		res, err := h.client.AskThreadTurn(r.Context(), opt)
		h.recordTurnOutcome(id, rt.prompt, threadURL, res, err, i+1, len(recordTurns)+1)
		seedTurns++
		seedBytes += len(rt.prompt)
		if err != nil {
			if !chatgpt.IsPreSubmit(err) && (res.URL != "" || !chatgpt.IsNavigateError(err)) {
				if res.URL != "" {
					s.URL = res.URL
				}
				if serr := h.save(); serr != nil {
					h.backendError(w, serr)
					return true
				}
				h.backendError(w, err)
				return true // submission may have happened: no automatic reset-replay
			}
			// Provably pre-submission for this turn, and every earlier
			// record turn was an inert ACK import: falling back to the
			// shared-project reseed (which replays the same record) cannot
			// duplicate execution.
			log.Printf("[openai] session=%s lane=%s rotation_fallback reason=pre_submission_navigation record_turn=%d; using shared-project reset-replay", id, lane, i+1)
			restore()
			return false
		}
		if res.ProjectURL != "" {
			newProjectURL = res.ProjectURL
		}
		threadURL = res.URL
		s.URL = threadURL
		if err := h.save(); err != nil {
			h.backendError(w, err)
			return true
		}
		if strings.TrimSpace(res.Answer) != "ACK" {
			s.URL = threadURL
			if serr := h.save(); serr != nil {
				h.backendError(w, serr)
				return true
			}
			h.backendError(w, errors.New("rotation record import did not acknowledge with ACK; session remains uncertain"))
			return true
		}
		log.Printf("[openai] session=%s lane=%s rotation record imported chunk=%d/%d thread=%s", id, lane, i+1, len(recordTurns), threadURL)
	}
	cardOpt := chatgpt.TurnOptions{Prompt: prompt, URL: threadURL, Attachments: atts, Thinking: thinking}
	if threadURL == "" {
		cardOpt.ProjectURL, cardOpt.ProjectName = projectURL, name
	}
	// Bind this actual final prompt only after any inert imports complete.
	s.URL, s.Phase, s.PendingPrompt = threadURL, "final", prompt
	if err := h.save(); err != nil {
		h.backendError(w, err)
		return true
	}
	h.appendSent(sentEntry{Event: "submitted", Session: id, URL: threadURL, PromptHash: promptHash(prompt), Bytes: len(prompt), Chunk: len(recordTurns) + 1, Chunks: len(recordTurns) + 1, Note: "rotation-final intent"})
	res, err := h.client.AskThreadTurn(r.Context(), cardOpt)
	h.recordTurnOutcome(id, prompt, threadURL, res, err, len(recordTurns)+1, len(recordTurns)+1)
	// Journal linkage for the rotation final, like the normal loop: the
	// pending prompt is verified against this bubble on read-back.
	if res.BubbleID != "" {
		s.PendingBubbleID = res.BubbleID
	}
	seedTurns++
	seedBytes += len(prompt)
	if err != nil {
		if !chatgpt.IsPreSubmit(err) && (res.URL != "" || !chatgpt.IsNavigateError(err)) {
			if res.URL != "" {
				s.URL = res.URL
			}
			if serr := h.save(); serr != nil {
				h.backendError(w, serr)
				return true
			}
			h.backendError(w, err)
			return true // submission may have happened: no automatic reset-replay
		}
		log.Printf("[openai] session=%s lane=%s rotation_fallback reason=pre_submission_navigation; using shared-project reset-replay", id, lane)
		restore()
		return false
	}
	if res.URL == "" || !strings.Contains(res.URL, "/c/") {
		s.URL = ""
		if serr := h.save(); serr != nil {
			h.backendError(w, serr)
			return true
		}
		h.backendError(w, errors.New("rotation returned no persistent thread URL; submission cannot be safely replayed"))
		return true
	}
	b, _ := json.Marshal(struct {
		Messages []message
		Tools    []toolDef
		Choice   any
	}{req.Messages, req.Tools, req.ToolChoice})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(b))
	out, err := parseAnswer(res.Answer, req)
	if err != nil {
		repaired, state := h.requestFormatRepair(r, id, lane, res.URL, req, res.Answer, thinking)
		if state == repairUncertain || state == repairStoreFailed {
			// Same class and status as the direct-path repair refusal: the bounded
			// repair turn is unverifiable, so the binding is retained and the
			// chat sentence stays actionable instead of naming an endpoint.
			apiError(w, 422, errors.New(failureText("An automatic second attempt at a usable reply could not be confirmed, so this chat still holds an unknown turn.", sayRecoverReset)))
			return true
		}
		if state == repairCompleted {
			res.Answer, res.Reasoning, res.URL = repaired.Answer, repaired.Reasoning, s.URL
			out, err = parseAnswer(res.Answer, req)
		}
	}
	if err != nil {
		// Model-protocol failure, not infra: behave exactly like the legacy
		// parse-error path (persist the thread, terminal 422) rather than resubmitting.
		s.URL = res.URL
		if res.ProjectURL != "" {
			s.Project = res.ProjectURL
		} else if projectURL != "" {
			s.Project = projectURL
		}
		if serr := h.rejectCompleted(s, fingerprint, rejectToolProtocol); serr != nil {
			h.backendError(w, serr)
			return true
		}
		log.Printf("[openai] invalid tool protocol session=%s lane=%s decision=rotation thread=%s", id, lane, res.URL)
		apiError(w, 422, errors.New(failureText(rejectionUnknown(rejectToolProtocol), nextNewMessage)))
		return true
	}
	out.Reasoning = res.Reasoning
	out.Images = res.Images
	out.ID, out.Created = fmt.Sprintf("chatcmpl-%x", sha256.Sum256([]byte(id+":"+fingerprint))), time.Now().Unix()
	for i := range out.Calls {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", id, fingerprint, i)))
		out.Calls[i].ID = fmt.Sprintf("call_%x", sum[:12])
	}
	beforeCommit := *s
	s.Messages = append(append([]message{}, req.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	s.URL = res.URL
	if res.ProjectURL != "" {
		newProjectURL = res.ProjectURL
	}
	if newProjectURL != "" {
		s.Project = newProjectURL
	} else if projectURL != "" {
		s.Project = projectURL
	}
	s.Fingerprint, s.Result, s.Dirty = fingerprint, out, false
	s.ToolsFP = toolsFP
	s.PendingRequest, s.PendingPrompt, s.Phase, s.PendingBubbleID = nil, "", "", ""
	s.RepairPendingPrompt = ""
	// The new thread URL is known only now: overwrite any previous event's
	// entry with this rotation's linkage so evidence never goes stale.
	s.ReseedFrom = evidencePlan.oldURL
	s.ReseedEvidence = evidencePlan.evidence(s.URL, seedTurns, seedBytes) + " via=rotation"
	if err := h.save(); err != nil {
		*s = beforeCommit
		h.backendError(w, err)
		return true
	}
	h.appendSent(sentEntry{Event: "reseed", Session: id, URL: s.URL, PromptHash: promptHash(prompt), Bytes: seedBytes, Note: evidencePlan.note(s.URL) + " via=rotation"})
	log.Printf("[openai] session=%s lane=%s done decision=rotation tool_calls=%d answer_bytes=%d thread=%s project=%s", id, lane, len(out.Calls), len(res.Answer), res.URL, s.Project)
	h.respond(w, req, out)
	return true
}

// eagerThreadTurn submits one turn, starting a new session's FIRST thread in
// its own project when the flag is on (decision=fresh, no stored thread yet)
// instead of the shared project. serveRotation still owns the mismatch path;
// this covers fresh only, so a session lives in its own project from turn
// one. Delta turns and all legacy paths pass through untouched. On mint
// failure before any conversation exists (provably nothing submitted) it
// falls back to the shared project once rather than failing closed. A minted
// project URL is stored on s for the caller to persist with its normal save.
// titleTimeout bounds low-value title turns: a short plain-text naming answer
// must never hog the single browser lane for the full 10-minute turn
// timeout while the real chat waits behind it. Main turns keep r's own
// deadline untouched. The caller defers the returned cancel.
func titleTimeout(r *http.Request, lane string) (context.Context, context.CancelFunc) {
	if lane != "title" {
		return r.Context(), func() {}
	}
	return context.WithTimeout(r.Context(), 3*time.Minute)
}

func (h *handler) eagerThreadTurn(r *http.Request, id, lane string, req completionsRequest, s *session, t turn, page string, thinking bool, decision string, resync bool) (chatgpt.TurnResult, error) {
	opt := chatgpt.TurnOptions{Prompt: t.prompt, URL: page, Attachments: t.atts, Thinking: thinking}
	eager := decision == "fresh" && page == "" && id != "" && !resync && projectPerSessionEnabled()
	if eager {
		if name := sessionProjectName(id, req); name != "" {
			opt.ProjectURL, opt.ProjectName = s.Project, name
		} else {
			eager = false
		}
	}
	tctx, cancel := titleTimeout(r, lane)
	defer cancel()
	res, err := h.client.AskThreadTurn(tctx, opt)
	if err != nil && eager && res.URL == "" && s.URL == "" && chatgpt.IsNavigateError(err) {
		log.Printf("[openai] session=%s lane=%s eager_fallback reason=%v; using shared project", id, lane, err)
		eager = false
		res, err = h.client.AskThreadTurn(tctx, chatgpt.TurnOptions{Prompt: t.prompt, URL: page, Attachments: t.atts, Thinking: thinking})
	}
	if err == nil && eager && res.ProjectURL != "" {
		s.Project = res.ProjectURL
		log.Printf("[openai] session=%s lane=%s decision=fresh-project project=%s thread=%s", id, lane, res.ProjectURL, res.URL)
	}
	return res, err
}
