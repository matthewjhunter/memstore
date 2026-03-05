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
			eq.processOnce()
		}
	}
}

func (eq *EmbedQueue) processOnce() {
	ctx := context.Background()
	facts, err := eq.store.NeedingEmbedding(ctx, eq.batch)
	if err != nil {
		log.Printf("embed queue: NeedingEmbedding: %v", err)
		return
	}
	if len(facts) == 0 {
		return
	}

	texts := make([]string, len(facts))
	for i, f := range facts {
		texts[i] = f.Subject + ": " + f.Content
	}

	embeddings, err := eq.embedder.Embed(ctx, texts)
	if err != nil {
		log.Printf("embed queue: Embed: %v", err)
		return
	}

	for i, f := range facts {
		if err := eq.store.SetEmbedding(ctx, f.ID, embeddings[i]); err != nil {
			log.Printf("embed queue: SetEmbedding id=%d: %v", f.ID, err)
		}
	}
	log.Printf("embed queue: embedded %d facts", len(facts))
}
