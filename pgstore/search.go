package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/matthewjhunter/memstore"
	pgvector "github.com/pgvector/pgvector-go"
)

// Search performs hybrid tsvector + pgvector search, merging and deduplicating results.
func (s *PostgresStore) Search(ctx context.Context, query string, opts memstore.SearchOpts) ([]memstore.SearchResult, error) {
	if s.embedder == nil {
		return nil, fmt.Errorf("pgstore: Search requires an embedder")
	}

	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	if opts.FTSWeight == 0 && opts.VecWeight == 0 {
		opts.FTSWeight = 0.6
		opts.VecWeight = 0.4
	}
	if s.reranker != nil && opts.RerankMode.Enabled() && opts.RerankCandidates <= 0 {
		// Size the candidate pool before the SQL runs (FetchLimit reads it).
		opts.RerankCandidates = memstore.DefaultRerankCandidates
	}

	queryEmb, err := s.queryCache.Single(ctx, s.embedder, query)
	if err != nil {
		return nil, err
	}

	ftsResults, err := s.searchFTS(ctx, query, opts)
	if err != nil {
		return nil, err
	}

	var vecResults []memstore.SearchResult
	if len(queryEmb) > 0 {
		vecResults, err = s.searchVector(ctx, queryEmb, opts)
		if err != nil {
			return nil, err
		}
	}

	return memstore.ScoreResults(ctx, s.reranker, query, ftsResults, vecResults, opts)
}

// SearchFTS performs tsvector-only search without requiring an embedder.
func (s *PostgresStore) SearchFTS(ctx context.Context, query string, opts memstore.SearchOpts) ([]memstore.SearchResult, error) {
	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	if opts.FTSWeight == 0 && opts.VecWeight == 0 {
		opts.FTSWeight = 1.0
		opts.VecWeight = 0.0
	}

	ftsResults, err := s.searchFTS(ctx, query, opts)
	if err != nil {
		return nil, err
	}

	// FTS-only path does not rerank: no embedder context, administrative fallback.
	return memstore.ScoreResults(ctx, nil, query, ftsResults, nil, opts)
}

// SearchBatch performs hybrid search for multiple queries with shared embedding.
func (s *PostgresStore) SearchBatch(ctx context.Context, queries []string, opts memstore.SearchOpts) ([][]memstore.SearchResult, error) {
	if len(queries) == 0 {
		return nil, nil
	}
	if s.embedder == nil {
		return nil, fmt.Errorf("pgstore: SearchBatch requires an embedder")
	}

	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	if opts.FTSWeight == 0 && opts.VecWeight == 0 {
		opts.FTSWeight = 0.6
		opts.VecWeight = 0.4
	}

	queryEmbs, err := s.queryCache.Embed(ctx, s.embedder, queries)
	if err != nil {
		return nil, err
	}

	results := make([][]memstore.SearchResult, len(queries))
	for i, query := range queries {
		ftsResults, err := s.searchFTS(ctx, query, opts)
		if err != nil {
			return nil, err
		}

		var vecResults []memstore.SearchResult
		if i < len(queryEmbs) && len(queryEmbs[i]) > 0 {
			vecResults, err = s.searchVector(ctx, queryEmbs[i], opts)
			if err != nil {
				return nil, err
			}
		}

		// Batch search does not rerank: bulk/backfill path, latency-sensitive.
		scored, err := memstore.ScoreResults(ctx, nil, query, ftsResults, vecResults, opts)
		if err != nil {
			return nil, err
		}
		results[i] = scored
	}

	return results, nil
}

// searchFTS performs a ts_rank-ranked full-text search using the stored tsvector column.
func (s *PostgresStore) searchFTS(ctx context.Context, query string, opts memstore.SearchOpts) ([]memstore.SearchResult, error) {
	tsquery := quoteFTSQuery(query)
	if tsquery == "" {
		return nil, nil
	}

	var b queryBuilder
	b.write(`SELECT f.id, f.namespace, f.user_id, f.content, f.subject, f.category, f.kind, f.subsystem, f.metadata,
	                f.superseded_by, f.superseded_at, f.confirmed_count, f.last_confirmed_at,
	                f.use_count, f.last_used_at, f.embedding, f.created_at,
	                ts_rank(f.fts, plainto_tsquery('english', `, tsquery)
	b.q += `)) AS rank
	         FROM memstore_facts f
	         WHERE f.fts @@ plainto_tsquery('english', `
	b.write(``, tsquery)
	b.q += `)`

	s.appendNamespaceFilter(&b, "f.namespace", opts.AllNamespaces, opts.Namespaces)
	s.appendUserFilter(&b, "f.user_id")
	b.q += memstore.ScreenReadableSQL("f.")
	if opts.OnlyActive {
		b.q += ` AND f.superseded_by IS NULL`
	}
	if opts.Subject != "" {
		b.write(` AND f.subject = `, opts.Subject)
	}
	if opts.Category != "" {
		b.write(` AND f.category = `, opts.Category)
	}
	if opts.Kind != "" {
		b.write(` AND f.kind = `, opts.Kind)
	}
	if opts.Subsystem != "" {
		b.write(` AND f.subsystem = `, opts.Subsystem)
	}
	if err := appendMetadataFilters(&b, "f.", opts.MetadataFilters); err != nil {
		return nil, err
	}
	appendTemporalFilters(&b, "f.", opts.CreatedAfter, opts.CreatedBefore)

	b.write(` ORDER BY rank DESC LIMIT `, memstore.FetchLimit(opts))

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: FTS search: %w", err)
	}
	defer rows.Close()

	var results []memstore.SearchResult
	for rows.Next() {
		var f memstore.Fact
		var metadata []byte
		var supersededBy *int64
		var supersededAt *time.Time
		var lastConfirmedAt *time.Time
		var lastUsedAt *time.Time
		var emb *pgvector.Vector
		var rank float64

		err := rows.Scan(
			&f.ID, &f.Namespace, &f.UserID, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
			&metadata, &supersededBy, &supersededAt,
			&f.ConfirmedCount, &lastConfirmedAt,
			&f.UseCount, &lastUsedAt,
			&emb, &f.CreatedAt, &rank,
		)
		if err != nil {
			return nil, fmt.Errorf("pgstore: scanning FTS result: %w", err)
		}

		if len(metadata) > 0 {
			f.Metadata = json.RawMessage(metadata)
		}
		f.SupersededBy = supersededBy
		f.SupersededAt = supersededAt
		f.LastConfirmedAt = lastConfirmedAt
		f.LastUsedAt = lastUsedAt
		if emb != nil {
			f.Embedding = emb.Slice()
		}

		// ts_rank returns positive scores (higher = better match).
		results = append(results, memstore.SearchResult{
			Fact:     f,
			FTSScore: rank,
		})
	}

	return results, rows.Err()
}

// searchVector performs cosine similarity search using pgvector's <=> operator.
func (s *PostgresStore) searchVector(ctx context.Context, queryEmb []float32, opts memstore.SearchOpts) ([]memstore.SearchResult, error) {
	qv := pgvector.NewVector(queryEmb)

	var b queryBuilder
	b.write(`SELECT `+factColumns+`, 1 - (embedding <=> `, qv)
	b.q += `) AS similarity
	         FROM memstore_facts
	         WHERE embedding IS NOT NULL`

	s.appendNamespaceFilter(&b, "namespace", opts.AllNamespaces, opts.Namespaces)
	s.appendUserFilter(&b, "user_id")
	b.q += memstore.ScreenReadableSQL("")
	if opts.OnlyActive {
		b.q += ` AND superseded_by IS NULL`
	}
	if opts.Subject != "" {
		b.write(` AND subject = `, opts.Subject)
	}
	if opts.Category != "" {
		b.write(` AND category = `, opts.Category)
	}
	if opts.Kind != "" {
		b.write(` AND kind = `, opts.Kind)
	}
	if opts.Subsystem != "" {
		b.write(` AND subsystem = `, opts.Subsystem)
	}
	if err := appendMetadataFilters(&b, "", opts.MetadataFilters); err != nil {
		return nil, err
	}
	appendTemporalFilters(&b, "", opts.CreatedAfter, opts.CreatedBefore)

	b.write(` ORDER BY embedding <=> `, qv)
	b.write(` LIMIT `, memstore.FetchLimit(opts))

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: vector search: %w", err)
	}
	defer rows.Close()

	var results []memstore.SearchResult
	for rows.Next() {
		var f memstore.Fact
		var metadata []byte
		var supersededBy *int64
		var supersededAt *time.Time
		var lastConfirmedAt *time.Time
		var lastUsedAt *time.Time
		var emb *pgvector.Vector
		var similarity float64

		err := rows.Scan(
			&f.ID, &f.Namespace, &f.UserID, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
			&metadata, &supersededBy, &supersededAt,
			&f.ConfirmedCount, &lastConfirmedAt,
			&f.UseCount, &lastUsedAt,
			&emb, &f.CreatedAt, &similarity,
		)
		if err != nil {
			return nil, fmt.Errorf("pgstore: scanning vector result: %w", err)
		}

		if len(metadata) > 0 {
			f.Metadata = json.RawMessage(metadata)
		}
		f.SupersededBy = supersededBy
		f.SupersededAt = supersededAt
		f.LastConfirmedAt = lastConfirmedAt
		f.LastUsedAt = lastUsedAt
		if emb != nil {
			f.Embedding = emb.Slice()
		}

		if similarity > 0 {
			results = append(results, memstore.SearchResult{
				Fact:     f,
				VecScore: similarity,
			})
		}
	}

	return results, rows.Err()
}
