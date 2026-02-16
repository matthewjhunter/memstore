package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Search performs hybrid FTS5 + vector search, merging and deduplicating results.
// If no embedder is configured, degrades to FTS5-only.
func (s *SQLiteStore) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	if opts.FTSWeight == 0 && opts.VecWeight == 0 {
		opts.FTSWeight = 0.6
		opts.VecWeight = 0.4
	}

	// Embed the query if an embedder is available.
	var queryEmb []float32
	if s.embedder != nil {
		if emb, err := Single(ctx, s.embedder, query); err == nil {
			queryEmb = emb
		}
		// On embedding error, fall through to FTS-only.
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ftsResults, err := s.searchFTS(ctx, query, opts)
	if err != nil {
		return nil, err
	}

	var vecResults []SearchResult
	if len(queryEmb) > 0 {
		vecResults, err = s.searchVector(ctx, queryEmb, opts)
		if err != nil {
			return nil, err
		}
	}

	return mergeResults(ftsResults, vecResults, opts), nil
}

// searchFTS performs a BM25-ranked FTS5 search.
func (s *SQLiteStore) searchFTS(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	q := `SELECT f.id, f.namespace, f.content, f.subject, f.category, f.metadata,
	             f.superseded_by, f.superseded_at, f.embedding, f.created_at, rank
	      FROM memstore_facts_fts fts
	      JOIN memstore_facts f ON f.id = fts.rowid
	      WHERE memstore_facts_fts MATCH ?`

	args := []any{query}

	if !opts.AllNamespaces {
		q += ` AND f.namespace = ?`
		args = append(args, s.namespace)
	}
	if opts.OnlyActive {
		q += ` AND f.superseded_by IS NULL`
	}
	if opts.Category != "" {
		q += ` AND f.category = ?`
		args = append(args, opts.Category)
	}
	if err := appendMetadataFilters(&q, &args, "f.", opts.MetadataFilters); err != nil {
		return nil, err
	}

	q += ` ORDER BY rank LIMIT ?`
	args = append(args, opts.MaxResults*2) // fetch extra for merge

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memstore: FTS search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var f Fact
		var metadata sql.NullString
		var supersededBy *int64
		var supersededAt sql.NullString
		var embBlob []byte
		var createdAt string
		var rank float64

		err := rows.Scan(
			&f.ID, &f.Namespace, &f.Content, &f.Subject, &f.Category,
			&metadata, &supersededBy, &supersededAt, &embBlob, &createdAt, &rank,
		)
		if err != nil {
			return nil, fmt.Errorf("memstore: scanning FTS result: %w", err)
		}

		if metadata.Valid && metadata.String != "" {
			f.Metadata = json.RawMessage(metadata.String)
		}
		f.SupersededBy = supersededBy
		if supersededAt.Valid {
			t, _ := time.Parse(time.RFC3339, supersededAt.String)
			f.SupersededAt = &t
		}
		if len(embBlob) > 0 {
			f.Embedding = DecodeFloat32s(embBlob)
		}
		f.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)

		// BM25 rank is negative (lower = better match), negate for scoring.
		results = append(results, SearchResult{
			Fact:     f,
			FTSScore: -rank,
		})
	}

	return results, rows.Err()
}

// searchVector performs cosine similarity search against stored embeddings.
func (s *SQLiteStore) searchVector(ctx context.Context, queryEmb []float32, opts SearchOpts) ([]SearchResult, error) {
	q := `SELECT ` + factColumns + `
	      FROM memstore_facts WHERE embedding IS NOT NULL`

	var args []any

	if !opts.AllNamespaces {
		q += ` AND namespace = ?`
		args = append(args, s.namespace)
	}
	if opts.OnlyActive {
		q += ` AND superseded_by IS NULL`
	}
	if opts.Category != "" {
		q += ` AND category = ?`
		args = append(args, opts.Category)
	}
	if err := appendMetadataFilters(&q, &args, "", opts.MetadataFilters); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memstore: vector search: %w", err)
	}
	defer rows.Close()

	type scored struct {
		fact  Fact
		score float64
	}
	var candidates []scored

	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, fmt.Errorf("memstore: scanning vector result: %w", err)
		}
		if len(f.Embedding) == 0 {
			continue
		}
		sim := CosineSimilarity(queryEmb, f.Embedding)
		if sim > 0 {
			candidates = append(candidates, scored{fact: *f, score: sim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memstore: vector search scan: %w", err)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	limit := opts.MaxResults * 2
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	results := make([]SearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = SearchResult{
			Fact:     c.fact,
			VecScore: c.score,
		}
	}
	return results, nil
}

// mergeResults combines FTS and vector results, deduplicates by fact ID,
// computes weighted combined scores, and returns the top N.
func mergeResults(fts, vec []SearchResult, opts SearchOpts) []SearchResult {
	byID := make(map[int64]*SearchResult)

	// Normalize FTS scores.
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
		sr := SearchResult{
			Fact:     r.Fact,
			FTSScore: norm,
		}
		byID[r.Fact.ID] = &sr
	}

	// Vector scores are already 0-1 from cosine similarity.
	for _, r := range vec {
		if existing, ok := byID[r.Fact.ID]; ok {
			existing.VecScore = r.VecScore
		} else {
			sr := SearchResult{
				Fact:     r.Fact,
				VecScore: r.VecScore,
			}
			byID[r.Fact.ID] = &sr
		}
	}

	// Compute combined score with configurable weights.
	merged := make([]SearchResult, 0, len(byID))
	for _, r := range byID {
		r.Combined = opts.FTSWeight*r.FTSScore + opts.VecWeight*r.VecScore
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
