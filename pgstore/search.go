package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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

	queryEmb, err := memstore.Single(ctx, s.embedder, query)
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

	return mergeResults(ftsResults, vecResults, opts), nil
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

	return mergeResults(ftsResults, nil, opts), nil
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

	queryEmbs, err := memstore.EmbedWithRetry(ctx, s.embedder, queries)
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

		results[i] = mergeResults(ftsResults, vecResults, opts)
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
	b.write(`SELECT f.id, f.namespace, f.content, f.subject, f.category, f.kind, f.subsystem, f.metadata,
	                f.superseded_by, f.superseded_at, f.confirmed_count, f.last_confirmed_at,
	                f.use_count, f.last_used_at, f.embedding, f.created_at,
	                ts_rank(f.fts, plainto_tsquery('english', `, tsquery)
	b.q += `)) AS rank
	         FROM memstore_facts f
	         WHERE f.fts @@ plainto_tsquery('english', `
	b.write(``, tsquery)
	b.q += `)`

	s.appendNamespaceFilter(&b, "f.namespace", opts.AllNamespaces, opts.Namespaces)
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
	appendMetadataFilters(&b, "f.", opts.MetadataFilters)
	appendTemporalFilters(&b, "f.", opts.CreatedAfter, opts.CreatedBefore)

	b.write(` ORDER BY rank DESC LIMIT `, opts.MaxResults*2)

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
			&f.ID, &f.Namespace, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
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
	appendMetadataFilters(&b, "", opts.MetadataFilters)
	appendTemporalFilters(&b, "", opts.CreatedAfter, opts.CreatedBefore)

	b.write(` ORDER BY embedding <=> `, qv)
	b.write(` LIMIT `, opts.MaxResults*2)

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
			&f.ID, &f.Namespace, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
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

// mergeResults combines FTS and vector results, deduplicates by fact ID,
// computes weighted combined scores, and returns the top N.
func mergeResults(fts, vec []memstore.SearchResult, opts memstore.SearchOpts) []memstore.SearchResult {
	byID := make(map[int64]*memstore.SearchResult)

	var maxFTS float64
	for _, r := range fts {
		if r.FTSScore > maxFTS {
			maxFTS = r.FTSScore
		}
	}
	for _, r := range fts {
		norm := r.FTSScore
		if maxFTS > 0 {
			norm = r.FTSScore / maxFTS
		}
		sr := memstore.SearchResult{
			Fact:     r.Fact,
			FTSScore: norm,
		}
		byID[r.Fact.ID] = &sr
	}

	for _, r := range vec {
		if existing, ok := byID[r.Fact.ID]; ok {
			existing.VecScore = r.VecScore
		} else {
			sr := memstore.SearchResult{
				Fact:     r.Fact,
				VecScore: r.VecScore,
			}
			byID[r.Fact.ID] = &sr
		}
	}

	now := time.Now()
	merged := make([]memstore.SearchResult, 0, len(byID))
	for _, r := range byID {
		r.Combined = opts.FTSWeight*r.FTSScore + opts.VecWeight*r.VecScore
		if hl := decayHalfLife(r.Fact.Category, opts); hl > 0 {
			age := now.Sub(r.Fact.CreatedAt).Seconds()
			r.Combined *= math.Pow(0.5, age/hl.Seconds())
		}
		merged = append(merged, *r)
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Combined > merged[j].Combined
	})

	if len(merged) > opts.MaxResults {
		merged = merged[:opts.MaxResults]
	}
	return merged
}

func decayHalfLife(category string, opts memstore.SearchOpts) time.Duration {
	if opts.CategoryDecay != nil {
		if hl, ok := opts.CategoryDecay[category]; ok {
			return hl
		}
	}
	return opts.DecayHalfLife
}
