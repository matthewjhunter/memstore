package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const schemaVersion = 5

// factColumns is the canonical SELECT list for fact queries.
// searchFTS has its own column list because it joins and adds rank.
const factColumns = `id, namespace, content, subject, category, metadata, superseded_by, superseded_at, confirmed_count, last_confirmed_at, embedding, created_at`

// SQLiteStore implements Store backed by a caller-provided SQLite database.
// It creates memstore_* tables and uses its own version tracking table so it
// doesn't conflict with any other schema in the same database.
type SQLiteStore struct {
	mu        sync.RWMutex
	db        *sql.DB
	embedder  Embedder // nil means FTS-only; embedding operations will fail
	namespace string   // partition key for multi-tenant isolation
}

// NewSQLiteStore creates a new fact store using the given database connection.
// It creates memstore_* tables if needed and runs any pending migrations.
// The caller is responsible for opening and configuring the database
// (WAL mode, busy timeout, connection limits, etc.).
//
// The namespace parameter partitions facts for multi-tenant isolation. All
// reads and writes are scoped to this namespace. Use SearchOpts.Namespaces
// to search across partitions. Pass "" for single-tenant usage.
//
// If embedder is non-nil, the store records its Model() on first embedding
// operation and validates that subsequent opens use the same model. Pass nil
// only for write-only or administrative access (Search requires an embedder).
func NewSQLiteStore(db *sql.DB, embedder Embedder, namespace string) (*SQLiteStore, error) {
	s := &SQLiteStore{db: db, embedder: embedder, namespace: namespace}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("memstore: migration: %w", err)
	}
	if embedder != nil {
		if err := s.validateEmbedder(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	// Create version tracking table (separate from PRAGMA user_version
	// so we don't conflict with the caller's schema versioning).
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS memstore_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("creating version table: %w", err)
	}

	var version int
	err := s.db.QueryRow("SELECT version FROM memstore_version").Scan(&version)
	if err == sql.ErrNoRows {
		version = 0
	} else if err != nil {
		return fmt.Errorf("reading version: %w", err)
	}

	if version >= schemaVersion {
		return nil
	}

	if version < 1 {
		if err := s.migrateV1(); err != nil {
			return err
		}
	}

	if version < 2 {
		if err := s.migrateV2(); err != nil {
			return err
		}
	}

	if version < 3 {
		if err := s.migrateV3(); err != nil {
			return err
		}
	}

	if version < 4 {
		if err := s.migrateV4(); err != nil {
			return err
		}
	}

	if version < 5 {
		if err := s.migrateV5(); err != nil {
			return err
		}
	}

	if version == 0 {
		_, err = s.db.Exec("INSERT INTO memstore_version (version) VALUES (?)", schemaVersion)
	} else {
		_, err = s.db.Exec("UPDATE memstore_version SET version = ?", schemaVersion)
	}
	return err
}

func (s *SQLiteStore) migrateV5() error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN confirmed_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memstore_facts ADD COLUMN last_confirmed_at TEXT`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V5 migration: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrateV4() error {
	_, err := s.db.Exec(`ALTER TABLE memstore_facts ADD COLUMN superseded_at TEXT`)
	if err != nil {
		return fmt.Errorf("memstore V4 migration: %w", err)
	}
	return nil
}

func (s *SQLiteStore) migrateV3() error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN namespace TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_namespace ON memstore_facts(namespace)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V3 migration: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrateV2() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS memstore_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("creating meta table: %w", err)
	}
	return nil
}

// validateEmbedder checks that the configured embedder's model matches
// the model recorded in the database (if any). Called during NewSQLiteStore.
func (s *SQLiteStore) validateEmbedder() error {
	var stored string
	err := s.db.QueryRow(`SELECT value FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&stored)
	if err == sql.ErrNoRows {
		return nil // no model recorded yet; will be recorded on first embed
	}
	if err != nil {
		return fmt.Errorf("memstore: reading embedding model: %w", err)
	}
	if got := s.embedder.Model(); got != stored {
		return fmt.Errorf("memstore: embedding model mismatch: store has %q, embedder provides %q", stored, got)
	}
	return nil
}

// recordEmbedder writes the embedding model and dimension to the meta table
// if not already recorded. Called on first embedding operation.
func (s *SQLiteStore) recordEmbedder(dim int) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&count); err != nil {
		return fmt.Errorf("memstore: checking meta: %w", err)
	}
	if count > 0 {
		return nil // already recorded
	}
	if _, err := s.db.Exec(`INSERT INTO memstore_meta (key, value) VALUES ('embedding_model', ?)`, s.embedder.Model()); err != nil {
		return fmt.Errorf("memstore: recording embedding model: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO memstore_meta (key, value) VALUES ('embedding_dim', ?)`, fmt.Sprintf("%d", dim)); err != nil {
		return fmt.Errorf("memstore: recording embedding dim: %w", err)
	}
	return nil
}

func (s *SQLiteStore) migrateV1() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memstore_facts (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			content       TEXT NOT NULL,
			subject       TEXT NOT NULL,
			category      TEXT NOT NULL,
			metadata      TEXT,
			superseded_by INTEGER REFERENCES memstore_facts(id),
			embedding     BLOB,
			created_at    TEXT NOT NULL
		)`,

		`CREATE VIRTUAL TABLE IF NOT EXISTS memstore_facts_fts USING fts5(
			content, subject, category,
			content='memstore_facts', content_rowid='id'
		)`,

		// FTS sync triggers (ai/ad/au pattern).
		`CREATE TRIGGER IF NOT EXISTS memstore_facts_ai AFTER INSERT ON memstore_facts BEGIN
			INSERT INTO memstore_facts_fts(rowid, content, subject, category)
			VALUES (new.id, new.content, new.subject, new.category);
		END`,

		`CREATE TRIGGER IF NOT EXISTS memstore_facts_ad AFTER DELETE ON memstore_facts BEGIN
			INSERT INTO memstore_facts_fts(memstore_facts_fts, rowid, content, subject, category)
			VALUES ('delete', old.id, old.content, old.subject, old.category);
		END`,

		`CREATE TRIGGER IF NOT EXISTS memstore_facts_au AFTER UPDATE ON memstore_facts BEGIN
			INSERT INTO memstore_facts_fts(memstore_facts_fts, rowid, content, subject, category)
			VALUES ('delete', old.id, old.content, old.subject, old.category);
			INSERT INTO memstore_facts_fts(rowid, content, subject, category)
			VALUES (new.id, new.content, new.subject, new.category);
		END`,

		`CREATE INDEX IF NOT EXISTS idx_memstore_subject ON memstore_facts(subject)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_category ON memstore_facts(category)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_active ON memstore_facts(id) WHERE superseded_by IS NULL`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore schema: %w", err)
		}
	}

	return nil
}

// Insert adds a single fact and returns its ID. The fact's Namespace field
// is set to the store's namespace regardless of any value provided.
func (s *SQLiteStore) Insert(ctx context.Context, f Fact) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}

	var embBlob []byte
	if len(f.Embedding) > 0 {
		embBlob = EncodeFloat32s(f.Embedding)
	}

	var metadata *string
	if len(f.Metadata) > 0 {
		ms := string(f.Metadata)
		metadata = &ms
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO memstore_facts (namespace, content, subject, category, metadata, superseded_by, embedding, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.namespace, f.Content, f.Subject, f.Category, metadata,
		f.SupersededBy, embBlob, f.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("memstore: inserting fact: %w", err)
	}
	return result.LastInsertId()
}

// InsertBatch inserts multiple facts in a single transaction.
// Each fact's ID field is set on the slice element after insertion.
func (s *SQLiteStore) InsertBatch(ctx context.Context, facts []Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memstore: beginning transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO memstore_facts (namespace, content, subject, category, metadata, superseded_by, embedding, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("memstore: preparing insert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC()
	for i := range facts {
		if facts[i].CreatedAt.IsZero() {
			facts[i].CreatedAt = now
		}

		var embBlob []byte
		if len(facts[i].Embedding) > 0 {
			embBlob = EncodeFloat32s(facts[i].Embedding)
		}

		var metadata *string
		if len(facts[i].Metadata) > 0 {
			ms := string(facts[i].Metadata)
			metadata = &ms
		}

		result, err := stmt.ExecContext(ctx,
			s.namespace, facts[i].Content, facts[i].Subject, facts[i].Category, metadata,
			facts[i].SupersededBy, embBlob, facts[i].CreatedAt.Format(time.RFC3339),
		)
		if err != nil {
			return fmt.Errorf("memstore: inserting fact %q: %w", facts[i].Content, err)
		}

		id, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("memstore: getting insert id: %w", err)
		}
		facts[i].ID = id
	}

	return tx.Commit()
}

// Supersede marks an old fact as superseded by a new fact and records the timestamp.
func (s *SQLiteStore) Supersede(ctx context.Context, oldID, newID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET superseded_by = ?, superseded_at = ? WHERE id = ? AND namespace = ? AND superseded_by IS NULL`,
		newID, now, oldID, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: superseding fact %d: %w", oldID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memstore: checking rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("memstore: fact %d not found or already superseded", oldID)
	}
	return nil
}

// Confirm increments a fact's confirmed_count and updates last_confirmed_at.
func (s *SQLiteStore) Confirm(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET confirmed_count = confirmed_count + 1, last_confirmed_at = ? WHERE id = ? AND namespace = ?`,
		now, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: confirming fact %d: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memstore: checking confirm result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("memstore: fact %d not found", id)
	}
	return nil
}

// Delete removes a fact by ID. Returns an error if the fact doesn't exist
// in this namespace.
func (s *SQLiteStore) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM memstore_facts WHERE id = ? AND namespace = ?`, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: deleting fact %d: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memstore: checking delete result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("memstore: fact %d not found", id)
	}
	return nil
}

// Get retrieves a single fact by ID. Returns nil if not found.
func (s *SQLiteStore) Get(ctx context.Context, id int64) (*Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx,
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = ? AND namespace = ?`, id, s.namespace,
	)
	f, err := scanFact(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memstore: getting fact %d: %w", id, err)
	}
	return f, nil
}

// List returns facts matching the given filters, ordered by ID.
func (s *SQLiteStore) List(ctx context.Context, opts QueryOpts) ([]Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT ` + factColumns + ` FROM memstore_facts WHERE 1=1`
	var args []any
	s.appendNamespaceFilter(&q, &args, "namespace", opts.Namespaces)

	if opts.Subject != "" {
		q += ` AND subject = ?`
		args = append(args, opts.Subject)
	}
	if opts.Category != "" {
		q += ` AND category = ?`
		args = append(args, opts.Category)
	}
	if opts.OnlyActive {
		q += ` AND superseded_by IS NULL`
	}
	if err := appendMetadataFilters(&q, &args, "", opts.MetadataFilters); err != nil {
		return nil, err
	}
	appendTemporalFilters(&q, &args, "", opts.CreatedAfter, opts.CreatedBefore)

	q += ` ORDER BY id`

	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memstore: listing facts: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// BySubject returns facts for a given subject. If onlyActive is true,
// superseded facts are excluded.
func (s *SQLiteStore) BySubject(ctx context.Context, subject string, onlyActive bool) ([]Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT ` + factColumns + `
	      FROM memstore_facts WHERE subject = ? AND namespace = ?`
	args := []any{subject, s.namespace}
	if onlyActive {
		q += ` AND superseded_by IS NULL`
	}
	q += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memstore: querying by subject: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// Exists checks whether a fact with the same content and subject exists.
func (s *SQLiteStore) Exists(ctx context.Context, content, subject string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memstore_facts WHERE content = ? AND subject = ? AND namespace = ?`,
		content, subject, s.namespace,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("memstore: checking existence: %w", err)
	}
	return count > 0, nil
}

// ActiveCount returns the number of non-superseded facts.
func (s *SQLiteStore) ActiveCount(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memstore_facts WHERE superseded_by IS NULL AND namespace = ?`,
		s.namespace,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("memstore: counting active facts: %w", err)
	}
	return count, nil
}

// NeedingEmbedding returns facts that don't have embeddings yet.
func (s *SQLiteStore) NeedingEmbedding(ctx context.Context, limit int) ([]Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factColumns+`
		 FROM memstore_facts WHERE embedding IS NULL AND namespace = ? ORDER BY id LIMIT ?`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memstore: querying unembedded facts: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// SetEmbedding stores a computed embedding for a fact.
func (s *SQLiteStore) SetEmbedding(ctx context.Context, id int64, emb []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET embedding = ? WHERE id = ? AND namespace = ?`,
		EncodeFloat32s(emb), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: setting embedding for fact %d: %w", id, err)
	}
	return nil
}

// EmbedFacts generates embeddings for all facts that don't have one yet,
// processing in batches for efficiency. Uses the store's configured embedder.
func (s *SQLiteStore) EmbedFacts(ctx context.Context, batchSize int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("memstore: no embedder configured")
	}
	if batchSize <= 0 {
		batchSize = 50
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content FROM memstore_facts WHERE embedding IS NULL AND namespace = ? ORDER BY id`,
		s.namespace)
	if err != nil {
		return 0, fmt.Errorf("memstore: querying unembedded facts: %w", err)
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
			return 0, fmt.Errorf("memstore: scanning fact: %w", err)
		}
		pending = append(pending, ic)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("memstore: iterating facts: %w", err)
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

		embeddings, err := embedWithRetry(ctx, s.embedder, texts)
		if err != nil {
			return total, err
		}

		if len(embeddings) != len(batch) {
			return total, fmt.Errorf("memstore: embedding count mismatch: got %d, want %d", len(embeddings), len(batch))
		}

		// Record model+dim on first embedding operation.
		if total == 0 && i == 0 && len(embeddings[0]) > 0 {
			if err := s.recordEmbedder(len(embeddings[0])); err != nil {
				return 0, err
			}
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return total, fmt.Errorf("memstore: beginning tx: %w", err)
		}

		stmt, err := tx.Prepare(`UPDATE memstore_facts SET embedding = ? WHERE id = ?`)
		if err != nil {
			tx.Rollback()
			return total, fmt.Errorf("memstore: preparing update: %w", err)
		}

		for j, emb := range embeddings {
			if _, err := stmt.Exec(EncodeFloat32s(emb), batch[j].id); err != nil {
				stmt.Close()
				tx.Rollback()
				return total, fmt.Errorf("memstore: updating fact %d: %w", batch[j].id, err)
			}
		}

		stmt.Close()
		if err := tx.Commit(); err != nil {
			return total, fmt.Errorf("memstore: committing batch: %w", err)
		}

		total += len(batch)
	}

	return total, nil
}

// validMetadataOps is the set of allowed comparison operators for metadata filters.
var validMetadataOps = map[string]bool{
	"=": true, "!=": true,
	"<": true, "<=": true,
	">": true, ">=": true,
}

// validMetadataKey checks that a metadata key contains only safe characters
// (alphanumeric and underscores) to prevent SQL injection via json path.
func validMetadataKey(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// appendNamespaceFilter appends a namespace WHERE clause to q.
// Namespaces non-empty: AND nsCol IN (?, ?, ...)
// Otherwise:            AND nsCol = ? (store's own namespace)
func (s *SQLiteStore) appendNamespaceFilter(q *string, args *[]any, nsCol string, namespaces []string) {
	if len(namespaces) > 0 {
		*q += ` AND ` + nsCol + ` IN (?` + strings.Repeat(`, ?`, len(namespaces)-1) + `)`
		for _, ns := range namespaces {
			*args = append(*args, ns)
		}
	} else {
		*q += ` AND ` + nsCol + ` = ?`
		*args = append(*args, s.namespace)
	}
}

// appendMetadataFilters adds json_extract-based WHERE clauses and args
// for each MetadataFilter. The table alias (e.g., "f." or "") is prepended
// to the column name. Returns an error for invalid operators or keys.
func appendMetadataFilters(q *string, args *[]any, alias string, filters []MetadataFilter) error {
	for _, mf := range filters {
		if !validMetadataKey(mf.Key) {
			return fmt.Errorf("memstore: invalid metadata filter key: %q", mf.Key)
		}
		if !validMetadataOps[mf.Op] {
			return fmt.Errorf("memstore: invalid metadata filter operator: %q", mf.Op)
		}
		extract := fmt.Sprintf("json_extract(%smetadata, '$.%s')", alias, mf.Key)
		if mf.IncludeNull {
			*q += fmt.Sprintf(` AND (%s IS NULL OR %s %s ?)`, extract, extract, mf.Op)
		} else {
			*q += fmt.Sprintf(` AND %s %s ?`, extract, mf.Op)
		}
		*args = append(*args, mf.Value)
	}
	return nil
}

// appendTemporalFilters adds created_at range conditions to the query.
// The alias (e.g., "f." or "") is prepended to the column name.
func appendTemporalFilters(q *string, args *[]any, alias string, after, before *time.Time) {
	if after != nil {
		*q += fmt.Sprintf(` AND %screated_at >= ?`, alias)
		*args = append(*args, after.UTC().Format(time.RFC3339))
	}
	if before != nil {
		*q += fmt.Sprintf(` AND %screated_at <= ?`, alias)
		*args = append(*args, before.UTC().Format(time.RFC3339))
	}
}

// History returns the supersession chain for a fact. Two modes:
//
// By ID (id > 0): walks backward (predecessors via superseded_by = id) then
// forward (following SupersededBy pointers) to assemble the full chain.
//
// By subject (id == 0, subject non-empty): returns all facts for that subject
// including superseded ones, ordered by created_at then id.
func (s *SQLiteStore) History(ctx context.Context, id int64, subject string) ([]HistoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if id > 0 {
		return s.historyByID(ctx, id)
	}
	if subject != "" {
		return s.historyBySubject(ctx, subject)
	}
	return nil, fmt.Errorf("memstore: History requires either id or subject")
}

// historyByID assembles the full supersession chain containing the given fact.
func (s *SQLiteStore) historyByID(ctx context.Context, id int64) ([]HistoryEntry, error) {
	// Start by fetching the anchor fact.
	row := s.db.QueryRowContext(ctx,
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = ? AND namespace = ?`, id, s.namespace)
	anchor, err := scanFact(row)
	if err != nil {
		return nil, fmt.Errorf("memstore: fact %d not found: %w", id, err)
	}

	// Walk backward: find predecessors (facts whose superseded_by points to us).
	var backward []Fact
	current := anchor.ID
	for {
		row := s.db.QueryRowContext(ctx,
			`SELECT `+factColumns+` FROM memstore_facts WHERE superseded_by = ? AND namespace = ?`,
			current, s.namespace)
		pred, err := scanFact(row)
		if err != nil {
			break // no more predecessors
		}
		backward = append(backward, *pred)
		current = pred.ID
	}

	// Build chain oldest-first: reversed backward + anchor + forward.
	chain := make([]Fact, 0, len(backward)+1)
	for i := len(backward) - 1; i >= 0; i-- {
		chain = append(chain, backward[i])
	}
	chain = append(chain, *anchor)

	// Walk forward: follow SupersededBy pointers.
	current = anchor.ID
	if anchor.SupersededBy != nil {
		next := *anchor.SupersededBy
		for {
			row := s.db.QueryRowContext(ctx,
				`SELECT `+factColumns+` FROM memstore_facts WHERE id = ? AND namespace = ?`,
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
		_ = current // anchor was the starting point
	}

	entries := make([]HistoryEntry, len(chain))
	for i, f := range chain {
		entries[i] = HistoryEntry{Fact: f, Position: i, ChainLength: len(chain)}
	}
	return entries, nil
}

// historyBySubject returns all facts for a subject, including superseded ones.
func (s *SQLiteStore) historyBySubject(ctx context.Context, subject string) ([]HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factColumns+` FROM memstore_facts WHERE subject = ? AND namespace = ? ORDER BY created_at, id`,
		subject, s.namespace)
	if err != nil {
		return nil, fmt.Errorf("memstore: history by subject: %w", err)
	}
	defer rows.Close()

	facts, err := scanFacts(rows)
	if err != nil {
		return nil, err
	}

	entries := make([]HistoryEntry, len(facts))
	for i, f := range facts {
		entries[i] = HistoryEntry{Fact: f, Position: i, ChainLength: len(facts)}
	}
	return entries, nil
}

// Close is a no-op; the caller owns the database connection.
func (s *SQLiteStore) Close() error {
	return nil
}

// scanner abstracts *sql.Row and *sql.Rows for scanFact.
type scanner interface {
	Scan(dest ...any) error
}

func scanFact(row scanner) (*Fact, error) {
	var f Fact
	var metadata sql.NullString
	var supersededBy *int64
	var supersededAt sql.NullString
	var lastConfirmedAt sql.NullString
	var embBlob []byte
	var createdAt string

	err := row.Scan(
		&f.ID, &f.Namespace, &f.Content, &f.Subject, &f.Category,
		&metadata, &supersededBy, &supersededAt,
		&f.ConfirmedCount, &lastConfirmedAt,
		&embBlob, &createdAt,
	)
	if err != nil {
		return nil, err
	}

	if metadata.Valid && metadata.String != "" {
		f.Metadata = json.RawMessage(metadata.String)
	}
	f.SupersededBy = supersededBy
	if supersededAt.Valid {
		t, _ := time.Parse(time.RFC3339, supersededAt.String)
		f.SupersededAt = &t
	}
	if lastConfirmedAt.Valid {
		t, _ := time.Parse(time.RFC3339, lastConfirmedAt.String)
		f.LastConfirmedAt = &t
	}
	if len(embBlob) > 0 {
		f.Embedding = DecodeFloat32s(embBlob)
	}
	f.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)

	return &f, nil
}

func scanFacts(rows *sql.Rows) ([]Fact, error) {
	var facts []Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, fmt.Errorf("memstore: scanning fact: %w", err)
		}
		facts = append(facts, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memstore: iterating facts: %w", err)
	}
	return facts, nil
}
