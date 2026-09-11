package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/matthewjhunter/go-embedding"
)

// The store's vector dimension, recorded in memstore_meta and checked on every
// write of a vector.
//
// It used to be recorded only by the EmbedFacts backfill, through
// recordEmbedder, so a store embedded by the embed queue or the extractor never
// had one on record and the dimension half of the fingerprint check never fired
// (#236). The check needs only a vector's length, not the embedder, so every
// write path makes it, and a store with no embedder -- the admin CLI, a test --
// is held to it as well.

// dimState caches the dimension this process has confirmed against
// memstore_meta, so only the first write of a dimension reads the table. It is
// shared with scoped copies, like annState.
type dimState struct {
	mu    sync.Mutex
	known int
}

// checkVectorDims confirms that vectors of these lengths belong in the store.
// It records the dimension when none is recorded yet, and refuses one that
// differs with an error wrapping embedding.ErrDimChanged. Call it before
// writing, so a refused write stores nothing.
func (s *PostgresStore) checkVectorDims(ctx context.Context, vecs ...[]float32) error {
	for _, v := range vecs {
		if err := s.checkVectorDim(ctx, len(v)); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) checkVectorDim(ctx context.Context, dim int) error {
	if dim == 0 {
		return nil
	}
	s.dims.mu.Lock()
	defer s.dims.mu.Unlock()
	if s.dims.known == dim {
		return nil
	}
	// Insert if absent, then read back, so two processes writing their first
	// vectors at once settle on one dimension instead of each recording its own.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO memstore_meta (key, value) VALUES ('embedding_dim', $1) ON CONFLICT (key) DO NOTHING`,
		strconv.Itoa(dim)); err != nil {
		return fmt.Errorf("pgstore: recording embedding_dim: %w", err)
	}
	var stored string
	if err := s.pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_dim'`).Scan(&stored); err != nil {
		return fmt.Errorf("pgstore: reading embedding_dim: %w", err)
	}
	if stored != strconv.Itoa(dim) {
		return fmt.Errorf("pgstore: refusing a %d-dimensional vector in a store of %s-dimensional ones: %w "+
			"(to switch models deliberately, run 'memstore admin reset-embeddings' and start again)",
			dim, stored, embedding.ErrDimChanged)
	}
	s.dims.known = dim
	return nil
}

// forgetVectorDim drops the cached dimension, for clearVectors: the vectors
// that replace the cleared ones may have another.
func (s *PostgresStore) forgetVectorDim() {
	s.dims.mu.Lock()
	defer s.dims.mu.Unlock()
	s.dims.known = 0
}

// recordStoredDim records the dimension of the vectors already stored when
// none is on record, which is how a store embedded before #236 looks. It checks
// a recorded one against them too. A store with no vectors is left alone.
func (s *PostgresStore) recordStoredDim(ctx context.Context) error {
	var dim int
	err := s.pool.QueryRow(ctx, `
		SELECT d FROM (
		  (SELECT vector_dims(embedding) AS d FROM memstore_fact_chunks WHERE embedding IS NOT NULL LIMIT 1)
		  UNION ALL
		  (SELECT vector_dims(embedding) FROM memstore_facts WHERE embedding IS NOT NULL LIMIT 1)
		  UNION ALL
		  (SELECT vector_dims(embedding) FROM memstore_document_chunks WHERE embedding IS NOT NULL LIMIT 1)
		) v LIMIT 1`).Scan(&dim)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pgstore: reading the stored vector dimension: %w", err)
	}
	return s.checkVectorDim(ctx, dim)
}
