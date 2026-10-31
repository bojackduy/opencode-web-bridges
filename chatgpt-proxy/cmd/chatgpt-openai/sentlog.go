package main

import (
	"chatgpt-proxy/internal/chatgpt"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Append-only sent-log: every thread submission intent is recorded BEFORE
// submit (event=submitted) with a completion record after
// (event=completed|interrupted|failed). The recovery path uses it to
// distinguish, in one lookup, "pending-overwritten-by-retry" desync (the
// thread holds an earlier submission H1 while the journal pending prompt is
// a newer H3) from genuinely-unknown thread state.
//
// The log lives alongside the session state file (same directory,
// sent-log.jsonl) plus a bounded in-memory ring so diagnosis works even when
// no state path is configured (tests) or the file is unavailable. Entries
// record hashes and lengths only, never prompt or answer content.
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
// chatgpt.PromptHash.
func promptHash(prompt string) string {
	return chatgpt.PromptHash(prompt)
}

func shortHash12(full string) string {
	if len(full) > 12 {
		return full[:12]
	}
	return full
}

func (h *handler) sentLogPath() string {
	if h.transactionOwner != nil {
		return h.transactionOwner.sentLogPath()
	}
	if h.statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.statePath), "sent-log.jsonl")
}

func (h *handler) appendSent(e sentEntry) {
	if h.transactionOwner != nil {
		h.transactionOwner.appendSent(e)
		return
	}
	if h.parallel != nil {
		h.parallel.mu.Lock()
		defer h.parallel.mu.Unlock()
	}
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
		return
	}
	if err := f.Sync(); err != nil {
		log.Printf("[openai] sent-log sync: %v", err)
	}
}

// Parallel handlers are short-lived request snapshots. The durable log and
// memory ring belong to their shared owner, just like the committed journal.
func (h *handler) sentEntries() []sentEntry {
	if h.transactionOwner != nil {
		return h.transactionOwner.sentEntries()
	}
	if h.parallel != nil {
		h.parallel.mu.Lock()
		defer h.parallel.mu.Unlock()
	}
	return append([]sentEntry(nil), h.sentLog...)
}

func (h *handler) recordTurnOutcome(id, prompt, previousURL string, res chatgpt.TurnResult, err error, chunk, chunks int) {
	disposition, answer := "completed", ""
	if err != nil {
		disposition = "interrupted"
		if chatgpt.IsPreSubmit(err) {
			disposition = "failed"
		}
	} else {
		answer = promptHash(res.Answer)
	}
	url := res.URL
	if url == "" {
		url = previousURL
	}
	h.appendSent(sentEntry{Event: disposition, Session: id, URL: url, PromptHash: promptHash(prompt), Bytes: len(prompt), Chunk: chunk, Chunks: chunks, AnswerHash: answer})
}

// sentLogTail parses the durable file tail (last n lines), oldest first.
// It is the shared reader behind file-fallback diagnosis so every lookup
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
// first and the durable log on a miss — e.g. after a restart. An old session's
// record must not disappear merely because other sessions made 300 entries.
func (h *handler) lastSentFor(session string) *sentEntry {
	entries := h.sentEntries()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Session == session {
			e := entries[i]
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
	var last *sentEntry
	for _, ln := range lines {
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

// sentDiagnosis reports, in one lookup, whether the pending prompt matches
// the last recorded submission for this session. A mismatch is the signature
// of pending-overwritten-by-retry desync: the thread holds the earlier
// submission, not the pending prompt, so read-only adoption against pending
// fails structurally while the earlier turn may be complete and recoverable.
// sentEntryForThread returns the most recent entry proving this session
// submitted to the given thread URL (memory ring first, durable file tail on
// a miss). A nil return means the thread cannot be tied to this session.
func (h *handler) sentEntryForThread(session, url string) *sentEntry {
	entries := h.sentEntries()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Session == session && entries[i].URL == url {
			e := entries[i]
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
	var last *sentEntry
	for _, ln := range lines {
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

func (h *handler) sentDiagnosis(id string, s *session) string {
	pending := "none"
	if s != nil && s.PendingPrompt != "" {
		pending = shortHash12(promptHash(s.PendingPrompt))
	}
	if s != nil && !s.Dirty && s.PendingPrompt == "" {
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
	return "sent-log: no submission recorded in the durable log; legacy or previously unlogged path is possible; outcome needs validation"
}
