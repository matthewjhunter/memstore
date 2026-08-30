package pgstore

// Embedding for the document corpus.
//
// Until this landed, memstore_document_chunks had no vector column and
// SearchDocumentChunks was an exact FTS pass with a decomposed-identifier
// fallback -- no semantic ranking of any kind, while facts had hybrid search
// and reranking. Keyword-only matching is the regime where prose that repeats
// the query terms beats prose that defines them, which is exactly what was
// observed: a promotional comment outranked the passage it was advertising a
// product about.
//
// The lifecycle mirrors the fact side deliberately (NeedingEmbedding /
// SetFactVectors / MarkEmbedFailed) so the same queue drains both and neither
// grows its own rules.

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/pgvector/pgvector-go"

	"github.com/matthewjhunter/memstore"
)

// migrateV12 gives document chunks a vector, and the two columns that keep a
// permanently unembeddable chunk from jamming the queue.
//
// The HNSW index is created only when a dimension is configured, matching the
// fact index in migrateV1: pgvector cannot index an unconstrained vector
// column, and no deployment here sets vec-dim.
func (s *PostgresStore) migrateV12(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE memstore_document_chunks ADD COLUMN IF NOT EXISTS embedding ` + s.vectorColumnType(),
		`ALTER TABLE memstore_document_chunks ADD COLUMN IF NOT EXISTS embed_failed_at TIMESTAMPTZ`,
		`ALTER TABLE memstore_document_chunks ADD COLUMN IF NOT EXISTS embed_error TEXT`,
	}
	if s.vecDim > 0 {
		stmts = append(stmts,
			`CREATE INDEX IF NOT EXISTS idx_memstore_doc_chunk_embedding
			   ON memstore_document_chunks USING hnsw (embedding vector_cosine_ops)`)
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrateV12: %w\nstatement: %s", err, q)
		}
	}
	return nil
}

// ChunksNeedingEmbedding returns chunks with no vector that have not been
// quarantined, oldest first, with the document basename each one needs for its
// embed header.
//
// The join is what makes the header cheap: ChunkEmbedText wants the document
// path, and fetching it per chunk would be one query per row for a value the
// queue already has to look up anyway.
func (s *PostgresStore) ChunksNeedingEmbedding(ctx context.Context, limit int) ([]memstore.PendingChunk, error) {
	if limit <= 0 {
		limit = 100
	}
	var b queryBuilder
	b.q = `SELECT d.path,
			c.id, c.document_id, c.ordinal, c.content, c.byte_start, c.byte_end, c.line_start, c.line_end,
			c.heading_path, c.heading_level, c.lang,
			c.package, c.import_path, c.symbol, c.receiver, c.decl_kind, c.exported, c.signature, c.scope_path, c.imports_used,
			c.created_at
		FROM memstore_document_chunks c
		JOIN memstore_documents d
		  ON d.id = c.document_id AND d.namespace = c.namespace AND d.user_id = c.user_id
		WHERE c.embedding IS NULL AND c.embed_failed_at IS NULL`
	b.write(` AND c.namespace = `, s.namespace)
	s.appendUserFilter(&b, "c.user_id")
	b.q += ` ORDER BY c.id`
	b.write(` LIMIT `, limit)

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: ChunksNeedingEmbedding: %w", err)
	}
	defer rows.Close()

	var out []memstore.PendingChunk
	for rows.Next() {
		var p memstore.PendingChunk
		c := &p.Chunk
		if err := rows.Scan(
			&p.DocPath,
			&c.ID, &c.DocumentID, &c.Ordinal, &c.Content, &c.ByteStart, &c.ByteEnd, &c.LineStart, &c.LineEnd,
			&c.HeadingPath, &c.HeadingLevel, &c.Lang,
			&c.Package, &c.ImportPath, &c.Symbol, &c.Receiver, &c.DeclKind, &c.Exported, &c.Signature, &c.ScopePath, &c.ImportsUsed,
			&c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("pgstore: ChunksNeedingEmbedding scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetChunkVector stores a chunk's embedding and clears any prior failure, so a
// chunk that failed under one embedder can be retried under another.
func (s *PostgresStore) SetChunkVector(ctx context.Context, id int64, vec []float32) error {
	var b queryBuilder
	b.q = `UPDATE memstore_document_chunks SET embedding = `
	b.write(``, pgvector.NewVector(vec))
	b.q += `, embed_failed_at = NULL, embed_error = NULL WHERE id = `
	b.write(``, id)
	b.write(` AND namespace = `, s.namespace)
	s.appendUserFilter(&b, "user_id")
	if _, err := s.pool.Exec(ctx, b.q, b.args...); err != nil {
		return fmt.Errorf("pgstore: SetChunkVector: %w", err)
	}
	return nil
}

// MarkChunkEmbedFailed quarantines a chunk the embedder cannot handle.
//
// Without it one bad chunk is returned by every poll forever and nothing
// behind it is ever embedded. The fact side learned this; the document side
// inherits the lesson rather than repeating it.
func (s *PostgresStore) MarkChunkEmbedFailed(ctx context.Context, id int64, reason string) error {
	var b queryBuilder
	b.q = `UPDATE memstore_document_chunks SET embed_failed_at = now(), embed_error = `
	b.write(``, reason)
	b.q += ` WHERE id = `
	b.write(``, id)
	b.write(` AND namespace = `, s.namespace)
	s.appendUserFilter(&b, "user_id")
	if _, err := s.pool.Exec(ctx, b.q, b.args...); err != nil {
		return fmt.Errorf("pgstore: MarkChunkEmbedFailed: %w", err)
	}
	return nil
}

// docChunkSelect is the column list shared by the FTS and vector passes, so
// the two produce rows the same scanner can read. Kept next to
// scanDocChunkRows for the same reason factColumns and scanFact are kept
// together: they must change as a pair.
const docChunkSelect = `c.id, c.document_id, c.ordinal, c.content, c.byte_start, c.byte_end, c.line_start, c.line_end,
			c.heading_path, c.heading_level, c.lang,
			c.package, c.import_path, c.symbol, c.receiver, c.decl_kind, c.exported, c.signature, c.scope_path, c.imports_used,
			c.created_at,
			d.repo_url, d.commit, d.path, d.basename, d.lang, d.trusted, d.dirty, d.generated, d.is_test`

// searchDocChunksVector ranks chunks by cosine similarity to the query vector.
//
// Unlike the fact side there is no DISTINCT ON: a fact is a set of chunk
// vectors and has to be collapsed to its best one, whereas a document chunk is
// itself the unit of retrieval. Chunks of one document legitimately compete
// with each other, because a citation names a passage rather than a file.
func (s *PostgresStore) searchDocChunksVector(ctx context.Context, queryEmb []float32, opts memstore.DocumentSearchOpts, limit int) ([]memstore.DocumentSearchResult, error) {
	qv := pgvector.NewVector(queryEmb)

	var b queryBuilder
	b.write(`SELECT `+docChunkSelect+`, 1 - (c.embedding <=> `, qv)
	b.q += `) AS similarity
		FROM memstore_document_chunks c
		JOIN memstore_documents d
		  ON d.id = c.document_id AND d.namespace = c.namespace AND d.user_id = c.user_id
		WHERE c.embedding IS NOT NULL`
	b.write(` AND c.namespace = `, s.namespace)
	s.appendUserFilter(&b, "c.user_id")
	if !opts.IncludeGenerated {
		b.q += ` AND NOT d.generated`
	}
	if opts.RepoURL != "" {
		b.write(` AND d.repo_url = `, opts.RepoURL)
	}
	if opts.PathPrefix != "" {
		b.write(` AND starts_with(d.path, `, opts.PathPrefix)
		b.q += `)`
	}
	if opts.Basename != "" {
		b.write(` AND d.basename = `, opts.Basename)
	}
	if opts.Lang != "" {
		b.write(` AND d.lang = `, opts.Lang)
	}
	b.write(` ORDER BY c.embedding <=> `, qv)
	b.write(` LIMIT `, limit)

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: document vector search: %w", err)
	}
	defer rows.Close()
	return scanDocChunkRows(rows)
}

// fuseDocResults merges the FTS and vector passes exactly as mergeFirstStage
// does for facts: FTS normalized against its own maximum so a raw ts_rank
// cannot dominate a cosine similarity, then a weighted sum.
//
// Normalizing matters more than it looks. ts_rank is unbounded and
// scale-dependent, cosine similarity is [0,1]; summing them raw would make the
// vector pass a rounding error on a query where FTS happened to score high,
// which is the ranking this whole change exists to fix.
func fuseDocResults(fts, vec []memstore.DocumentSearchResult, ftsWeight, vecWeight float64) []memstore.DocumentSearchResult {
	byID := make(map[int64]*memstore.DocumentSearchResult, len(fts)+len(vec))
	order := make([]int64, 0, len(fts)+len(vec))

	var maxFTS float64
	for _, r := range fts {
		if r.Score > maxFTS {
			maxFTS = r.Score
		}
	}
	for _, r := range fts {
		norm := r.Score
		if maxFTS > 0 {
			norm = r.Score / maxFTS
		}
		cp := r
		cp.FTSScore = norm
		byID[r.Chunk.ID] = &cp
		order = append(order, r.Chunk.ID)
	}
	for _, r := range vec {
		if existing, ok := byID[r.Chunk.ID]; ok {
			existing.VecScore = r.Score
			continue
		}
		cp := r
		cp.VecScore = r.Score
		cp.Score = 0
		byID[r.Chunk.ID] = &cp
		order = append(order, r.Chunk.ID)
	}

	out := make([]memstore.DocumentSearchResult, 0, len(byID))
	for _, id := range order {
		r := byID[id]
		r.Score = ftsWeight*r.FTSScore + vecWeight*r.VecScore
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// Fusion weights for document search, matching the fact side's defaults
// (SearchOpts.FTSWeight 0.6 / VecWeight 0.4). One set of numbers for both
// corpora until there is a measurement that says they should differ -- and
// measuring that is what having document vectors at all makes possible.
const (
	docFTSWeight = 0.6
	docVecWeight = 0.4
)

// searchDocChunksVectorFor embeds the query and runs the vector pass, or
// returns nothing when the store cannot embed.
//
// The query is rendered by FactQueryText, shared with the fact side rather
// than duplicated. Both corpora are searched with the same queries, and a
// second spelling of the query task is a thing that can silently drift out of
// step with the document task ChunkEmbedText applies.
func (s *PostgresStore) searchDocChunksVectorFor(ctx context.Context, query string, opts memstore.DocumentSearchOpts) ([]memstore.DocumentSearchResult, error) {
	if s.embedder == nil {
		return nil, nil
	}
	qv, err := s.queryCache.Single(ctx, s.embedder, memstore.FactQueryText(s.embedder.Model(), query))
	if err != nil {
		// A retrieval degradation, not a failure: the keyword pass already
		// produced results and returning an error would turn a slow embedder
		// into an outage of a search that used to work without one.
		log.Printf("document search: query embedding unavailable, keyword-only: %v", err)
		return nil, nil
	}
	return s.searchDocChunksVector(ctx, qv, opts, opts.MaxResults)
}
