package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Search performs hybrid FTS5 + vector search, merging and deduplicating results.
// Requires an embedder; returns an error if none is configured.
func (s *SQLiteStore) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	if s.embedder == nil {
		return nil, fmt.Errorf("memstore: Search requires an embedder")
	}

	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	if opts.FTSWeight == 0 && opts.VecWeight == 0 {
		opts.FTSWeight = 0.6
		opts.VecWeight = 0.4
	}

	queryEmb, err := Single(ctx, s.embedder, query)
	if err != nil {
		return nil, err
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

// quoteFTSQuery makes a raw string safe for use in an FTS5 MATCH expression.
// Each word is individually double-quoted (with internal quotes escaped) so
// FTS5 treats them as literal terms joined by implicit AND, without interpreting
// any special syntax (column prefixes, boolean operators, etc.).
func quoteFTSQuery(raw string) string {
	words := strings.Fields(raw)
	if len(words) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		escaped := strings.ReplaceAll(w, `"`, `""`)
		quoted = append(quoted, `"`+escaped+`"`)
	}
	return strings.Join(quoted, " ")
}

// searchFTS performs a BM25-ranked FTS5 search.
func (s *SQLiteStore) searchFTS(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	query = quoteFTSQuery(query)
	if query == "" {
		return nil, nil
	}

	q := `SELECT f.id, f.namespace, f.content, f.subject, f.category, f.metadata,
	             f.superseded_by, f.superseded_at, f.embedding, f.created_at, rank
	      FROM memstore_facts_fts fts
	      JOIN memstore_facts f ON f.id = fts.rowid
	      WHERE memstore_facts_fts MATCH ?`

	args := []any{query}

	s.appendNamespaceFilter(&q, &args, "f.namespace", opts.Namespaces)
	if opts.OnlyActive {
		q += ` AND f.superseded_by IS NULL`
	}
	if opts.Subject != "" {
		q += ` AND f.subject = ?`
		args = append(args, opts.Subject)
	}
	if opts.Category != "" {
		q += ` AND f.category = ?`
		args = append(args, opts.Category)
	}
	if err := appendMetadataFilters(&q, &args, "f.", opts.MetadataFilters); err != nil {
		return nil, err
	}
	appendTemporalFilters(&q, &args, "f.", opts.CreatedAfter, opts.CreatedBefore)

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

	s.appendNamespaceFilter(&q, &args, "namespace", opts.Namespaces)
	if opts.OnlyActive {
		q += ` AND superseded_by IS NULL`
	}
	if opts.Subject != "" {
		q += ` AND subject = ?`
		args = append(args, opts.Subject)
	}
	if opts.Category != "" {
		q += ` AND category = ?`
		args = append(args, opts.Category)
	}
	if err := appendMetadataFilters(&q, &args, "", opts.MetadataFilters); err != nil {
		return nil, err
	}
	appendTemporalFilters(&q, &args, "", opts.CreatedAfter, opts.CreatedBefore)

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

	// Compute combined score with configurable weights and optional time decay.
	now := time.Now()
	merged := make([]SearchResult, 0, len(byID))
	for _, r := range byID {
		r.Combined = opts.FTSWeight*r.FTSScore + opts.VecWeight*r.VecScore
		if opts.DecayHalfLife > 0 {
			age := now.Sub(r.Fact.CreatedAt).Seconds()
			r.Combined *= math.Pow(0.5, age/opts.DecayHalfLife.Seconds())
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

// SearchBatch performs hybrid search for multiple queries, sharing a single
// batched embedding call across all queries to avoid N separate API calls.
// Unlike Search, SearchBatch requires an embedder — batched embedding is its
// reason to exist. Returns an error if no embedder is configured or if
// embedding fails after retries.
func (s *SQLiteStore) SearchBatch(ctx context.Context, queries []string, opts SearchOpts) ([][]SearchResult, error) {
	if len(queries) == 0 {
		return nil, nil
	}
	if s.embedder == nil {
		return nil, fmt.Errorf("memstore: SearchBatch requires an embedder")
	}

	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	if opts.FTSWeight == 0 && opts.VecWeight == 0 {
		opts.FTSWeight = 0.6
		opts.VecWeight = 0.4
	}

	queryEmbs, err := embedWithRetry(ctx, s.embedder, queries)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([][]SearchResult, len(queries))
	for i, query := range queries {
		ftsResults, err := s.searchFTS(ctx, query, opts)
		if err != nil {
			return nil, err
		}

		var vecResults []SearchResult
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
