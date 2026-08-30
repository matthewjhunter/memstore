package httpapi

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// EmbedQueue processes facts that need embeddings in the background.
type EmbedQueue struct {
	similarity memstore.SimilarityPolicy
	store      memstore.Store
	embedder   embedding.Embedder
	interval   time.Duration
	batch      int
	ceiling    int

	done chan struct{}
	wg   sync.WaitGroup
}

// NewEmbedQueue creates a background embedding processor.
// It polls for unembedded facts every interval and processes them in batches.
// SetSimilarityPolicy sets the gate used when linking newly embedded facts
// to their neighbours. Call before Start; without it the linker uses the
// historical default.
func (eq *EmbedQueue) SetSimilarityPolicy(pol memstore.SimilarityPolicy) {
	eq.similarity = pol
}

func NewEmbedQueue(store memstore.Store, embedder embedding.Embedder, interval time.Duration, batchSize int) *EmbedQueue {
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

// SetCeiling sets the hard byte bound on any single embed request, normally
// the configured embedder's effective budget (embedding.Config.Limits().MaxBytes).
//
// It has to be passed in because the Embedder interface does not expose it, and
// it must be the *configured* budget rather than the model's registered one: a
// deployment lowers it when the backend serving the model is stricter than the
// model itself, and sizing against the registered figure while the request is
// clipped to a lower configured one truncates every chunk's tail silently.
//
// Zero leaves chunk sizing to the retrieval target alone.
func (eq *EmbedQueue) SetCeiling(n int) { eq.ceiling = n }

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
	// keep the rest of the queue moving. A fact's own chunks are still batched
	// together inside EmbedFact -- they succeed or fail as a unit anyway.
	embedded := 0
	var linkable []int64
	for _, f := range facts {
		vecs, err := memstore.EmbedFact(ctx, eq.embedder, eq.embedder.Model(), f, eq.ceiling)
		if err != nil {
			// A transient failure (timeout, 5xx) keeps its NULL embedding and
			// is retried next tick. A permanent failure would otherwise loop
			// forever -- NeedingEmbedding hands the same row back every poll --
			// so quarantine it. Chunking already keeps each request inside the
			// budget, so a permanent failure here means a genuinely
			// unembeddable fact, not merely a long one.
			if !embedding.IsRetryable(err) {
				log.Printf("embed queue: quarantining id=%d (permanent embed failure): %v", f.ID, err)
				if mErr := eq.store.MarkEmbedFailed(ctx, f.ID, err.Error()); mErr != nil {
					log.Printf("embed queue: MarkEmbedFailed id=%d: %v", f.ID, mErr)
				}
				continue
			}
			log.Printf("embed queue: EmbedFact id=%d: %v", f.ID, err)
			continue
		}
		if len(vecs.Chunks) == 0 {
			// Content with nothing embeddable in it (empty or whitespace).
			// Quarantine rather than re-queue it forever.
			log.Printf("embed queue: quarantining id=%d (no embeddable content)", f.ID)
			if mErr := eq.store.MarkEmbedFailed(ctx, f.ID, "no embeddable content"); mErr != nil {
				log.Printf("embed queue: MarkEmbedFailed id=%d: %v", f.ID, mErr)
			}
			continue
		}
		if err := eq.store.SetFactVectors(ctx, f.ID, vecs); err != nil {
			log.Printf("embed queue: SetFactVectors id=%d: %v", f.ID, err)
			continue
		}
		embedded++
		linkable = append(linkable, f.ID)
	}
	if embedded > 0 {
		log.Printf("embed queue: embedded %d/%d facts", embedded, len(facts))
	}
	eq.linkNeighbors(ctx, linkable)
}

// linkNeighbors links what was just embedded. Best-effort: a fact is stored
// and embedded whether or not it acquires links, and failing the batch over
// a link would be a worse outcome than an orphan.
//
// This runs here rather than at insert because a fact has no vector until
// the queue gives it one, so there is nothing to compare at write time.
func (eq *EmbedQueue) linkNeighbors(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	linker, ok := eq.store.(memstore.NeighborLinker)
	if !ok {
		return
	}
	n, err := linker.LinkNeighbors(ctx, ids, eq.similarity, 0)
	if err != nil {
		log.Printf("embed queue: linking neighbours: %v", err)
		return
	}
	if n > 0 {
		log.Printf("embed queue: linked %d new neighbour pairs", n)
	}
}
