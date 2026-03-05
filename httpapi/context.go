package httpapi

import (
	"sync"
	"time"
)

const (
	maxRecentFiles    = 20
	sessionIdleExpiry = 1 * time.Hour
	cleanupInterval   = 5 * time.Minute
)

// SessionContext tracks per-session file access for recall context boosting.
// It maintains an in-memory ring buffer of recently touched files per session.
type SessionContext struct {
	mu       sync.Mutex
	sessions map[string]*sessionState
	done     chan struct{}
	wg       sync.WaitGroup
}

type sessionState struct {
	files    []string
	lastSeen time.Time
}

// NewSessionContext creates a context tracker with background cleanup of idle sessions.
func NewSessionContext() *SessionContext {
	sc := &SessionContext{
		sessions: make(map[string]*sessionState),
		done:     make(chan struct{}),
	}
	sc.wg.Add(1)
	go sc.cleanupLoop()
	return sc
}

// Stop terminates the background cleanup goroutine.
func (sc *SessionContext) Stop() {
	close(sc.done)
	sc.wg.Wait()
}

// TouchFiles records file paths as recently accessed for a session.
func (sc *SessionContext) TouchFiles(sessionID string, files []string) {
	if sessionID == "" || len(files) == 0 {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()

	state, ok := sc.sessions[sessionID]
	if !ok {
		state = &sessionState{}
		sc.sessions[sessionID] = state
	}
	state.lastSeen = time.Now()

	for _, f := range files {
		// Remove duplicates — if already present, move to end.
		for i, existing := range state.files {
			if existing == f {
				state.files = append(state.files[:i], state.files[i+1:]...)
				break
			}
		}
		state.files = append(state.files, f)
	}

	// Trim to ring buffer size.
	if len(state.files) > maxRecentFiles {
		state.files = state.files[len(state.files)-maxRecentFiles:]
	}
}

// RecentFiles returns the recently accessed files for a session.
func (sc *SessionContext) RecentFiles(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()

	state, ok := sc.sessions[sessionID]
	if !ok {
		return nil
	}
	out := make([]string, len(state.files))
	copy(out, state.files)
	return out
}

func (sc *SessionContext) cleanupLoop() {
	defer sc.wg.Done()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sc.done:
			return
		case <-ticker.C:
			sc.expireSessions()
		}
	}
}

func (sc *SessionContext) expireSessions() {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	cutoff := time.Now().Add(-sessionIdleExpiry)
	for id, state := range sc.sessions {
		if state.lastSeen.Before(cutoff) {
			delete(sc.sessions, id)
		}
	}
}
