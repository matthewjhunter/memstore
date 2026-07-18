package screening

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// PendingFact is a write awaiting screening.
//
// It carries only what the screener needs plus the attempt count, which is the
// worker's evidence that a fact is not merely slow but stuck.
type PendingFact struct {
	ID       int64
	Content  string
	Attempts int
}

// PendingStore is the storage the worker drives. Kept to four methods so the state
// machine is legible: a pending fact either resolves, gets another try, or is given
// up on.
type PendingStore interface {
	// PendingFacts returns facts awaiting screening, oldest first. It must not
	// return facts that have been abandoned, or the worker will spin on them.
	PendingFacts(ctx context.Context, limit int) ([]PendingFact, error)

	// Resolve applies a terminal decision. An allowed fact becomes readable; a
	// blocked one is removed from the readable corpus and recorded. Either way the
	// fact stops being pending.
	Resolve(ctx context.Context, id int64, d Decision) error

	// Defer records a failed attempt and leaves the fact pending for a later tick.
	// The reason is a fixed-vocabulary string, never model or content text.
	Defer(ctx context.Context, id int64, reason string) error

	// Abandon stops the worker retrying a fact. The fact stays pending, and pending
	// means unreadable, so giving up is safe: the content is retained for review but
	// never served. It must no longer be returned by PendingFacts.
	Abandon(ctx context.Context, id int64, reason string) error
}

// Worker screens pending writes in the background.
//
// Screening is deliberately outside the synchronous write path. A write returns as
// soon as it is durable, marked pending and therefore invisible to every read, and
// this worker decides its fate afterwards. That is what buys the generous per-call
// timeout: a screen taking 7 seconds against a loaded local model is fine when
// nothing is waiting on it, and fatal when a user is.
//
// The safety property holds throughout. At no point between the write returning and
// the screen completing is the content retrievable, so a slow, backlogged, or entirely
// stopped worker delays memories from appearing but never serves an unscreened one.
// Failure lands on the side of losing availability, not containment.
type Worker struct {
	store    PendingStore
	screener *Screener
	log      *slog.Logger

	interval    time.Duration
	concurrency int
	batch       int
	maxAttempts int

	done chan struct{}
	wg   sync.WaitGroup

	stats Stats
}

// Stats are cumulative worker counters, safe to read while it runs.
type Stats struct {
	Screened  atomic.Int64
	Allowed   atomic.Int64
	Blocked   atomic.Int64
	Deferred  atomic.Int64
	Abandoned atomic.Int64
}

// WorkerConfig tunes the worker. Zero values take documented defaults.
type WorkerConfig struct {
	// Interval is how often to poll for pending facts.
	Interval time.Duration

	// Concurrency is how many facts are screened at once.
	//
	// Each screen is one model call, so this is the knob that decides whether the
	// backlog drains or grows, and also the one that can flatten a local model
	// serving other work. At roughly 7s per screen, concurrency 1 clears about 500
	// facts an hour -- fine for steady state, slow for a first pass over a corpus of
	// thousands. Raise it to backfill, lower it to stay polite.
	Concurrency int

	// Batch is how many pending facts to claim per tick. It is clamped to at least
	// Concurrency, since a batch smaller than the worker pool leaves workers idle.
	Batch int

	// MaxAttempts is how many failed screens a fact gets before the worker abandons
	// it.
	//
	// Without a ceiling one unscreenable fact stalls the queue forever: PendingFacts
	// returns oldest-first, so the same row comes back every tick and the backlog
	// behind it never moves. Abandoning is safe precisely because pending is
	// unreadable -- the content is kept for review, just never served and never
	// retried.
	MaxAttempts int
}

const (
	defaultInterval    = 30 * time.Second
	defaultConcurrency = 1
	defaultBatch       = 16
	defaultMaxAttempts = 5
)

// NewWorker builds a screening worker. A nil logger discards its output.
func NewWorker(store PendingStore, sc *Screener, cfg WorkerConfig, log *slog.Logger) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Batch <= 0 {
		cfg.Batch = defaultBatch
	}
	if cfg.Batch < cfg.Concurrency {
		cfg.Batch = cfg.Concurrency
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Worker{
		store:       store,
		screener:    sc,
		log:         log,
		interval:    cfg.Interval,
		concurrency: cfg.Concurrency,
		batch:       cfg.Batch,
		maxAttempts: cfg.MaxAttempts,
		done:        make(chan struct{}),
	}
}

// Stats returns the worker's counters.
func (w *Worker) Stats() *Stats { return &w.stats }

// Start begins the background loop.
func (w *Worker) Start() {
	w.wg.Add(1)
	go w.loop()
}

// Stop signals the loop to stop and waits for the current tick to finish.
func (w *Worker) Stop() {
	close(w.done)
	w.wg.Wait()
}

func (w *Worker) loop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			// Ticks do not overlap: ProcessOnce runs to completion before the next
			// one starts. A tick slower than the interval would otherwise stack
			// batches on top of each other and multiply the real concurrency past
			// the configured ceiling.
			w.ProcessOnce(context.Background())
		}
	}
}

// ProcessOnce claims one batch of pending facts and screens them. Exposed for tests
// and for a one-shot backfill.
//
// It returns the number of facts it took a terminal decision on.
func (w *Worker) ProcessOnce(ctx context.Context) int {
	facts, err := w.store.PendingFacts(ctx, w.batch)
	if err != nil {
		w.log.Error("screening worker: claiming pending facts", "err", err)
		return 0
	}
	if len(facts) == 0 {
		return 0
	}

	jobs := make(chan PendingFact)
	var resolved atomic.Int64
	var wg sync.WaitGroup

	for range w.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if w.handle(ctx, f) {
					resolved.Add(1)
				}
			}
		}()
	}

	for _, f := range facts {
		select {
		case <-ctx.Done():
		case <-w.done:
			// Stop feeding on shutdown. Facts already handed out finish; the rest
			// stay pending, which is the safe state to be interrupted in.
			close(jobs)
			wg.Wait()
			return int(resolved.Load())
		case jobs <- f:
			continue
		}
		break
	}
	close(jobs)
	wg.Wait()

	return int(resolved.Load())
}

// handle screens one fact and applies the result. It reports whether the fact reached
// a terminal state.
func (w *Worker) handle(ctx context.Context, f PendingFact) bool {
	if f.Attempts >= w.maxAttempts {
		// Out of retries. Abandoning keeps the content for review and keeps it
		// unreadable; the alternative is this row heading the queue forever.
		w.log.Warn("screening worker: abandoning fact after repeated failures",
			"id", f.ID, "attempts", f.Attempts)
		if err := w.store.Abandon(ctx, f.ID, "max-attempts"); err != nil {
			w.log.Error("screening worker: abandon", "id", f.ID, "err", err)
			return false
		}
		w.stats.Abandoned.Add(1)
		return true
	}

	d := w.screener.Screen(ctx, f.Content)

	// Still pending means the screen could not be completed -- no generator, a model
	// failure, or a fabricated citation. Nothing was learned, so this is a retry
	// rather than a decision.
	if d.Outcome == OutcomePending {
		if err := w.store.Defer(ctx, f.ID, "screen-unavailable"); err != nil {
			w.log.Error("screening worker: defer", "id", f.ID, "err", err)
			return false
		}
		w.stats.Deferred.Add(1)
		return false
	}

	if err := w.store.Resolve(ctx, f.ID, d); err != nil {
		w.log.Error("screening worker: resolve", "id", f.ID, "outcome", string(d.Outcome), "err", err)
		return false
	}

	w.stats.Screened.Add(1)
	if d.Blocked() {
		w.stats.Blocked.Add(1)
		// Logged at warn with no content: the block is operationally interesting and
		// the payload is exactly what must not reach a log.
		w.log.Warn("screening worker: blocked a stored write",
			"id", f.ID, "threat", d.Threat, "category", d.Category, "reason", d.Reason)
	} else {
		w.stats.Allowed.Add(1)
	}
	return true
}
