package httpapi

// The document half of the embed queue.
//
// Facts and document chunks drain through the same ticker deliberately: they
// share an embedder, a ceiling, and a failure policy, and running two queues
// would mean two places to get the retryable-versus-permanent distinction
// right.

import (
	"context"

	embedding "github.com/matthewjhunter/go-embedding"

	"github.com/matthewjhunter/memstore"
)

// processChunks embeds a batch of document chunks. A backend with no document
// corpus has nothing to do here and says so by not implementing the interface.
func (eq *EmbedQueue) processChunks(ctx context.Context) {
	es, ok := eq.store.(memstore.DocumentEmbedStore)
	if !ok {
		return
	}
	pending, err := es.ChunksNeedingEmbedding(ctx, eq.batch)
	if err != nil {
		eq.log().Error("embed queue: listing chunks needing embedding failed", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	// One chunk per request, for the reason the fact loop gives: a batched
	// call lets one poisoned input fail the whole batch, and the queue hands
	// back the same head-of-queue rows every tick, so the stall is permanent.
	embedded := 0
	for _, p := range pending {
		body := p.Chunk.Content
		if eq.ceiling > 0 {
			// Head truncation, and it is a stopgap. A document chunk may be
			// up to chunk.Max non-whitespace characters, which can exceed the
			// embedder's input budget; the fact side handles that by
			// splitting into several vectors, which needs a table documents
			// do not have. The Chroma measurements say our chunks are too
			// large for retrieval anyway, so the case this papers over should
			// shrink rather than be engineered around.
			body = memstore.Truncate(body, eq.ceiling)
		}
		text := memstore.ChunkEmbedText(eq.embedder.Model(), p.DocPath, p.Chunk, body)

		vecs, err := embedding.EmbedWithRetry(ctx, eq.embedder, []string{text})
		if err != nil || len(vecs) != 1 {
			if err != nil && !embedding.IsRetryable(err) {
				eq.log().Error("embed queue: quarantining chunk after a permanent embed failure", "chunk", p.Chunk.ID, "err", err)
				if mErr := es.MarkChunkEmbedFailed(ctx, p.Chunk.ID, err.Error()); mErr != nil {
					eq.log().Error("embed queue: marking the chunk failed", "chunk", p.Chunk.ID, "err", mErr)
				}
				continue
			}
			eq.log().Error("embed queue: embedding the chunk failed", "chunk", p.Chunk.ID, "err", err)
			continue
		}
		if err := es.SetChunkVector(ctx, p.Chunk.ID, vecs[0]); err != nil {
			eq.log().Error("embed queue: storing the chunk vector failed", "chunk", p.Chunk.ID, "err", err)
			continue
		}
		embedded++
	}
	if embedded > 0 {
		eq.log().Info("embed queue: embedded document chunks", "embedded", embedded, "pending", len(pending))
	}
}
