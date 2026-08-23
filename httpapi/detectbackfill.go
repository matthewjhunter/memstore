package httpapi

import (
	"context"
	"log"
	"sync"
	"time"
)

// DetectScorer is the store capability DetectBackfill needs. Both backends implement
// it; it is an interface here so the runner can be tested without a database.
type DetectScorer interface {
	// BackfillDetectScores scores up to limit facts that have none, returning how
	// many it scored. Zero means there is nothing left to do.
	BackfillDetectScores(ctx context.Context, limit int) (int, error)
}

// DetectBackfill scores the facts that predate the detect_score column, so the read
// filter has something to act on.
//
// It exists because the score cannot be computed in SQL, so the migration that added
// the column could not populate it. Until this runs, every existing fact reads as
// "not yet computed" -- which is deliberately permissive, since treating unknown as
// hostile would withhold the whole corpus the moment the column landed.
//
// It runs regardless of screen_mode. The read filter is the regex screen, which is
// independent of the model pass, so a deployment with the model screen off still
// needs its corpus scored.
type DetectBackfill struct {
	store    DetectScorer
	interval time.Duration
	batch    int

	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// NewDetectBackfill creates the backfill runner. A zero interval or batch takes a
// sensible default.
func NewDetectBackfill(store DetectScorer, interval time.Duration, batch int) *DetectBackfill {
	if interval == 0 {
		interval = 5 * time.Second
	}
	if batch == 0 {
		batch = 500
	}
	return &DetectBackfill{
		store:    store,
		interval: interval,
		batch:    batch,
		done:     make(chan struct{}),
	}
}

// Start begins draining the backlog in the background.
func (b *DetectBackfill) Start() {
	b.wg.Add(1)
	go b.loop()
}

// Stop signals the loop to finish and waits for it. Safe to call more than once, and
// safe to call after the loop has already exited on its own.
func (b *DetectBackfill) Stop() {
	b.stop.Do(func() { close(b.done) })
	b.wg.Wait()
}

// loop drains the backlog and then exits.
//
// Exiting rather than ticking forever is the point: every fact written from now on is
// scored at insert, so once the pre-existing corpus is done there is nothing left for
// this to find. A permanent ticker would be a query per interval against the live
// database for the lifetime of the process, answering the same "nothing to do"
// forever. Anything that later inserts unscored rows -- a restore from backup, a
// direct SQL load -- is picked up on the next restart.
func (b *DetectBackfill) loop() {
	defer b.wg.Done()

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	total := 0
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			n, err := b.store.BackfillDetectScores(context.Background(), b.batch)
			if err != nil {
				// Retry on the next tick rather than in a tight loop, and do not
				// treat a failure as "drained" -- that would leave the corpus
				// permanently unscored after one transient database error.
				log.Printf("detect backfill: %v", err)
				continue
			}
			if n == 0 {
				if total > 0 {
					log.Printf("detect backfill: complete, %d facts scored", total)
				}
				return
			}
			total += n
		}
	}
}
