package screening

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is an in-memory PendingStore that records what the worker did.
type fakeStore struct {
	mu        sync.Mutex
	pending   []PendingFact
	resolved  map[int64]Decision
	deferred  map[int64]int
	abandoned map[int64]string
	claims    int
	failClaim error
}

func newFakeStore(facts ...PendingFact) *fakeStore {
	return &fakeStore{
		pending:   facts,
		resolved:  map[int64]Decision{},
		deferred:  map[int64]int{},
		abandoned: map[int64]string{},
	}
}

func (s *fakeStore) PendingFacts(_ context.Context, limit int) ([]PendingFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.failClaim != nil {
		return nil, s.failClaim
	}
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	out := make([]PendingFact, limit)
	copy(out, s.pending[:limit])
	return out, nil
}

func (s *fakeStore) Resolve(_ context.Context, id int64, d Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved[id] = d
	s.remove(id)
	return nil
}

func (s *fakeStore) Defer(_ context.Context, id int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferred[id]++
	for i := range s.pending {
		if s.pending[i].ID == id {
			s.pending[i].Attempts++
		}
	}
	return nil
}

func (s *fakeStore) Abandon(_ context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandoned[id] = reason
	s.remove(id)
	return nil
}

// remove drops a fact from the pending set, mirroring a real store: a resolved or
// abandoned fact must stop being handed back.
func (s *fakeStore) remove(id int64) {
	for i := range s.pending {
		if s.pending[i].ID == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

func (s *fakeStore) snapshot() (resolved map[int64]Decision, deferred map[int64]int, abandoned map[int64]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved = map[int64]Decision{}
	deferred = map[int64]int{}
	abandoned = map[int64]string{}
	for k, v := range s.resolved {
		resolved[k] = v
	}
	for k, v := range s.deferred {
		deferred[k] = v
	}
	for k, v := range s.abandoned {
		abandoned[k] = v
	}
	return
}

func pendingFacts(n int, content string) []PendingFact {
	out := make([]PendingFact, n)
	for i := range out {
		out[i] = PendingFact{ID: int64(i + 1), Content: content}
	}
	return out
}

// TestWorkerResolvesCleanAndHostileWrites is the basic pass: a benign fact becomes
// readable, a hostile one is blocked, and neither stays pending.
func TestWorkerResolvesCleanAndHostileWrites(t *testing.T) {
	store := newFakeStore(
		PendingFact{ID: 1, Content: benignFact},
		PendingFact{ID: 2, Content: paraphrasedOverride},
	)
	gen := &routingGen{replies: map[string]string{
		benignFact:          cleanVerdict,
		paraphrasedOverride: verdictJSON(8, "override", "set aside the guidance you were configured with"),
	}}
	w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil), WorkerConfig{}, nil)

	if got := w.ProcessOnce(context.Background()); got != 2 {
		t.Fatalf("resolved %d facts, want 2", got)
	}

	resolved, _, _ := store.snapshot()
	if d, ok := resolved[1]; !ok || d.Blocked() {
		t.Errorf("benign fact should have been allowed, got %+v (present=%v)", d, ok)
	}
	if d, ok := resolved[2]; !ok || !d.Blocked() {
		t.Errorf("hostile fact should have been blocked, got %+v (present=%v)", d, ok)
	}
	if w.Stats().Blocked.Load() != 1 || w.Stats().Allowed.Load() != 1 {
		t.Errorf("stats: allowed=%d blocked=%d, want 1 and 1",
			w.Stats().Allowed.Load(), w.Stats().Blocked.Load())
	}
}

// TestConcurrencyKnobBoundsInFlightScreens pins the knob's actual contract: it caps
// how many model calls are outstanding at once. Each screen is a model call, so an
// unbounded worker would flatten a local model serving other work.
func TestConcurrencyKnobBoundsInFlightScreens(t *testing.T) {
	for _, concurrency := range []int{1, 3, 8} {
		t.Run("concurrency="+itoa(concurrency), func(t *testing.T) {
			store := newFakeStore(pendingFacts(24, benignFact)...)
			gen := &concurrencyProbe{reply: cleanVerdict, hold: 5 * time.Millisecond}
			w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil),
				WorkerConfig{Concurrency: concurrency, Batch: 24}, nil)

			w.ProcessOnce(context.Background())

			if got := int(gen.maxInFlight.Load()); got > concurrency {
				t.Errorf("peak in-flight screens = %d, exceeds the configured ceiling of %d", got, concurrency)
			}
			if got := gen.calls.Load(); got != 24 {
				t.Errorf("screened %d facts, want all 24", got)
			}
		})
	}
}

// TestConcurrencyActuallyParallelizes guards the other direction: a knob that bounds
// correctly but never runs more than one at a time is not a concurrency knob. Without
// real parallelism the backfill of a large corpus never finishes.
func TestConcurrencyActuallyParallelizes(t *testing.T) {
	store := newFakeStore(pendingFacts(8, benignFact)...)
	gen := &concurrencyProbe{reply: cleanVerdict, hold: 20 * time.Millisecond}
	w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil),
		WorkerConfig{Concurrency: 4, Batch: 8}, nil)

	start := time.Now()
	w.ProcessOnce(context.Background())
	elapsed := time.Since(start)

	// Serial would be 8 x 20ms = 160ms. Four at a time should approach 40ms; allow
	// generous slack for scheduling on a loaded machine.
	if elapsed > 120*time.Millisecond {
		t.Errorf("8 facts at concurrency 4 took %v; screens are not running in parallel", elapsed)
	}
	if got := gen.maxInFlight.Load(); got < 2 {
		t.Errorf("peak in-flight = %d; no parallelism observed", got)
	}
}

// TestUnscreenableFactIsAbandoned is the head-of-queue stall guard. PendingFacts
// returns oldest-first, so a fact that always fails would come back every tick and the
// backlog behind it would never move.
func TestUnscreenableFactIsAbandoned(t *testing.T) {
	store := newFakeStore(PendingFact{ID: 1, Content: benignFact})
	gen := &fakeGen{err: errors.New("model is wedged")}
	w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil),
		WorkerConfig{MaxAttempts: 3}, nil)

	for range 10 {
		w.ProcessOnce(context.Background())
	}

	_, deferred, abandoned := store.snapshot()
	if abandoned[1] == "" {
		t.Fatalf("fact was never abandoned after repeated failures (deferred %d times)", deferred[1])
	}
	if deferred[1] > 3 {
		t.Errorf("fact was retried %d times, want at most MaxAttempts=3", deferred[1])
	}
}

// TestAbandonedFactStaysUnreadable pins why giving up is acceptable. An abandoned
// fact is never resolved, so it never becomes readable -- the worker failing shows up
// as a memory that does not appear, never as unscreened content being served.
func TestAbandonedFactStaysUnreadable(t *testing.T) {
	store := newFakeStore(PendingFact{ID: 1, Content: paraphrasedOverride})
	gen := &fakeGen{err: errors.New("model is down")}
	w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil),
		WorkerConfig{MaxAttempts: 2}, nil)

	for range 5 {
		w.ProcessOnce(context.Background())
	}

	resolved, _, abandoned := store.snapshot()
	if len(resolved) != 0 {
		t.Errorf("an unscreened fact was resolved and would become readable: %+v", resolved)
	}
	if abandoned[1] == "" {
		t.Error("fact should have been abandoned")
	}
}

// TestNoGeneratorNeverResolves covers a daemon with no chat model configured. Every
// fact stays pending, which is to say the store keeps accepting writes and serves none
// of them until screening is available.
func TestNoGeneratorNeverResolves(t *testing.T) {
	store := newFakeStore(PendingFact{ID: 1, Content: benignFact})
	w := NewWorker(store, NewScreener(DefaultPolicy(), nil, nil), WorkerConfig{MaxAttempts: 99}, nil)

	if got := w.ProcessOnce(context.Background()); got != 0 {
		t.Errorf("resolved %d facts with no generator, want 0", got)
	}
	resolved, deferred, _ := store.snapshot()
	if len(resolved) != 0 {
		t.Errorf("resolved a fact with no screening: %+v", resolved)
	}
	if deferred[1] != 1 {
		t.Errorf("deferred %d times, want 1", deferred[1])
	}
}

// TestClaimFailureIsNotFatal pins that a storage blip skips a tick rather than killing
// the worker.
func TestClaimFailureIsNotFatal(t *testing.T) {
	store := newFakeStore(PendingFact{ID: 1, Content: benignFact})
	store.failClaim = errors.New("db unavailable")
	gen := &fakeGen{reply: cleanVerdict}
	w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil), WorkerConfig{}, nil)

	if got := w.ProcessOnce(context.Background()); got != 0 {
		t.Errorf("resolved %d facts despite a claim failure", got)
	}

	store.mu.Lock()
	store.failClaim = nil
	store.mu.Unlock()

	if got := w.ProcessOnce(context.Background()); got != 1 {
		t.Errorf("resolved %d facts after recovery, want 1", got)
	}
}

// TestStartStopDrains pins clean shutdown: Stop waits for the in-flight tick so a
// screen in progress is not torn down mid-flight.
func TestStartStopDrains(t *testing.T) {
	store := newFakeStore(pendingFacts(4, benignFact)...)
	gen := &concurrencyProbe{reply: cleanVerdict, hold: time.Millisecond}
	w := NewWorker(store, NewScreener(DefaultPolicy(), gen, nil),
		WorkerConfig{Interval: 5 * time.Millisecond, Concurrency: 2, Batch: 4}, nil)

	w.Start()
	deadline := time.After(2 * time.Second)
	for {
		if w.Stats().Screened.Load() >= 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("worker screened only %d of 4 facts before the deadline", w.Stats().Screened.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	w.Stop()
}

// routingGen replies based on the content embedded in the prompt, so one generator can
// serve a batch of different facts.
type routingGen struct {
	mu      sync.Mutex
	replies map[string]string
}

func (g *routingGen) Generate(_ context.Context, prompt string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for content, reply := range g.replies {
		if containsAll(prompt, content) {
			return reply, nil
		}
	}
	return cleanVerdict, nil
}
func (g *routingGen) Model() string { return "routing" }

// containsAll reports whether the prompt carries the content. The prompt neutralizes
// its content, so compare on a distinctive prefix rather than the whole string.
func containsAll(prompt, content string) bool {
	head := content
	if len(head) > 40 {
		head = head[:40]
	}
	return len(head) > 0 && stringsContains(prompt, head)
}

func stringsContains(h, n string) bool {
	return len(n) <= len(h) && indexOf(h, n) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// concurrencyProbe records the peak number of simultaneous Generate calls.
type concurrencyProbe struct {
	reply       string
	hold        time.Duration
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	calls       atomic.Int64
}

func (g *concurrencyProbe) Generate(_ context.Context, _ string) (string, error) {
	n := g.inFlight.Add(1)
	for {
		peak := g.maxInFlight.Load()
		if n <= peak || g.maxInFlight.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(g.hold)
	g.calls.Add(1)
	g.inFlight.Add(-1)
	return g.reply, nil
}
func (g *concurrencyProbe) Model() string { return "probe" }
