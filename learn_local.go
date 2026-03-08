package memstore

import (
	"context"
	"strings"
	"sync"
)

// LocalLearner implements Learner for embedded mode (direct store access).
// It wraps CodebaseLearner for per-file learning and tracks session state
// in-process for finalize synthesis.
type LocalLearner struct {
	cl   *CodebaseLearner
	mu   sync.Mutex
	sess map[string]*localSession
}

type localSession struct {
	facts []LearnedFactRef
}

// NewLocalLearner creates a Learner that processes files locally using the
// given store, embedder, and generator. Used when there is no memstored daemon.
func NewLocalLearner(store Store, embedder Embedder, generator Generator) *LocalLearner {
	return &LocalLearner{
		cl:   NewCodebaseLearner(store, embedder, generator),
		sess: make(map[string]*localSession),
	}
}

func (ll *LocalLearner) LearnFile(ctx context.Context, opts LearnFileOpts) (*LearnFileResult, error) {
	result, err := ll.cl.LearnFile(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Record in session if not skipped.
	if opts.SessionID != "" && !result.Skipped {
		ll.recordSession(opts, result)
	}

	return result, nil
}

func (ll *LocalLearner) LearnFinalize(ctx context.Context, opts LearnFinalizeOpts) (*LearnFinalizeResult, error) {
	sess := ll.consumeSession(opts.SessionID)
	if sess == nil || len(sess.facts) == 0 {
		return &LearnFinalizeResult{}, nil
	}

	return SynthesizeSession(ctx, ll.cl.store, ll.cl.embedder, ll.cl.generator, sess.facts, opts)
}

func (ll *LocalLearner) recordSession(opts LearnFileOpts, result *LearnFileResult) {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	sess, ok := ll.sess[opts.SessionID]
	if !ok {
		sess = &localSession{}
		ll.sess[opts.SessionID] = sess
	}

	surface := "file"
	ext := strings.ToLower(opts.FilePath)
	if strings.HasSuffix(ext, ".md") || strings.HasSuffix(ext, ".markdown") {
		surface = "doc"
	}

	if result.FileFactID != 0 {
		sess.facts = append(sess.facts, LearnedFactRef{
			FactID:  result.FileFactID,
			Surface: surface,
			RelPath: opts.FilePath,
		})
	}
	for _, symID := range result.SymbolIDs {
		sess.facts = append(sess.facts, LearnedFactRef{
			FactID:  symID,
			Surface: "symbol",
			RelPath: opts.FilePath,
		})
	}
}

func (ll *LocalLearner) consumeSession(sessionID string) *localSession {
	if sessionID == "" {
		return nil
	}
	ll.mu.Lock()
	defer ll.mu.Unlock()

	sess, ok := ll.sess[sessionID]
	if !ok {
		return nil
	}
	delete(ll.sess, sessionID)
	return sess
}
