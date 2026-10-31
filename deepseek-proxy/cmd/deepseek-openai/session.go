package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	planAppend = "append"
	planReseed = "reseed"
	planRetry  = "retry"
)

// session is the bridge's record of one OpenCode session. Messages is the last
// committed canonical history; the client's history is authoritative and any
// divergence simply reseeds, so there is no uncertain state to repair.
type session struct {
	Remote    string        `json:"remote,omitempty"`
	Parent    string        `json:"parent,omitempty"`
	Messages  []chatMessage `json:"messages,omitempty"`
	Pending   string        `json:"pending,omitempty"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type journalFile struct {
	Version  int                 `json:"version"`
	Sessions map[string]*session `json:"sessions"`
}

type journal struct {
	path     string
	mu       sync.Mutex
	busy     map[string]bool
	sessions map[string]*session
}

func openJournal(path string) (*journal, error) {
	j := &journal{path: path, busy: map[string]bool{}, sessions: map[string]*session{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	var f journalFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("session journal %s is corrupt: %w", path, err)
	}
	if f.Sessions != nil {
		j.sessions = f.Sessions
	}
	return j, nil
}

// acquire takes the per-session turn slot. One turn per session at a time keeps
// the append chain deterministic.
func (j *journal) acquire(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.busy[id] {
		return false
	}
	j.busy[id] = true
	return true
}

func (j *journal) release(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.busy, id)
}

// snapshot returns a deep copy so a turn never reads a half-written session.
func (j *journal) snapshot(id string) session {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.sessions[id]
	if s == nil {
		return session{}
	}
	out := *s
	out.Messages = append([]chatMessage(nil), s.Messages...)
	return out
}

func (j *journal) put(id string, s session) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	s.UpdatedAt = time.Now().UTC()
	j.sessions[id] = &s
	return j.persistLocked()
}

func (j *journal) reset(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id == "" {
		j.sessions = map[string]*session{}
	} else {
		delete(j.sessions, id)
	}
	return j.persistLocked()
}

func (j *journal) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.sessions)
}

func (j *journal) persistLocked() error {
	if j.path == "" {
		return nil
	}
	raw, err := json.Marshal(journalFile{Version: 1, Sessions: j.sessions})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return err
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	return nil
}

// plan describes how the next turn reaches DeepSeek.
type plan struct {
	Mode   string
	Remote string
	Parent string
	Prompt string
	Seal   string
}

// planTurn decides append vs reseed vs retry. Rules:
//   - retry:  same history as committed, with an unresolved turn pending.
//   - append: remote thread exists and the client added messages on top of the
//     committed prefix.
//   - reseed: everything else (first turn, revert, fork, model switch, compact,
//     drift). A fresh remote session gets the whole rendered history.
func planTurn(s session, msgs []chatMessage, tools []toolDef, choice any) plan {
	seal := fingerprint(msgs)
	if s.Pending != "" {
		if seal == s.Pending {
			return plan{Mode: planRetry, Seal: seal}
		}
		// A different request after an unresolved turn abandons the unknown
		// branch and starts clean: the client history is authoritative.
		return plan{Mode: planReseed, Prompt: renderReseed(msgs, tools, choice), Seal: seal}
	}
	if s.Remote != "" && len(msgs) > len(s.Messages) && prefix(msgs, s.Messages) {
		// Append only when the delta fits the remote validation budget. An
		// oversized append (e.g. a large tool result) is promoted to a reseed,
		// which trims oldest history instead of wedging the thread in 409s.
		if prompt := renderAppend(msgs[len(s.Messages):], tools, choice); len(prompt) <= maxReseedPromptBytes {
			return plan{
				Mode:   planAppend,
				Remote: s.Remote,
				Parent: s.Parent,
				Prompt: prompt,
				Seal:   seal,
			}
		}
	}
	return plan{Mode: planReseed, Prompt: renderReseed(msgs, tools, choice), Seal: seal}
}

func fingerprint(msgs []chatMessage) string {
	raw, _ := json.Marshal(msgs)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:12])
}

// commit records a completed turn. parent is the remote assistant message id;
// when it is missing the thread is dropped so the next turn reseeds instead of
// guessing a parent.
func (j *journal) commit(id string, s session, msgs []chatMessage, reply string, calls []toolCall, remote, parent string) error {
	next := session{
		Remote:   remote,
		Parent:   parent,
		Messages: append(append([]chatMessage(nil), msgs...), assistantMessage(reply, calls)),
	}
	if parent == "" {
		next.Remote = ""
	}
	if err := j.put(id, next); err != nil {
		log.Printf("[deepseek] session=%s journal write failed messages=%d", id, len(next.Messages))
		return err
	}
	return nil
}

func assistantMessage(reply string, calls []toolCall) chatMessage {
	return chatMessage{Role: "assistant", Content: text(reply), ToolCalls: calls}
}
