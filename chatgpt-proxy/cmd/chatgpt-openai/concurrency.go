package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"chatgpt-proxy/internal/chatgpt"
)

// Committed sessions are immutable. Each active transaction owns its session
// clone and backend for the entire replay/final/recovery operation. The short
// store lock protects snapshots and atomic file replacement, never browser work.
type parallelState struct {
	mu      sync.Mutex
	clients chan backend
	locks   map[string]*sessionLock
}

type sessionLock struct {
	gate chan struct{}
	refs int
}

func (h *handler) enableParallel(clients []backend) {
	p := &parallelState{clients: make(chan backend, len(clients)), locks: make(map[string]*sessionLock)}
	for _, client := range clients {
		p.clients <- client
	}
	h.parallel = p
}

// releaseSession drops one reference to a session lock and removes it when
// no transaction holds or awaits it.
func (p *parallelState) releaseSession(id string, l *sessionLock) {
	p.mu.Lock()
	l.refs--
	if l.refs == 0 {
		delete(p.locks, id)
	}
	p.mu.Unlock()
}

func (p *parallelState) lockSession(ctx context.Context, id string) (func(), bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	p.mu.Lock()
	l := p.locks[id]
	if l == nil {
		l = &sessionLock{gate: make(chan struct{}, 1)}
		p.locks[id] = l
	}
	l.refs++
	p.mu.Unlock()
	select {
	case l.gate <- struct{}{}:
		return func() { <-l.gate; p.releaseSession(id, l) }, true
	case <-ctx.Done():
		p.releaseSession(id, l)
		return nil, false
	}
}

// tryLockSession takes this session's turn lock WITHOUT waiting. The operator
// abandonment path uses it so it can never queue behind, or interleave with, a
// live turn on the same session: a queued clear that lands after the turn
// commits would silently undo the abandonment.
func (p *parallelState) tryLockSession(id string) (func(), bool) {
	p.mu.Lock()
	l := p.locks[id]
	if l == nil {
		l = &sessionLock{gate: make(chan struct{}, 1)}
		p.locks[id] = l
	}
	l.refs++
	p.mu.Unlock()
	select {
	case l.gate <- struct{}{}:
		return func() { <-l.gate; p.releaseSession(id, l) }, true
	default:
		p.releaseSession(id, l)
		return nil, false
	}
}

func cloneSession(s *session) (*session, error) {
	if s == nil {
		return nil, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var copy session
	err = json.Unmarshal(b, &copy)
	return &copy, err
}

// lockTurn takes the cross-process browser guard for this route. Parallel
// transactions already hold the shared owned-turn guard from transactionRoute,
// so they must re-acquire it shared: an exclusive LockTurn here would block
// behind our own shared hold (same-process flock fds conflict) and two
// transactions would wedge each other upgrading SH->EX. Serial mode keeps the
// exclusive whole-turn guard.
func (h *handler) lockTurn(ctx context.Context) (func(), error) {
	if h.transactionOwner != nil {
		return chatgpt.LockOwnedTurn(ctx, h.profileDir)
	}
	return chatgpt.LockTurn(ctx, h.profileDir)
}

func (h *handler) transactionRoute(next func(*handler, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.parallel == nil {
			next(h, w, r)
			return
		}
		id, _, err := completionSessionID(r)
		if err != nil {
			apiError(w, 400, err)
			return
		}
		p := h.parallel
		unlock, ok := p.lockSession(r.Context(), id)
		if !ok {
			return
		}
		defer unlock()
		guard, err := chatgpt.LockOwnedTurn(r.Context(), h.profileDir)
		if err != nil {
			apiError(w, 503, err)
			return
		}
		defer guard()
		var client backend
		select {
		case client = <-p.clients:
		case <-r.Context().Done():
			return
		}
		defer func() { p.clients <- client }()
		if r.Context().Err() != nil {
			return
		}
		local := &handler{client: client, model: h.model, maxChars: h.maxChars,
			forceThinking: h.forceThinking, gate: make(chan struct{}, 1),
			transactionOwner: h, transactionID: id, sessions: make(map[string]*session)}
		p.mu.Lock()
		for key, s := range h.sessions {
			local.sessions[key] = s // immutable committed snapshots
		}
		local.sessions[id], err = cloneSession(h.sessions[id])
		// Legacy root-key inheritance can later become this child's writable
		// journal. Clone that source too, so its nested history never aliases
		// the immutable root snapshot during the inherited continuation.
		legacy := r.Header.Get("X-Session-Id")
		if err == nil && legacy != id {
			local.sessions[legacy], err = cloneSession(h.sessions[legacy])
			if local.sessions[legacy] == nil {
				delete(local.sessions, legacy)
			}
		}
		p.mu.Unlock()
		if err != nil {
			apiError(w, 500, err)
			return
		}
		// Removing a nil entry preserves the existing new-session path. The
		// local profileDir stays empty: owned clients coordinate setup, not turns.
		if local.sessions[id] == nil {
			delete(local.sessions, id)
		}
		next(local, w, r)
	}
}

func (h *handler) commitSession() error {
	owner := h.transactionOwner
	copy, err := cloneSession(h.sessions[h.transactionID])
	if err != nil {
		return err
	}
	p := owner.parallel
	p.mu.Lock()
	defer p.mu.Unlock()
	old, existed := owner.sessions[h.transactionID]
	if owner.sessions == nil {
		owner.sessions = make(map[string]*session)
	}
	owner.sessions[h.transactionID] = copy
	if err := owner.save(); err != nil {
		if existed {
			owner.sessions[h.transactionID] = old
		} else {
			delete(owner.sessions, h.transactionID)
		}
		return err
	}
	return nil
}
