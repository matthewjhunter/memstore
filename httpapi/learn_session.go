package httpapi

import (
	"sync"
	"time"
)

// learnedFactRef tracks a fact produced during a learn session.
type learnedFactRef struct {
	FactID  int64
	Surface string // "file", "doc", "symbol", "section"
	RelPath string // relative file path
	Subject string // fact subject
}

// learnSession accumulates facts produced during a learn session
// so that cross-file links can be created at finalize time.
type learnSession struct {
	facts    []learnedFactRef
	subject  string // project subject name
	lastSeen time.Time
}

// LearnSessionStore tracks learn sessions with TTL-based cleanup.
type LearnSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*learnSession
	done     chan struct{}
	wg       sync.WaitGroup
}

const learnSessionExpiry = 30 * time.Minute

// NewLearnSessionStore creates a session store with background cleanup.
func NewLearnSessionStore() *LearnSessionStore {
	ls := &LearnSessionStore{
		sessions: make(map[string]*learnSession),
		done:     make(chan struct{}),
	}
	ls.wg.Add(1)
	go ls.cleanupLoop()
	return ls
}

// Stop terminates the background cleanup goroutine.
func (ls *LearnSessionStore) Stop() {
	close(ls.done)
	ls.wg.Wait()
}

// Record adds learned fact references to a session.
func (ls *LearnSessionStore) Record(sessionID, subject string, refs []learnedFactRef) {
	if sessionID == "" || len(refs) == 0 {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()

	sess, ok := ls.sessions[sessionID]
	if !ok {
		sess = &learnSession{subject: subject}
		ls.sessions[sessionID] = sess
	}
	sess.lastSeen = time.Now()
	sess.facts = append(sess.facts, refs...)
}

// Consume retrieves and removes all facts for a session. Returns nil if not found.
func (ls *LearnSessionStore) Consume(sessionID string) *learnSession {
	if sessionID == "" {
		return nil
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()

	sess, ok := ls.sessions[sessionID]
	if !ok {
		return nil
	}
	delete(ls.sessions, sessionID)
	return sess
}

func (ls *LearnSessionStore) cleanupLoop() {
	defer ls.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ls.done:
			return
		case <-ticker.C:
			ls.expire()
		}
	}
}

func (ls *LearnSessionStore) expire() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	cutoff := time.Now().Add(-learnSessionExpiry)
	for id, sess := range ls.sessions {
		if sess.lastSeen.Before(cutoff) {
			delete(ls.sessions, id)
		}
	}
}
