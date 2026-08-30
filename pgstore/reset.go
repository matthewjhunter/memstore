package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ResetEmbeddings drops every stored vector and forgets the recorded embedder
// fingerprint, so the daemon can be started under a different embedding model.
// It returns how many fact vectors were cleared.
//
// This is the operator-side half of reconcileEmbedder: a model or dimension
// change is refused at open because vectors from two models do not compare,
// and silently discarding them would hide a configuration drift. Running this
// deliberately, then starting the daemon with the new model, records the new
// fingerprint and lets the embed queue rebuild every vector.
//
// The reset is deployment-wide, not namespace-scoped: the fingerprint is one
// row per database and the embedder is one per daemon.
func ResetEmbeddings(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pgstore: reset embeddings: %w", err)
	}
	defer tx.Rollback(ctx)

	// Both corpora, and both counted. Document chunks were given vectors
	// after this was written, so it cleared the fact side and left document
	// vectors behind -- in the previous model's space, which is exactly the
	// silently degraded ranking the fingerprint check exists to prevent.
	var cleared int64
	if err := tx.QueryRow(ctx,
		`SELECT (SELECT count(embedding) FROM memstore_facts)
		      + (SELECT count(embedding) FROM memstore_document_chunks)`).Scan(&cleared); err != nil {
		return 0, fmt.Errorf("pgstore: counting vectors: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM memstore_fact_chunks`,
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
		`UPDATE memstore_document_chunks SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
		`DELETE FROM memstore_meta WHERE key IN ('embedding_model', 'embedding_dim', 'embedding_recipe')`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return 0, fmt.Errorf("pgstore: reset embeddings: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pgstore: reset embeddings: %w", err)
	}
	return cleared, nil
}
