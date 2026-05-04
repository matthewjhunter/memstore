package httpapi

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/matthewjhunter/memstore"
)

// EmbedQueue processes facts that need embeddings in the background.
type EmbedQueue struct {
	store    memstore.Store
	embedder memstore.Embedder
	interval time.Duration
	batch    int

	done chan struct{}
	wg   sync.WaitGroup
}

// NewEmbedQueue creates a background embedding processor.
// It polls for unembedded facts every interval and processes them in batches.
func NewEmbedQueue(store memstore.Store, embedder memstore.Embedder, interval time.Duration, batchSize int) *EmbedQueue {
	if interval == 0 {
		interval = 2 * time.Second
	}
	if batchSize == 0 {
		batchSize = 32
	}
	return &EmbedQueue{
		store:    store,
		embedder: embedder,
		interval: interval,
		batch:    batchSize,
		done:     make(chan struct{}),
	}
}

// Start begins the background embedding loop.
func (eq *EmbedQueue) Start() {
	eq.wg.Add(1)
	go eq.loop()
}

// Stop signals the loop to stop and waits for it to finish.
func (eq *EmbedQueue) Stop() {
	close(eq.done)
	eq.wg.Wait()
}

func (eq *EmbedQueue) loop() {
	defer eq.wg.Done()
	ticker := time.NewTicker(eq.interval)
	defer ticker.Stop()

	for {
		select {
		case <-eq.done:
			return
		case <-ticker.C:
			eq.ProcessOnce()
		}
	}
}

// ProcessOnce drains one tick's worth of unembedded facts. Called from the
// background loop and exposed for tests.
func (eq *EmbedQueue) ProcessOnce() {
	ctx := context.Background()
	facts, err := eq.store.NeedingEmbedding(ctx, eq.batch)
	if err != nil {
		log.Printf("embed queue: NeedingEmbedding: %v", err)
		return
	}
	if len(facts) == 0 {
		return
	}

	// Embed one fact at a time. Batched embed calls would let a single
	// poisoned input (e.g. context-length error) fail the whole batch and
	// stall the queue forever, since NeedingEmbedding would keep returning
	// the same head-of-queue rows. Per-fact lets us isolate the bad fact and
	// keep the rest of the queue moving.
	embedded := 0
	for _, f := range facts {
		text := f.Subject + ": " + f.Content
		embs, err := eq.embedder.Embed(ctx, []string{text})
		if err != nil {
			log.Printf("embed queue: Embed id=%d: %v", f.ID, err)
			continue
		}
		if len(embs) != 1 {
			log.Printf("embed queue: Embed id=%d: got %d embeddings, want 1", f.ID, len(embs))
			continue
		}
		if err := eq.store.SetEmbedding(ctx, f.ID, embs[0]); err != nil {
			log.Printf("embed queue: SetEmbedding id=%d: %v", f.ID, err)
			continue
		}
		embedded++
	}
	if embedded > 0 {
		log.Printf("embed queue: embedded %d/%d facts", embedded, len(facts))
	}
}
