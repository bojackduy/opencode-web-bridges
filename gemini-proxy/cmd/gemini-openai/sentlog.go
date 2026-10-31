package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojackduy/gemini-proxy/internal/gemini"
)

// Append-only sent-log: every thread submission intent is recorded BEFORE
// submit (event=submitted) with a completion record after
// (event=completed|interrupted|failed). The recovery path uses it to
// distinguish, in one lookup, "pending-overwritten-by-retry" desync (the
// thread holds an earlier submission H1 while the pending prompt is a newer
// H3) from genuinely-unknown thread state.
//
// The log lives alongside the session state file (same directory,
// sent-log.jsonl) plus a bounded in-memory ring so diagnosis works even when
// no state path is configured (tests) or the file is unavailable. Entries
// record hashes and lengths only, never prompt or answer content.
//
// Chunk lineage: the Gemini submit path is currently single-turn (one prompt
// per Ask; over-budget turns are rejected with 413, never split), so every
// entry records chunk 1 of 1. If boundary-aware chunking ever lands here,
// each chunk must be logged with its own index/count exactly like the
// ChatGPT bridge, and splits must land on tool-wrapper boundaries only.
type sentEntry struct {
	Event      string `json:"event"`
	Session    string `json:"session"`
	URL        string `json:"url,omitempty"`
	PromptHash string `json:"promptHash"`
	Bytes      int    `json:"bytes"`
	Chunk      int    `json:"chunk,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`
	At         string `json:"at"`
	AnswerHash string `json:"answerHash,omitempty"`
	// Note carries free-form linkage for rare events (reseed old→new
	// thread plus classification and item lists). Hashes and IDs only,
	// never prompt or answer content.
	Note string `json:"note,omitempty"`
}

// maxSentLogMemory bounds the in-memory ring; the file remains the durable
// record and diagnosis falls back to its tail when memory has nothing.
const maxSentLogMemory = 2000

// promptHash identifies a submitted prompt exactly the way adoption compares
// it (normalized, then hashed): the thread-side reading goes through the
// same normalization, so hash lineage compares equal. See
// gemini.PromptHash.
func promptHash(prompt string) string {
	return gemini.PromptHash(prompt)
}

func shortHash12(full string) string {
	if len(full) > 12 {
		return full[:12]
	}
	return full
}

func (h *handler) sentLogPath() string {
	if h.statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.statePath), "sent-log.jsonl")
}

func (h *handler) appendSent(e sentEntry) {
	e.At = time.Now().UTC().Format(time.RFC3339)
	h.sentLog = append(h.sentLog, e)
	if len(h.sentLog) > maxSentLogMemory {
		h.sentLog = append([]sentEntry(nil), h.sentLog[len(h.sentLog)-maxSentLogMemory:]...)
	}
	path := h.sentLogPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		log.Printf("[openai] sent-log mkdir: %v", err)
		return
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("[openai] sent-log append: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("[openai] sent-log write: %v", err)
	}
}

// sentLogTail parses the durable file tail (last n lines), oldest first.
// It is the shared reader behind file-fallback lookups so every check
// survives a restart that loaded the journal but not the memory ring.
func (h *handler) sentLogTail(n int) []sentEntry {
	path := h.sentLogPath()
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	var out []sentEntry
	for _, ln := range lines[start:] {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e sentEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// lastSentFor returns the most recent entry for a session, scanning memory
// first and the durable file tail (last 300 lines) on a miss — e.g. after a
// restart that loaded the journal but not the ring.
func (h *handler) lastSentFor(session string) *sentEntry {
	for i := len(h.sentLog) - 1; i >= 0; i-- {
		if h.sentLog[i].Session == session {
			e := h.sentLog[i]
			return &e
		}
	}
	path := h.sentLogPath()
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	start := 0
	if len(lines) > 300 {
		start = len(lines) - 300
	}
	var last *sentEntry
	for _, ln := range lines[start:] {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e sentEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil || e.Session != session {
			continue
		}
		c := e
		last = &c
	}
	return last
}

// sentEntryForThread returns the most recent entry proving this session
// submitted to the given thread URL (memory ring first, durable file tail on
// a miss). A nil return means the thread cannot be tied to this session.
func (h *handler) sentEntryForThread(session, url string) *sentEntry {
	for i := len(h.sentLog) - 1; i >= 0; i-- {
		if h.sentLog[i].Session == session && h.sentLog[i].URL == url {
			e := h.sentLog[i]
			return &e
		}
	}
	path := h.sentLogPath()
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	start := 0
	if len(lines) > 300 {
		start = len(lines) - 300
	}
	var last *sentEntry
	for _, ln := range lines[start:] {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e sentEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil || e.Session != session || e.URL != url {
			continue
		}
		c := e
		last = &c
	}
	return last
}

// sentDiagnosis reports, in one lookup, whether the pending prompt matches
// the last recorded submission for this session. A mismatch is the signature
// of pending-overwritten-by-retry desync: the thread holds the earlier
// submission, not the pending prompt, so read-only adoption against pending
// fails structurally while the earlier turn may be complete and recoverable.
// The returned line always starts with "sent-log:" and is wired into every
// 409/422 body on the Dirty (Pending) path.
func (h *handler) sentDiagnosis(id string, s *session) string {
	pending := "none"
	if s != nil && s.Pending {
		if s.PendingPrompt != "" {
			pending = shortHash12(promptHash(s.PendingPrompt))
		} else {
			pending = "present (unrecorded legacy pending)"
		}
	}
	if s != nil && !s.Pending && s.PendingPrompt == "" {
		if last := h.lastSentFor(id); last != nil {
			return fmt.Sprintf("sent-log: no pending prompt (session clean); last submit %s (%s %s)", shortHash12(last.PromptHash), last.Event, last.At)
		}
		return "sent-log: no pending prompt and no submission recorded"
	}
	if s != nil && s.URL != "" {
		if last := h.lastSentFor(id); last != nil {
			short := shortHash12(last.PromptHash)
			if s.PendingPrompt != "" && last.PromptHash == promptHash(s.PendingPrompt) {
				return fmt.Sprintf("sent-log: last submit %s == pending %s (consistent; thread %s)", short, pending, last.URL)
			}
			return fmt.Sprintf("sent-log: MISMATCH last submit %s (%dB chunk %d/%d %s %s) vs pending %s — pending-overwritten-by-retry desync likely: thread may hold the earlier submission, not the pending prompt", short, last.Bytes, last.Chunk, last.Chunks, last.Event, last.At, pending)
		}
	}
	if last := h.lastSentFor(id); last != nil {
		return fmt.Sprintf("sent-log: last submit %s (%s %s) with no stored thread URL; vs pending %s", shortHash12(last.PromptHash), last.Event, last.At, pending)
	}
	return "sent-log: no submission recorded for this session (genuinely unknown thread state)"
}
