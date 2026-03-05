// Package pgstore implements the memstore.Store interface backed by PostgreSQL
// with pgvector for vector search and tsvector/GIN for full-text search.
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	pgvector "github.com/pgvector/pgvector-go"
)

const schemaVersion = 1

// factColumns is the canonical SELECT list for fact queries.
// searchFTS has its own column list because it joins and adds ts_rank.
const factColumns = `id, namespace, content, subject, category, kind, subsystem, metadata, superseded_by, superseded_at, confirmed_count, last_confirmed_at, use_count, last_used_at, embedding, created_at`

// PostgresStore implements memstore.Store backed by PostgreSQL.
// It uses pgvector for vector similarity search and tsvector with GIN
// indexing for full-text search. No mutex is needed — Postgres handles
// concurrency natively via MVCC.
type PostgresStore struct {
	pool      *pgxpool.Pool
	embedder  memstore.Embedder
	namespace string
	vecDim    int // embedding dimension, set at construction or first embed
}

// New creates a new PostgresStore using the given connection pool.
// It creates memstore_* tables if needed and runs any pending migrations.
//
// The namespace parameter partitions facts for multi-tenant isolation.
// vecDim is the embedding vector dimension (e.g. 768 for embeddinggemma).
// If vecDim is 0, embedding columns are created without a dimension constraint.
func New(ctx context.Context, pool *pgxpool.Pool, embedder memstore.Embedder, namespace string, vecDim int) (*PostgresStore, error) {
	s := &PostgresStore{
		pool:      pool,
		embedder:  embedder,
		namespace: namespace,
		vecDim:    vecDim,
	}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("pgstore: migration: %w", err)
	}
	if embedder != nil {
		if err := s.validateEmbedder(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	// Ensure pgvector extension exists.
	if _, err := s.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("creating pgvector extension: %w", err)
	}

	// Version tracking table.
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS memstore_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("creating version table: %w", err)
	}

	var version int
	err := s.pool.QueryRow(ctx, `SELECT version FROM memstore_version`).Scan(&version)
	if err == pgx.ErrNoRows {
		version = 0
	} else if err != nil {
		return fmt.Errorf("reading version: %w", err)
	}

	if version >= schemaVersion {
		return nil
	}

	if version < 1 {
		if err := s.migrateV1(ctx); err != nil {
			return err
		}
	}

	if version == 0 {
		_, err = s.pool.Exec(ctx, `INSERT INTO memstore_version (version) VALUES ($1)`, schemaVersion)
	} else {
		_, err = s.pool.Exec(ctx, `UPDATE memstore_version SET version = $1`, schemaVersion)
	}
	return err
}

func (s *PostgresStore) migrateV1(ctx context.Context) error {
	// Build vector column type. If vecDim is set, use vector(N) for HNSW index support.
	vecType := "vector"
	if s.vecDim > 0 {
		vecType = fmt.Sprintf("vector(%d)", s.vecDim)
	}

	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memstore_facts (
			id               BIGSERIAL PRIMARY KEY,
			namespace        TEXT NOT NULL DEFAULT '',
			content          TEXT NOT NULL,
			subject          TEXT NOT NULL,
			category         TEXT NOT NULL,
			kind             TEXT NOT NULL DEFAULT '',
			subsystem        TEXT NOT NULL DEFAULT '',
			metadata         JSONB,
			superseded_by    BIGINT REFERENCES memstore_facts(id),
			superseded_at    TIMESTAMPTZ,
			confirmed_count  INTEGER NOT NULL DEFAULT 0,
			last_confirmed_at TIMESTAMPTZ,
			use_count        INTEGER NOT NULL DEFAULT 0,
			last_used_at     TIMESTAMPTZ,
			embedding        %s,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			fts              TSVECTOR GENERATED ALWAYS AS (
				setweight(to_tsvector('english', coalesce(subject, '')), 'A') ||
				setweight(to_tsvector('english', coalesce(content, '')), 'B') ||
				setweight(to_tsvector('english', coalesce(category, '')), 'C')
			) STORED
		)`, vecType),

		`CREATE INDEX IF NOT EXISTS idx_memstore_fts ON memstore_facts USING GIN (fts)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_subject ON memstore_facts (subject)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_category ON memstore_facts (category)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_kind ON memstore_facts (kind)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_subsystem ON memstore_facts (subsystem)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_namespace ON memstore_facts (namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_active ON memstore_facts (id) WHERE superseded_by IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_metadata ON memstore_facts USING GIN (metadata)`,

		`CREATE TABLE IF NOT EXISTS memstore_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS memstore_links (
			id            BIGSERIAL PRIMARY KEY,
			namespace     TEXT NOT NULL DEFAULT '',
			source_id     BIGINT NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			target_id     BIGINT NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			link_type     TEXT NOT NULL DEFAULT 'reference',
			bidirectional BOOLEAN NOT NULL DEFAULT FALSE,
			label         TEXT NOT NULL DEFAULT '',
			metadata      JSONB,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_source ON memstore_links (namespace, source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_target ON memstore_links (namespace, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_type ON memstore_links (namespace, link_type)`,
	}

	// Add HNSW index for vector search if dimension is known.
	if s.vecDim > 0 {
		stmts = append(stmts,
			`CREATE INDEX IF NOT EXISTS idx_memstore_embedding ON memstore_facts USING hnsw (embedding vector_cosine_ops)`,
		)
	}

	for _, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pgstore V1 migration: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

func (s *PostgresStore) validateEmbedder(ctx context.Context) error {
	var stored string
	err := s.pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&stored)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pgstore: reading embedding model: %w", err)
	}
	if got := s.embedder.Model(); got != stored {
		return fmt.Errorf("pgstore: embedding model mismatch: store has %q, embedder provides %q", stored, got)
	}
	return nil
}

func (s *PostgresStore) recordEmbedder(ctx context.Context, dim int) error {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&count); err != nil {
		return fmt.Errorf("pgstore: checking meta: %w", err)
	}
	if count > 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO memstore_meta (key, value) VALUES ('embedding_model', $1)`, s.embedder.Model()); err != nil {
		return fmt.Errorf("pgstore: recording embedding model: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO memstore_meta (key, value) VALUES ('embedding_dim', $1)`, fmt.Sprintf("%d", dim)); err != nil {
		return fmt.Errorf("pgstore: recording embedding dim: %w", err)
	}
	return nil
}

// Insert adds a single fact and returns its ID.
func (s *PostgresStore) Insert(ctx context.Context, f memstore.Fact) (int64, error) {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}

	var emb *pgvector.Vector
	if len(f.Embedding) > 0 {
		v := pgvector.NewVector(f.Embedding)
		emb = &v
	}

	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO memstore_facts (namespace, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		s.namespace, f.Content, f.Subject, f.Category, f.Kind, f.Subsystem,
		nullableJSON(f.Metadata), f.SupersededBy, emb, f.CreatedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pgstore: inserting fact: %w", err)
	}
	return id, nil
}

// InsertBatch inserts multiple facts in a single transaction.
func (s *PostgresStore) InsertBatch(ctx context.Context, facts []memstore.Fact) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	for i := range facts {
		if facts[i].CreatedAt.IsZero() {
			facts[i].CreatedAt = now
		}

		var emb *pgvector.Vector
		if len(facts[i].Embedding) > 0 {
			v := pgvector.NewVector(facts[i].Embedding)
			emb = &v
		}

		err := tx.QueryRow(ctx,
			`INSERT INTO memstore_facts (namespace, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 RETURNING id`,
			s.namespace, facts[i].Content, facts[i].Subject, facts[i].Category, facts[i].Kind, facts[i].Subsystem,
			nullableJSON(facts[i].Metadata), facts[i].SupersededBy, emb, facts[i].CreatedAt,
		).Scan(&facts[i].ID)
		if err != nil {
			return fmt.Errorf("pgstore: inserting fact %q: %w", facts[i].Content, err)
		}
	}

	return tx.Commit(ctx)
}

// Supersede marks an old fact as superseded by a new fact.
func (s *PostgresStore) Supersede(ctx context.Context, oldID, newID int64) error {
	now := time.Now().UTC()
	ct, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET superseded_by = $1, superseded_at = $2
		 WHERE id = $3 AND namespace = $4 AND superseded_by IS NULL`,
		newID, now, oldID, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: superseding fact %d: %w", oldID, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: fact %d not found or already superseded", oldID)
	}
	return nil
}

// Confirm increments a fact's confirmed_count and updates last_confirmed_at.
func (s *PostgresStore) Confirm(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	ct, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET confirmed_count = confirmed_count + 1, last_confirmed_at = $1
		 WHERE id = $2 AND namespace = $3`,
		now, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: confirming fact %d: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: fact %d not found", id)
	}
	return nil
}

// Touch increments use_count and updates last_used_at for the given fact IDs.
func (s *PostgresStore) Touch(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	now := time.Now().UTC()
	// pgx supports ANY($1::bigint[]) for IN-list queries.
	_, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET use_count = use_count + 1, last_used_at = $1
		 WHERE namespace = $2 AND id = ANY($3::bigint[])`,
		now, s.namespace, ids,
	)
	if err != nil {
		return fmt.Errorf("pgstore: touching facts: %w", err)
	}
	return nil
}

// UpdateMetadata merges a patch into the metadata JSON for a fact.
func (s *PostgresStore) UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error {
	// Read current metadata.
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT metadata FROM memstore_facts WHERE id = $1 AND namespace = $2`,
		id, s.namespace,
	).Scan(&raw)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("pgstore: fact %d not found", id)
	}
	if err != nil {
		return fmt.Errorf("pgstore: reading metadata for fact %d: %w", id, err)
	}

	existing := make(map[string]any)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("pgstore: unmarshaling metadata for fact %d: %w", id, err)
		}
	}

	for k, v := range patch {
		if v == nil {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}

	merged, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("pgstore: marshaling metadata for fact %d: %w", id, err)
	}

	_, err = s.pool.Exec(ctx,
		`UPDATE memstore_facts SET metadata = $1 WHERE id = $2 AND namespace = $3`,
		merged, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: updating metadata for fact %d: %w", id, err)
	}
	return nil
}

// Delete removes a fact by ID.
func (s *PostgresStore) Delete(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM memstore_facts WHERE id = $1 AND namespace = $2`, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: deleting fact %d: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: fact %d not found", id)
	}
	return nil
}

// Get retrieves a single fact by ID. Returns nil if not found.
func (s *PostgresStore) Get(ctx context.Context, id int64) (*memstore.Fact, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`, id, s.namespace,
	)
	f, err := scanFact(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: getting fact %d: %w", id, err)
	}
	return f, nil
}

// List returns facts matching the given filters, ordered by ID.
func (s *PostgresStore) List(ctx context.Context, opts memstore.QueryOpts) ([]memstore.Fact, error) {
	var b queryBuilder
	b.write(`SELECT ` + factColumns + ` FROM memstore_facts WHERE 1=1`)
	s.appendNamespaceFilter(&b, "namespace", opts.Namespaces)

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
	if opts.OnlyActive {
		b.q += ` AND superseded_by IS NULL`
	}
	if len(opts.IDs) > 0 {
		b.write(` AND id = ANY(`, opts.IDs)
		b.q += `::bigint[])`
	}
	appendMetadataFilters(&b, "", opts.MetadataFilters)
	appendTemporalFilters(&b, "", opts.CreatedAfter, opts.CreatedBefore)

	b.q += ` ORDER BY id`

	if opts.Limit > 0 {
		b.write(` LIMIT `, opts.Limit)
	}

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: listing facts: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// BySubject returns facts for a given subject.
func (s *PostgresStore) BySubject(ctx context.Context, subject string, onlyActive bool) ([]memstore.Fact, error) {
	var b queryBuilder
	b.write(`SELECT `+factColumns+` FROM memstore_facts WHERE subject = `, subject)
	b.write(` AND namespace = `, s.namespace)
	if onlyActive {
		b.q += ` AND superseded_by IS NULL`
	}
	b.q += ` ORDER BY id`

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: querying by subject: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// Exists checks whether a fact with the same content and subject exists.
func (s *PostgresStore) Exists(ctx context.Context, content, subject string) (bool, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memstore_facts WHERE content = $1 AND subject = $2 AND namespace = $3`,
		content, subject, s.namespace,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("pgstore: checking existence: %w", err)
	}
	return count > 0, nil
}

// ActiveCount returns the number of non-superseded facts.
func (s *PostgresStore) ActiveCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM memstore_facts WHERE superseded_by IS NULL AND namespace = $1`,
		s.namespace,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("pgstore: counting active facts: %w", err)
	}
	return count, nil
}

// NeedingEmbedding returns facts that don't have embeddings yet.
func (s *PostgresStore) NeedingEmbedding(ctx context.Context, limit int) ([]memstore.Fact, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+factColumns+`
		 FROM memstore_facts WHERE embedding IS NULL AND namespace = $1 ORDER BY id LIMIT $2`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("pgstore: querying unembedded facts: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// SetEmbedding stores a computed embedding for a fact.
func (s *PostgresStore) SetEmbedding(ctx context.Context, id int64, emb []float32) error {
	v := pgvector.NewVector(emb)
	_, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET embedding = $1 WHERE id = $2 AND namespace = $3`,
		v, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: setting embedding for fact %d: %w", id, err)
	}
	return nil
}

// EmbedFacts generates embeddings for all facts that don't have one yet.
func (s *PostgresStore) EmbedFacts(ctx context.Context, batchSize int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("pgstore: no embedder configured")
	}
	if batchSize <= 0 {
		batchSize = 50
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, content FROM memstore_facts WHERE embedding IS NULL AND namespace = $1 ORDER BY id`,
		s.namespace)
	if err != nil {
		return 0, fmt.Errorf("pgstore: querying unembedded facts: %w", err)
	}

	type idContent struct {
		id      int64
		content string
	}
	var pending []idContent

	for rows.Next() {
		var ic idContent
		if err := rows.Scan(&ic.id, &ic.content); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pgstore: scanning fact: %w", err)
		}
		pending = append(pending, ic)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pgstore: iterating facts: %w", err)
	}

	if len(pending) == 0 {
		return 0, nil
	}

	total := 0
	for i := 0; i < len(pending); i += batchSize {
		end := min(i+batchSize, len(pending))
		batch := pending[i:end]

		texts := make([]string, len(batch))
		for j, ic := range batch {
			texts[j] = ic.content
		}

		embeddings, err := memstore.EmbedWithRetry(ctx, s.embedder, texts)
		if err != nil {
			return total, err
		}

		if len(embeddings) != len(batch) {
			return total, fmt.Errorf("pgstore: embedding count mismatch: got %d, want %d", len(embeddings), len(batch))
		}

		if total == 0 && i == 0 && len(embeddings[0]) > 0 {
			if err := s.recordEmbedder(ctx, len(embeddings[0])); err != nil {
				return 0, err
			}
		}

		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return total, fmt.Errorf("pgstore: beginning tx: %w", err)
		}

		for j, emb := range embeddings {
			v := pgvector.NewVector(emb)
			if _, err := tx.Exec(ctx,
				`UPDATE memstore_facts SET embedding = $1 WHERE id = $2`,
				v, batch[j].id,
			); err != nil {
				tx.Rollback(ctx)
				return total, fmt.Errorf("pgstore: updating fact %d: %w", batch[j].id, err)
			}
		}

		if err := tx.Commit(ctx); err != nil {
			return total, fmt.Errorf("pgstore: committing batch: %w", err)
		}

		total += len(batch)
	}

	return total, nil
}

// History returns the supersession chain for a fact.
func (s *PostgresStore) History(ctx context.Context, id int64, subject string) ([]memstore.HistoryEntry, error) {
	if id > 0 {
		return s.historyByID(ctx, id)
	}
	if subject != "" {
		return s.historyBySubject(ctx, subject)
	}
	return nil, fmt.Errorf("pgstore: History requires either id or subject")
}

func (s *PostgresStore) historyByID(ctx context.Context, id int64) ([]memstore.HistoryEntry, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`, id, s.namespace)
	anchor, err := scanFact(row)
	if err != nil {
		return nil, fmt.Errorf("pgstore: fact %d not found: %w", id, err)
	}

	// Walk backward.
	var backward []memstore.Fact
	current := anchor.ID
	for {
		row := s.pool.QueryRow(ctx,
			`SELECT `+factColumns+` FROM memstore_facts WHERE superseded_by = $1 AND namespace = $2`,
			current, s.namespace)
		pred, err := scanFact(row)
		if err != nil {
			break
		}
		backward = append(backward, *pred)
		current = pred.ID
	}

	chain := make([]memstore.Fact, 0, len(backward)+1)
	for i := len(backward) - 1; i >= 0; i-- {
		chain = append(chain, backward[i])
	}
	chain = append(chain, *anchor)

	// Walk forward.
	if anchor.SupersededBy != nil {
		next := *anchor.SupersededBy
		for {
			row := s.pool.QueryRow(ctx,
				`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`,
				next, s.namespace)
			succ, err := scanFact(row)
			if err != nil {
				break
			}
			chain = append(chain, *succ)
			if succ.SupersededBy == nil {
				break
			}
			next = *succ.SupersededBy
		}
	}

	entries := make([]memstore.HistoryEntry, len(chain))
	for i, f := range chain {
		entries[i] = memstore.HistoryEntry{Fact: f, Position: i, ChainLength: len(chain)}
	}
	return entries, nil
}

func (s *PostgresStore) historyBySubject(ctx context.Context, subject string) ([]memstore.HistoryEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+factColumns+` FROM memstore_facts WHERE subject = $1 AND namespace = $2 ORDER BY created_at, id`,
		subject, s.namespace)
	if err != nil {
		return nil, fmt.Errorf("pgstore: history by subject: %w", err)
	}
	defer rows.Close()

	facts, err := scanFacts(rows)
	if err != nil {
		return nil, err
	}

	entries := make([]memstore.HistoryEntry, len(facts))
	for i, f := range facts {
		entries[i] = memstore.HistoryEntry{Fact: f, Position: i, ChainLength: len(facts)}
	}
	return entries, nil
}

// ListSubsystems returns all distinct non-empty subsystem values.
func (s *PostgresStore) ListSubsystems(ctx context.Context, subject string) ([]string, error) {
	var b queryBuilder
	b.write(`SELECT DISTINCT subsystem FROM memstore_facts WHERE namespace = `, s.namespace)
	b.q += ` AND superseded_by IS NULL AND subsystem != ''`
	if subject != "" {
		b.write(` AND subject = `, subject)
	}
	b.q += ` ORDER BY subsystem`

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: listing subsystems: %w", err)
	}
	defer rows.Close()

	var subsystems []string
	for rows.Next() {
		var ss string
		if err := rows.Scan(&ss); err != nil {
			return nil, fmt.Errorf("pgstore: scanning subsystem: %w", err)
		}
		subsystems = append(subsystems, ss)
	}
	return subsystems, rows.Err()
}

// Close is a no-op; the caller owns the connection pool.
func (s *PostgresStore) Close() error {
	return nil
}

// --- Link methods ---

const linkColumns = `id, namespace, source_id, target_id, link_type, bidirectional, label, metadata, created_at`

// LinkFacts creates a directed edge between two facts.
func (s *PostgresStore) LinkFacts(ctx context.Context, sourceID, targetID int64, linkType string, bidirectional bool, label string, metadata map[string]any) (int64, error) {
	var metaJSON []byte
	if len(metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(metadata)
		if err != nil {
			return 0, fmt.Errorf("pgstore: marshaling link metadata: %w", err)
		}
	}

	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO memstore_links (namespace, source_id, target_id, link_type, bidirectional, label, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		s.namespace, sourceID, targetID, linkType, bidirectional, label, nullableBytes(metaJSON), time.Now().UTC(),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pgstore: creating link %d->%d: %w", sourceID, targetID, err)
	}
	return id, nil
}

// GetLink retrieves a single link by ID.
func (s *PostgresStore) GetLink(ctx context.Context, linkID int64) (*memstore.Link, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+linkColumns+` FROM memstore_links WHERE id = $1 AND namespace = $2`,
		linkID, s.namespace,
	)
	l, err := scanLink(row)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("pgstore: link %d not found", linkID)
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: getting link %d: %w", linkID, err)
	}
	return l, nil
}

// GetLinks returns edges touching factID filtered by direction.
func (s *PostgresStore) GetLinks(ctx context.Context, factID int64, direction memstore.LinkDirection, linkTypes ...string) ([]memstore.Link, error) {
	var b queryBuilder

	switch direction {
	case memstore.LinkOutbound:
		b.write(`SELECT `+linkColumns+` FROM memstore_links WHERE namespace = `, s.namespace)
		b.write(` AND (source_id = `, factID)
		b.write(` OR (target_id = `, factID)
		b.q += ` AND bidirectional = TRUE))`
	case memstore.LinkInbound:
		b.write(`SELECT `+linkColumns+` FROM memstore_links WHERE namespace = `, s.namespace)
		b.write(` AND (target_id = `, factID)
		b.write(` OR (source_id = `, factID)
		b.q += ` AND bidirectional = TRUE))`
	default: // LinkBoth
		b.write(`SELECT `+linkColumns+` FROM memstore_links WHERE namespace = `, s.namespace)
		b.write(` AND (source_id = `, factID)
		b.write(` OR target_id = `, factID)
		b.q += `)`
	}

	if len(linkTypes) > 0 {
		b.write(` AND link_type = ANY(`, linkTypes)
		b.q += `::text[])`
	}

	b.q += ` ORDER BY id`

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: getting links for fact %d: %w", factID, err)
	}
	defer rows.Close()

	return scanLinks(rows)
}

// UpdateLink patches the label and/or metadata of an existing link.
func (s *PostgresStore) UpdateLink(ctx context.Context, linkID int64, label string, metadata map[string]any) error {
	var currentLabel string
	var metaRaw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT label, metadata FROM memstore_links WHERE id = $1 AND namespace = $2`,
		linkID, s.namespace,
	).Scan(&currentLabel, &metaRaw)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("pgstore: link %d not found", linkID)
	}
	if err != nil {
		return fmt.Errorf("pgstore: reading link %d: %w", linkID, err)
	}

	newLabel := currentLabel
	if label != "" {
		newLabel = label
	}

	existing := make(map[string]any)
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &existing); err != nil {
			return fmt.Errorf("pgstore: unmarshaling link metadata %d: %w", linkID, err)
		}
	}
	for k, v := range metadata {
		if v == nil {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}

	var metaJSON []byte
	if len(existing) > 0 {
		metaJSON, err = json.Marshal(existing)
		if err != nil {
			return fmt.Errorf("pgstore: marshaling link metadata %d: %w", linkID, err)
		}
	}

	_, err = s.pool.Exec(ctx,
		`UPDATE memstore_links SET label = $1, metadata = $2 WHERE id = $3 AND namespace = $4`,
		newLabel, nullableBytes(metaJSON), linkID, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: updating link %d: %w", linkID, err)
	}
	return nil
}

// DeleteLink removes a link by ID.
func (s *PostgresStore) DeleteLink(ctx context.Context, linkID int64) error {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM memstore_links WHERE id = $1 AND namespace = $2`, linkID, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: deleting link %d: %w", linkID, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: link %d not found", linkID)
	}
	return nil
}

// --- scan helpers ---

// scanner abstracts pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanFact(row scanner) (*memstore.Fact, error) {
	var f memstore.Fact
	var metadata []byte
	var supersededBy *int64
	var supersededAt *time.Time
	var lastConfirmedAt *time.Time
	var lastUsedAt *time.Time
	var emb *pgvector.Vector

	err := row.Scan(
		&f.ID, &f.Namespace, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
		&metadata, &supersededBy, &supersededAt,
		&f.ConfirmedCount, &lastConfirmedAt,
		&f.UseCount, &lastUsedAt,
		&emb, &f.CreatedAt,
	)
	if err != nil {
		return nil, err
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

	return &f, nil
}

func scanFacts(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]memstore.Fact, error) {
	var facts []memstore.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: scanning fact: %w", err)
		}
		facts = append(facts, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: iterating facts: %w", err)
	}
	return facts, nil
}

func scanLink(row scanner) (*memstore.Link, error) {
	var l memstore.Link
	var metaRaw []byte
	var namespace string

	err := row.Scan(&l.ID, &namespace, &l.SourceID, &l.TargetID, &l.LinkType,
		&l.Bidirectional, &l.Label, &metaRaw, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(metaRaw) > 0 {
		l.Metadata = json.RawMessage(metaRaw)
	}
	return &l, nil
}

func scanLinks(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]memstore.Link, error) {
	var links []memstore.Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: scanning link: %w", err)
		}
		links = append(links, *l)
	}
	return links, rows.Err()
}

// --- query builder helpers ---

// queryBuilder accumulates a parameterized query with numbered placeholders.
type queryBuilder struct {
	q    string
	args []any
}

// write appends SQL text, and if a value is provided, appends a numbered placeholder.
func (b *queryBuilder) write(sql string, vals ...any) {
	if len(vals) == 0 {
		b.q += sql
		return
	}
	b.args = append(b.args, vals[0])
	b.q += sql + fmt.Sprintf("$%d", len(b.args))
}

func (s *PostgresStore) appendNamespaceFilter(b *queryBuilder, nsCol string, namespaces []string) {
	if len(namespaces) > 0 {
		b.args = append(b.args, namespaces)
		b.q += fmt.Sprintf(` AND %s = ANY($%d::text[])`, nsCol, len(b.args))
	} else {
		b.write(` AND `+nsCol+` = `, s.namespace)
	}
}

// validMetadataKey checks that a metadata key contains only safe characters.
func validMetadataKey(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

var validMetadataOps = map[string]bool{
	"=": true, "!=": true,
	"<": true, "<=": true,
	">": true, ">=": true,
}

// appendMetadataFilters adds jsonb-based WHERE clauses using the ->> operator.
func appendMetadataFilters(b *queryBuilder, alias string, filters []memstore.MetadataFilter) {
	for _, mf := range filters {
		if !validMetadataKey(mf.Key) || !validMetadataOps[mf.Op] {
			continue // silently skip invalid filters to match SQLite behavior
		}
		extract := fmt.Sprintf("%smetadata->>'%s'", alias, mf.Key)
		if mf.IncludeNull {
			b.args = append(b.args, mf.Value)
			b.q += fmt.Sprintf(` AND (%s IS NULL OR %s %s $%d)`, extract, extract, mf.Op, len(b.args))
		} else {
			b.args = append(b.args, mf.Value)
			b.q += fmt.Sprintf(` AND %s %s $%d`, extract, mf.Op, len(b.args))
		}
	}
}

func appendTemporalFilters(b *queryBuilder, alias string, after, before *time.Time) {
	if after != nil {
		b.write(fmt.Sprintf(` AND %screated_at >= `, alias), after.UTC())
	}
	if before != nil {
		b.write(fmt.Sprintf(` AND %screated_at <= `, alias), before.UTC())
	}
}

// nullableJSON converts a json.RawMessage to a []byte suitable for JSONB, or nil.
func nullableJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// nullableBytes returns nil if b is empty, otherwise returns b.
func nullableBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// quoteFTSQuery makes a raw string safe for use in a PostgreSQL tsquery.
// Each word is individually quoted so special characters don't cause parse errors.
func quoteFTSQuery(raw string) string {
	words := strings.Fields(raw)
	if len(words) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		// Escape single quotes for plainto_tsquery safety.
		escaped := strings.ReplaceAll(w, "'", "''")
		quoted = append(quoted, escaped)
	}
	return strings.Join(quoted, " & ")
}
