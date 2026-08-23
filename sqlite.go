package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/user"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/matthewjhunter/go-embedding"
)

const schemaVersion = 14

// factColumns is the canonical SELECT list for fact queries.
// searchFTS has its own column list because it joins and adds rank.
const factColumns = `id, namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, superseded_at, confirmed_count, last_confirmed_at, use_count, last_used_at, embedding, created_at`

// prefixedFactColumns renders factColumns with a table alias applied to each
// column, for queries that join memstore_facts to another table and would
// otherwise leave shared names (id, embedding) ambiguous.
func prefixedFactColumns(prefix string) string {
	cols := strings.Split(factColumns, ", ")
	for i, c := range cols {
		cols[i] = prefix + c
	}
	return strings.Join(cols, ", ")
}

// SQLiteStore implements Store backed by a caller-provided SQLite database.
// It creates memstore_* tables and uses its own version tracking table so it
// doesn't conflict with any other schema in the same database.
type SQLiteStore struct {
	mu sync.RWMutex
	// embedCeiling is the hard byte bound on a single embed request; see
	// SetEmbedCeiling.
	embedCeiling int
	db           *sql.DB
	embedder     embedding.Embedder // nil means FTS-only; embedding operations will fail
	namespace    string             // partition key for multi-tenant isolation
	userID       int64              // resolved owner for this store; set after migrateV12
	reranker     embedding.Reranker // nil means no second-stage rerank; set via SetReranker
}

// SetEmbedCeiling sets the hard byte bound on any single embed request,
// normally the configured embedder's effective budget
// (embedding.Config.Limits().MaxBytes). Zero leaves chunk sizing to the
// retrieval target alone.
func (s *SQLiteStore) SetEmbedCeiling(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedCeiling = n
}

// SetReranker configures a second-stage cross-encoder reranker for Search.
// Pass a Reranker built with embedding.NewReranker (configured with
// NormalizeScores so its scores arrive on a [0,1] scale). Intended to be called
// once at startup before the store serves queries; nil disables reranking.
func (s *SQLiteStore) SetReranker(rr embedding.Reranker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reranker = rr
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
func NewSQLiteStore(db *sql.DB, embedder embedding.Embedder, namespace string) (*SQLiteStore, error) {
	s := &SQLiteStore{db: db, embedder: embedder, namespace: namespace}
	// Enable foreign key enforcement. This is a per-connection setting in SQLite;
	// safe here because callers are expected to use SetMaxOpenConns(1).
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return nil, fmt.Errorf("memstore: enabling foreign keys: %w", err)
	}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("memstore: migration: %w", err)
	}
	// Resolve the store's owning user. migrateV12 created or backfilled the row;
	// for subsequent opens we just look it up from memstore_meta.
	if uid, err := s.resolveOrCreateUser(); err != nil {
		return nil, fmt.Errorf("memstore: resolving user: %w", err)
	} else {
		s.userID = uid
	}
	if embedder != nil {
		if err := s.validateEmbedder(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// resolveOrCreateUser returns the user_id for the store's default user,
// reading the name from memstore_meta key 'default_user'. If no row
// exists in memstore_users yet (fresh DB case), it creates one using
// the current OS user name, lowercased.
func (s *SQLiteStore) resolveOrCreateUser() (int64, error) {
	var name string
	err := s.db.QueryRow(`SELECT value FROM memstore_meta WHERE key = 'default_user'`).Scan(&name)
	if err == sql.ErrNoRows || name == "" {
		// Fresh DB or pre-V12 meta missing: derive from OS user.
		u, oserr := user.Current()
		if oserr != nil {
			name = "default"
		} else {
			name = strings.ToLower(u.Username)
		}
	} else if err != nil {
		return 0, fmt.Errorf("reading default_user meta: %w", err)
	}
	return s.ensureUser(name)
}

// ensureUser creates a user row for (namespace, name) if one doesn't
// exist and returns its id.
func (s *SQLiteStore) ensureUser(name string) (int64, error) {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO memstore_users (namespace, name, created_at) VALUES (?, ?, datetime('now'))`,
		s.namespace, name,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting user %q: %w", name, err)
	}
	var id int64
	if err := s.db.QueryRow(
		`SELECT id FROM memstore_users WHERE namespace = ? AND name = ?`,
		s.namespace, name,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("looking up user %q: %w", name, err)
	}
	return id, nil
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

	if version < 6 {
		if err := s.migrateV6(); err != nil {
			return err
		}
	}

	if version < 7 {
		if err := s.migrateV7(); err != nil {
			return err
		}
	}

	if version < 8 {
		if err := s.migrateV8(); err != nil {
			return err
		}
	}

	if version < 9 {
		if err := s.migrateV9(); err != nil {
			return err
		}
	}

	if version < 10 {
		if err := s.migrateV10(); err != nil {
			return err
		}
	}

	if version < 11 {
		if err := s.migrateV11(); err != nil {
			return err
		}
	}

	if version < 12 {
		if err := s.migrateV12(); err != nil {
			return err
		}
	}

	if version < 13 {
		if err := s.migrateV13(); err != nil {
			return err
		}
	}

	if version < 14 {
		if err := s.migrateV14(); err != nil {
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

// migrateV10 caps Fact.Content length at MaxContentLength via BEFORE triggers.
// SQLite cannot ALTER TABLE to add a CHECK constraint without rebuilding the
// table, so triggers are the simpler equivalent. This guards against oversized
// content poisoning the embed queue with repeated context-length 400s.
func (s *SQLiteStore) migrateV10() error {
	stmts := []string{
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS memstore_facts_content_length_insert
			BEFORE INSERT ON memstore_facts
			WHEN length(NEW.content) > %d
			BEGIN
				SELECT RAISE(ABORT, 'memstore: content exceeds MaxContentLength');
			END`, MaxContentLength),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS memstore_facts_content_length_update
			BEFORE UPDATE OF content ON memstore_facts
			WHEN length(NEW.content) > %d
			BEGIN
				SELECT RAISE(ABORT, 'memstore: content exceeds MaxContentLength');
			END`, MaxContentLength),
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V10 migration: %w", err)
		}
	}
	return nil
}

// migrateV11 adds quarantine columns for the embed queue. A fact whose embed
// fails permanently (see embedding.IsRetryable) is marked here so
// NeedingEmbedding stops handing it back every poll — without this the queue
// re-attempts a poison fact forever. embed_failed_at is unix seconds.
func (s *SQLiteStore) migrateV11() error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN embed_failed_at INTEGER`,
		`ALTER TABLE memstore_facts ADD COLUMN embed_error TEXT`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V11 migration: %w", err)
		}
	}
	return nil
}

// migrateV12 introduces first-class user identity. It creates the
// memstore_users table, adds user_id to facts and links (nullable --
// SQLite cannot add NOT NULL without a table rebuild), backfills every
// existing fact and link to the default OS user, and rewrites subject
// to ” for non-identity/preference facts whose subject was the user's name.
func (s *SQLiteStore) migrateV12() error {
	// Determine default user name from OS.
	defaultUser := "default"
	if u, err := user.Current(); err == nil && u.Username != "" {
		defaultUser = strings.ToLower(u.Username)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memstore_users (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace  TEXT NOT NULL,
			name       TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(namespace, name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_users_namespace ON memstore_users(namespace)`,
		`ALTER TABLE memstore_facts ADD COLUMN user_id INTEGER`,
		`ALTER TABLE memstore_links ADD COLUMN user_id INTEGER`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V12 migration: %w\nstatement: %s", err, stmt)
		}
	}

	// Collect distinct namespaces that already have facts.
	rows, err := s.db.Query(`SELECT DISTINCT namespace FROM memstore_facts`)
	if err != nil {
		return fmt.Errorf("memstore V12 migration: listing namespaces: %w", err)
	}
	var namespaces []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			rows.Close()
			return fmt.Errorf("memstore V12 migration: scanning namespace: %w", err)
		}
		namespaces = append(namespaces, ns)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memstore V12 migration: iterating namespaces: %w", err)
	}

	// Ensure the store's own namespace is included (may have no facts yet).
	found := slices.Contains(namespaces, s.namespace)
	if !found {
		namespaces = append(namespaces, s.namespace)
	}

	// For each namespace: create user row, backfill facts and links.
	for _, ns := range namespaces {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO memstore_users (namespace, name, created_at) VALUES (?, ?, datetime('now'))`,
			ns, defaultUser,
		); err != nil {
			return fmt.Errorf("memstore V12 migration: creating user for ns %q: %w", ns, err)
		}
		var uid int64
		if err := s.db.QueryRow(
			`SELECT id FROM memstore_users WHERE namespace = ? AND name = ?`,
			ns, defaultUser,
		).Scan(&uid); err != nil {
			return fmt.Errorf("memstore V12 migration: resolving user for ns %q: %w", ns, err)
		}
		if _, err := s.db.Exec(
			`UPDATE memstore_facts SET user_id = ? WHERE namespace = ? AND user_id IS NULL`,
			uid, ns,
		); err != nil {
			return fmt.Errorf("memstore V12 migration: backfilling facts ns %q: %w", ns, err)
		}
		if _, err := s.db.Exec(
			`UPDATE memstore_links SET user_id = ? WHERE namespace = ? AND user_id IS NULL`,
			uid, ns,
		); err != nil {
			return fmt.Errorf("memstore V12 migration: backfilling links ns %q: %w", ns, err)
		}
		// Subject rewrite: facts where subject was the user's name but is not
		// an identity or preference fact had subject overloaded as ownership marker.
		// Now that user_id carries ownership, free subject to mean topic only.
		if _, err := s.db.Exec(
			`UPDATE memstore_facts SET subject = '' WHERE namespace = ? AND subject = ? AND category NOT IN ('identity', 'preference')`,
			ns, defaultUser,
		); err != nil {
			return fmt.Errorf("memstore V12 migration: subject rewrite ns %q: %w", ns, err)
		}
	}

	// Record the default_user name for subsequent opens.
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO memstore_meta (key, value) VALUES ('default_user', ?)`,
		defaultUser,
	); err != nil {
		return fmt.Errorf("memstore V12 migration: recording default_user: %w", err)
	}

	return nil
}

func (s *SQLiteStore) migrateV9() error {
	_, err := s.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memstore_facts_fts_vocab
		USING fts5vocab(memstore_facts_fts, row)`)
	if err != nil {
		return fmt.Errorf("memstore V9 migration: %w", err)
	}
	return nil
}

// TermDocCounts returns the number of documents containing each term and the
// total number of active documents. Uses the fts5vocab virtual table for
// efficient term frequency lookup.
func (s *SQLiteStore) TermDocCounts(ctx context.Context, terms []string) (map[string]int, int, error) {
	if len(terms) == 0 {
		return nil, 0, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get total active document count.
	var totalDocs int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memstore_facts WHERE namespace = ? AND superseded_by IS NULL`,
		s.namespace).Scan(&totalDocs)
	if err != nil {
		return nil, 0, fmt.Errorf("memstore: counting docs: %w", err)
	}

	// Query fts5vocab for document frequencies.
	placeholders := make([]string, len(terms))
	args := make([]any, len(terms))
	for i, t := range terms {
		placeholders[i] = "?"
		args[i] = strings.ToLower(t)
	}
	q := fmt.Sprintf(`SELECT term, doc FROM memstore_facts_fts_vocab WHERE term IN (%s)`,
		strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("memstore: querying term frequencies: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int, len(terms))
	for rows.Next() {
		var term string
		var doc int
		if err := rows.Scan(&term, &doc); err != nil {
			return nil, 0, fmt.Errorf("memstore: scanning term freq: %w", err)
		}
		counts[term] = doc
	}
	return counts, totalDocs, rows.Err()
}

func (s *SQLiteStore) migrateV8() error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memstore_facts ADD COLUMN subsystem TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_kind ON memstore_facts(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_subsystem ON memstore_facts(subsystem)`,
		// Migrate existing metadata.kind values to the new column.
		// This handles tasks (kind="task") and any other facts that used the metadata convention.
		`UPDATE memstore_facts SET kind = json_extract(metadata, '$.kind')
		 WHERE kind = ''
		   AND json_extract(metadata, '$.kind') IS NOT NULL
		   AND json_extract(metadata, '$.kind') != ''`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V8 migration: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrateV7() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memstore_links (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace     TEXT NOT NULL DEFAULT '',
			source_id     INTEGER NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			target_id     INTEGER NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			link_type     TEXT NOT NULL DEFAULT 'reference',
			bidirectional INTEGER NOT NULL DEFAULT 0,
			label         TEXT NOT NULL DEFAULT '',
			metadata      TEXT,
			created_at    TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_source ON memstore_links(namespace, source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_target ON memstore_links(namespace, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_type   ON memstore_links(namespace, link_type)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V7 migration: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrateV6() error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN use_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memstore_facts ADD COLUMN last_used_at TEXT`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("memstore V6 migration: %w", err)
		}
	}
	return nil
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

// storedFingerprint reads the persisted fingerprint. Absent fields come back
// as their zero value, which Reconcile reads as "not known yet".
func (s *SQLiteStore) storedFingerprint() (embedding.Fingerprint, error) {
	var fp embedding.Fingerprint
	rows, err := s.db.Query(
		`SELECT key, value FROM memstore_meta
		 WHERE key IN ('embedding_model', 'embedding_dim', 'embedding_recipe')`)
	if err != nil {
		return fp, fmt.Errorf("memstore: reading embedder fingerprint: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return fp, fmt.Errorf("memstore: scanning embedder fingerprint: %w", err)
		}
		switch k {
		case "embedding_model":
			fp.Model = v
		case "embedding_dim":
			fmt.Sscanf(v, "%d", &fp.Dim)
		case "embedding_recipe":
			fp.Recipe = v
		}
	}
	return fp, rows.Err()
}

// persistFingerprint writes the fingerprint back, replacing whatever was there.
func (s *SQLiteStore) persistFingerprint(fp embedding.Fingerprint) error {
	vals := map[string]string{"embedding_model": fp.Model, "embedding_recipe": fp.Recipe}
	if fp.Dim > 0 {
		vals["embedding_dim"] = fmt.Sprintf("%d", fp.Dim)
	}
	for k, v := range vals {
		if v == "" {
			continue
		}
		if _, err := s.db.Exec(
			`INSERT INTO memstore_meta (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return fmt.Errorf("memstore: recording %s: %w", k, err)
		}
	}
	return nil
}

// reconcileEmbedder compares what is currently known against what is stored,
// and persists the result.
//
// A model or dimension change is returned as an error: those mean something
// moved underneath the deployment -- a tag advanced, a backend swapped a model
// -- and clearing vectors automatically would hide it.
//
// A recipe-only change is handled here instead. It follows a deliberate edit,
// the stored vectors are merely in the wrong region rather than incomparable,
// and clearing them lets the existing backfill rebuild. Doing that here is what
// removes the hand-written-migration-per-recipe-change pattern: chunking and
// task prefixes each needed one, written from whoever changed the recipe
// remembering that they had.
func (s *SQLiteStore) reconcileEmbedder(current embedding.Fingerprint) error {
	stored, err := s.storedFingerprint()
	if err != nil {
		return err
	}

	merged, err := embedding.Reconcile(stored, current)
	if err != nil {
		if !embedding.RecipeOnly(err) {
			return err
		}
		log.Printf("memstore: embedding recipe changed (%s -> %s); clearing vectors for re-embedding",
			stored.Recipe, current.Recipe)
		if err := s.clearVectorsLocked(); err != nil {
			return err
		}
		// The dimension is re-learned on the next embed; keeping the old one
		// would assert something about vectors that no longer exist.
		merged = current
	}
	return s.persistFingerprint(merged)
}

// clearVectorsLocked drops every stored vector so the backfill repopulates
// them. The caller holds the write lock.
func (s *SQLiteStore) clearVectorsLocked() error {
	for _, q := range []string{
		`DELETE FROM memstore_fact_chunks`,
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
		`DELETE FROM memstore_meta WHERE key = 'embedding_dim'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("memstore: clearing vectors: %w", err)
		}
	}
	return nil
}

// validateEmbedder checks, at store-open, everything knowable from
// configuration alone: the model and the recipe. Only the dimension has to
// wait, since it is not known until a vector comes back.
func (s *SQLiteStore) validateEmbedder() error {
	if s.embedder == nil {
		return nil
	}
	return s.reconcileEmbedder(embedding.Fingerprint{
		Model:  s.embedder.Model(),
		Recipe: EmbedRecipe(s.embedder.Model()),
	})
}

// recordEmbedder reconciles everything now knowable -- model, dimension and
// recipe -- on the first embedding operation.
func (s *SQLiteStore) recordEmbedder(dim int) error {
	return s.reconcileEmbedder(embedding.Fingerprint{
		Model:  s.embedder.Model(),
		Dim:    dim,
		Recipe: EmbedRecipe(s.embedder.Model()),
	})
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
		embBlob = embedding.EncodeFloat32s(f.Embedding)
	}

	var metadata *string
	if len(f.Metadata) > 0 {
		ms := string(f.Metadata)
		metadata = &ms
	}

	userID := s.userID
	if f.UserID != 0 {
		userID = f.UserID
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO memstore_facts (namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.namespace, userID, f.Content, f.Subject, f.Category, f.Kind, f.Subsystem, metadata,
		f.SupersededBy, embBlob, f.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("memstore: inserting fact: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	// A caller that supplies a precomputed vector gets a matching chunk row.
	// Vector search reads chunks, so a fact carrying only the marker would be
	// invisible to it -- embedded as far as the queue is concerned, and
	// unfindable in practice.
	if len(f.Embedding) > 0 {
		if err := insertWholeFactChunk(ctx, s.db, id, f); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// insertWholeFactChunk records a precomputed whole-fact vector as chunk 0
// spanning the entire content. It is the single-chunk case of what
// SetFactChunks writes, for callers that embedded the fact themselves rather
// than going through EmbedFact.
func insertWholeFactChunk(ctx context.Context, db execer, id int64, f Fact) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO memstore_fact_chunks (fact_id, ordinal, embedding, byte_start, byte_end)
		 VALUES (?, 0, ?, 0, ?)`,
		id, embedding.EncodeFloat32s(f.Embedding), len(f.Content),
	)
	if err != nil {
		return fmt.Errorf("memstore: inserting chunk for fact %d: %w", id, err)
	}
	return nil
}

// execer is the subset of *sql.DB / *sql.Tx the chunk insert needs, so it can
// run inside a batch transaction or on its own.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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
		`INSERT INTO memstore_facts (namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
			embBlob = embedding.EncodeFloat32s(facts[i].Embedding)
		}

		var metadata *string
		if len(facts[i].Metadata) > 0 {
			ms := string(facts[i].Metadata)
			metadata = &ms
		}

		userID := s.userID
		if facts[i].UserID != 0 {
			userID = facts[i].UserID
		}

		result, err := stmt.ExecContext(ctx,
			s.namespace, userID, facts[i].Content, facts[i].Subject, facts[i].Category, facts[i].Kind, facts[i].Subsystem, metadata,
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

		if len(facts[i].Embedding) > 0 {
			if err := insertWholeFactChunk(ctx, tx, id, facts[i]); err != nil {
				return err
			}
		}
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

// Touch increments use_count and updates last_used_at for the given fact IDs.
// Silently ignores IDs that don't exist or belong to other namespaces.
func (s *SQLiteStore) Touch(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	placeholders := "?" + strings.Repeat(", ?", len(ids)-1)
	args := make([]any, 0, len(ids)+2)
	args = append(args, now, s.namespace)
	for _, id := range ids {
		args = append(args, id)
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET use_count = use_count + 1, last_used_at = ?
		 WHERE namespace = ? AND id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("memstore: touching facts: %w", err)
	}
	return nil
}

// UpdateMetadata merges a patch into the metadata JSON for a fact.
// Keys with non-nil values are set; keys with nil values are deleted.
// Returns an error if the fact doesn't exist in this namespace.
// Does not trigger FTS re-index or re-embedding (metadata is not indexed).
func (s *SQLiteStore) UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Read current metadata.
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT metadata FROM memstore_facts WHERE id = ? AND namespace = ?`,
		id, s.namespace,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memstore: fact %d not found", id)
	}
	if err != nil {
		return fmt.Errorf("memstore: reading metadata for fact %d: %w", id, err)
	}

	// Unmarshal existing metadata (empty object if NULL).
	existing := make(map[string]any)
	if raw.Valid && raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &existing); err != nil {
			return fmt.Errorf("memstore: unmarshaling metadata for fact %d: %w", id, err)
		}
	}

	// Apply patch: non-nil values set, nil values delete.
	for k, v := range patch {
		if v == nil {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}

	merged, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("memstore: marshaling metadata for fact %d: %w", id, err)
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET metadata = ? WHERE id = ? AND namespace = ?`,
		string(merged), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: updating metadata for fact %d: %w", id, err)
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
	s.appendNamespaceFilter(&q, &args, "namespace", false, opts.Namespaces)

	if opts.Subject != "" {
		q += ` AND subject = ?`
		args = append(args, opts.Subject)
	}
	if opts.Category != "" {
		q += ` AND category = ?`
		args = append(args, opts.Category)
	}
	if opts.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, opts.Kind)
	}
	if opts.Subsystem != "" {
		q += ` AND subsystem = ?`
		args = append(args, opts.Subsystem)
	}
	if opts.OnlyActive {
		q += ` AND superseded_by IS NULL`
	}
	if len(opts.IDs) > 0 {
		placeholders := make([]string, len(opts.IDs))
		for i, id := range opts.IDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q += ` AND id IN (` + strings.Join(placeholders, ",") + `)`
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
		 FROM memstore_facts
		 WHERE embedding IS NULL AND embed_failed_at IS NULL AND namespace = ?
		 ORDER BY id LIMIT ?`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memstore: querying unembedded facts: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// migrateV13 gives a fact a set of chunk vectors rather than a single point.
//
// A fact was previously one vector over its whole content, clipped to the
// model's byte budget. That is the wrong target for retrieval: a vector has
// fixed capacity, so filling the model's context window averages away the
// specificity retrieval depends on, and the longest facts -- the substantive
// ones -- lost the most. It also silently discarded the tail of anything over
// the budget. Chunking answers both (see ChunkFact).
//
// Existing vectors are cleared rather than carried forward. They were produced
// from whole-fact text against a different budget, so they sit in a different
// region of the space than anything produced after this; mixing the two would
// quietly degrade ranking rather than loudly fail. Clearing makes every fact
// look unembedded, and the existing backfill (NeedingEmbedding, which keys off
// embedding IS NULL) repopulates them through its normal path.
func (s *SQLiteStore) migrateV13() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memstore_fact_chunks (
			fact_id    INTEGER NOT NULL,
			ordinal    INTEGER NOT NULL,
			embedding  BLOB NOT NULL,
			byte_start INTEGER NOT NULL,
			byte_end   INTEGER NOT NULL,
			PRIMARY KEY (fact_id, ordinal),
			FOREIGN KEY (fact_id) REFERENCES memstore_facts(id) ON DELETE CASCADE
		)`,
		`DELETE FROM memstore_fact_chunks`,
		// Re-queue every fact for the backfill. embed_failed_at is cleared too:
		// a fact quarantined for overrunning the old whole-fact budget is
		// exactly the fact chunking fixes, so it deserves a fresh attempt.
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("memstore: migrateV13: %w", err)
		}
	}
	return nil
}

// migrateV14 clears every vector because the embed recipe changed: stored text
// now carries the model's task prefix.
//
// nomic-embed-text is trained to require "search_document:" on stored text and
// "search_query:" on the query. memstore sent neither, so every comparison
// crossed a boundary the model was trained to distinguish. Adding the prefixes
// moves vectors into a different region of the space, and the fact row's
// fingerprint records only model and dim -- not the recipe -- so nothing would
// otherwise notice the change and the store would keep serving old-recipe
// vectors against new-recipe queries. That is worse than either recipe used
// consistently.
//
// As with migrateV13, clearing makes every fact look unembedded and the
// existing backfill repopulates them.
func (s *SQLiteStore) migrateV14() error {
	stmts := []string{
		`DELETE FROM memstore_fact_chunks`,
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("memstore: migrateV14: %w", err)
		}
	}
	return nil
}

// SetFactVectors replaces a fact's vectors in one transaction.
//
// The fact row's embedding column is written in the same transaction, holding
// the whole-fact vector. It serves two purposes: it is the marker the embed
// queue keys off (NeedingEmbedding selects embedding IS NULL), so writing
// chunks without it would leave the fact queued forever; and it is the vector
// deduplication compares facts on, which is why it is the whole fact rather
// than chunk 0. Writing both together is what keeps them from diverging.
func (s *SQLiteStore) SetFactVectors(ctx context.Context, id int64, v FactVectors) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memstore: setting chunks for fact %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM memstore_fact_chunks WHERE fact_id = ?`, id); err != nil {
		return fmt.Errorf("memstore: clearing chunks for fact %d: %w", id, err)
	}

	for _, c := range v.Chunks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memstore_fact_chunks (fact_id, ordinal, embedding, byte_start, byte_end)
			 VALUES (?, ?, ?, ?, ?)`,
			id, c.Ordinal, embedding.EncodeFloat32s(c.Vector), c.ByteStart, c.ByteEnd,
		); err != nil {
			return fmt.Errorf("memstore: inserting chunk %d for fact %d: %w", c.Ordinal, id, err)
		}
	}

	var marker []byte
	if len(v.Whole) > 0 {
		marker = embedding.EncodeFloat32s(v.Whole)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE memstore_facts SET embedding = ? WHERE id = ? AND namespace = ?`,
		marker, id, s.namespace,
	); err != nil {
		return fmt.Errorf("memstore: setting embedding marker for fact %d: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memstore: committing chunks for fact %d: %w", id, err)
	}
	return nil
}

// FactChunks returns a fact's chunk vectors in ordinal order.
func (s *SQLiteStore) FactChunks(ctx context.Context, id int64) ([]FactChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT ordinal, embedding, byte_start, byte_end
		 FROM memstore_fact_chunks WHERE fact_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("memstore: reading chunks for fact %d: %w", id, err)
	}
	defer rows.Close()

	var out []FactChunk
	for rows.Next() {
		var c FactChunk
		var blob []byte
		if err := rows.Scan(&c.Ordinal, &blob, &c.ByteStart, &c.ByteEnd); err != nil {
			return nil, fmt.Errorf("memstore: scanning chunk for fact %d: %w", id, err)
		}
		c.Vector = embedding.DecodeFloat32s(blob)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetEmbedding stores a computed embedding for a fact.
func (s *SQLiteStore) SetEmbedding(ctx context.Context, id int64, emb []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET embedding = ? WHERE id = ? AND namespace = ?`,
		embedding.EncodeFloat32s(emb), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: setting embedding for fact %d: %w", id, err)
	}
	return nil
}

// MarkEmbedFailed quarantines a fact whose embedding failed permanently, so
// NeedingEmbedding no longer returns it. reason is stored for diagnostics.
// Superseding replaces the fact with a fresh row that starts unquarantined.
func (s *SQLiteStore) MarkEmbedFailed(ctx context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET embed_failed_at = ?, embed_error = ? WHERE id = ? AND namespace = ?`,
		time.Now().Unix(), reason, id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: marking embed failed for fact %d: %w", id, err)
	}
	return nil
}

// EmbedFacts generates vectors for every fact that does not have them yet,
// using the store's configured embedder. It is the documented re-embed path
// after an import (see transfer.go), where facts arrive without vectors.
//
// It routes through memstore.EmbedFact so an imported store ends up with
// exactly what the embed queue and the MCP paths produce: chunked, subject on
// every chunk, task-prefixed, and a pooled whole-fact marker. Building its own
// text here is what let this path drift before -- it embedded bare content,
// unprefixed and unchunked, and wrote only the marker, leaving imported facts
// invisible to vector search.
//
// Facts are embedded one at a time rather than batched across facts. A fact's
// own chunks are still batched into a single request inside EmbedFact, and
// per-fact isolation means one unembeddable fact fails alone instead of
// stalling the whole import.
func (s *SQLiteStore) EmbedFacts(ctx context.Context, batchSize int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("memstore: EmbedFacts requires an embedder")
	}
	if batchSize <= 0 {
		batchSize = 32
	}

	// Read phase -- hold only the read lock while querying.
	var pending []Fact
	if err := func() error {
		s.mu.RLock()
		defer s.mu.RUnlock()

		rows, err := s.db.QueryContext(ctx,
			`SELECT id, subject, content FROM memstore_facts
			 WHERE embedding IS NULL AND namespace = ? ORDER BY id`,
			s.namespace)
		if err != nil {
			return fmt.Errorf("memstore: querying unembedded facts: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var f Fact
			if err := rows.Scan(&f.ID, &f.Subject, &f.Content); err != nil {
				return fmt.Errorf("memstore: scanning fact: %w", err)
			}
			pending = append(pending, f)
		}
		return rows.Err()
	}(); err != nil {
		return 0, err
	}

	// Embed phase -- no lock held during network I/O. Batched across facts:
	// this is bulk work, and one request per fact would pay the per-request
	// overhead thousands of times over an import.
	results := EmbedFactsBatch(ctx, s.embedder, s.embedder.Model(), pending, s.embedCeiling, batchSize)

	total := 0
	for i, r := range results {
		if r.Err != nil {
			return total, r.Err
		}
		if len(r.Vectors.Chunks) == 0 {
			continue // nothing embeddable in the content
		}
		if err := func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.recordEmbedder(len(r.Vectors.Whole))
		}(); err != nil {
			return total, err
		}
		if err := s.SetFactVectors(ctx, pending[i].ID, r.Vectors); err != nil {
			return total, err
		}
		total++
	}
	return total, nil
}
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

// appendNamespaceFilter appends a namespace WHERE clause to q.
// allNS true:           no filter (search all namespaces)
// Namespaces non-empty: AND nsCol IN (?, ?, ...)
// Otherwise:            AND nsCol = ? (store's own namespace)
func (s *SQLiteStore) appendNamespaceFilter(q *string, args *[]any, nsCol string, allNS bool, namespaces []string) {
	if allNS {
		return
	}
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

// validMetadataOps is the set of allowed comparison operators for metadata filters.
var validMetadataOps = map[string]bool{
	"=": true, "!=": true,
	"<": true, "<=": true,
	">": true, ">=": true,
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
	visited := map[int64]bool{anchor.ID: true}
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
		if visited[pred.ID] {
			break // cycle detected
		}
		visited[pred.ID] = true
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
		// Walk until the chain ends or repeats.
		for !visited[next] {
			row := s.db.QueryRowContext(ctx,
				`SELECT `+factColumns+` FROM memstore_facts WHERE id = ? AND namespace = ?`,
				next, s.namespace)
			succ, err := scanFact(row)
			if err != nil {
				break
			}
			visited[succ.ID] = true
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

// ListSubsystems returns all distinct non-empty subsystem values,
// optionally filtered by subject (empty = all subjects).
func (s *SQLiteStore) ListSubsystems(ctx context.Context, subject string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT DISTINCT subsystem FROM memstore_facts WHERE namespace = ? AND superseded_by IS NULL AND subsystem != ''`
	args := []any{s.namespace}
	if subject != "" {
		q += ` AND subject = ?`
		args = append(args, subject)
	}
	q += ` ORDER BY subsystem`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memstore: listing subsystems: %w", err)
	}
	defer rows.Close()

	var subsystems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("memstore: scanning subsystem: %w", err)
		}
		subsystems = append(subsystems, s)
	}
	return subsystems, rows.Err()
}

// Close is a no-op; the caller owns the database connection.
func (s *SQLiteStore) Close() error {
	return nil
}

// --- Link methods ---

const linkColumns = `id, namespace, source_id, target_id, link_type, bidirectional, label, metadata, created_at`

func scanLink(r scanner) (*Link, error) {
	var l Link
	var bidi int
	var metaStr sql.NullString
	var createdAt string
	var namespace string

	err := r.Scan(&l.ID, &namespace, &l.SourceID, &l.TargetID, &l.LinkType, &bidi, &l.Label, &metaStr, &createdAt)
	if err != nil {
		return nil, err
	}
	l.Bidirectional = bidi == 1
	if metaStr.Valid && metaStr.String != "" {
		l.Metadata = json.RawMessage(metaStr.String)
	}
	if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
		l.CreatedAt = t
	}
	return &l, nil
}

func scanLinks(rows *sql.Rows) ([]Link, error) {
	var links []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, fmt.Errorf("memstore: scanning link: %w", err)
		}
		links = append(links, *l)
	}
	return links, rows.Err()
}

// LinkFacts creates a directed edge between two facts.
func (s *SQLiteStore) LinkFacts(ctx context.Context, sourceID, targetID int64, linkType string, bidirectional bool, label string, metadata map[string]any) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var metaStr *string
	if len(metadata) > 0 {
		b, err := json.Marshal(metadata)
		if err != nil {
			return 0, fmt.Errorf("memstore: marshaling link metadata: %w", err)
		}
		ms := string(b)
		metaStr = &ms
	}

	bidi := 0
	if bidirectional {
		bidi = 1
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO memstore_links (namespace, user_id, source_id, target_id, link_type, bidirectional, label, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.namespace, s.userID, sourceID, targetID, linkType, bidi, label, metaStr, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("memstore: creating link %d->%d: %w", sourceID, targetID, err)
	}
	return result.LastInsertId()
}

// GetLink retrieves a single link by ID. Returns (nil, nil) when no link with
// that ID exists in this namespace, matching Get's not-found contract.
func (s *SQLiteStore) GetLink(ctx context.Context, linkID int64) (*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx,
		`SELECT `+linkColumns+` FROM memstore_links WHERE id = ? AND namespace = ?`,
		linkID, s.namespace,
	)
	l, err := scanLink(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memstore: getting link %d: %w", linkID, err)
	}
	return l, nil
}

// GetLinks returns edges touching factID filtered by direction and optional link types.
// LinkOutbound: edges where factID is source, plus bidirectional edges where factID is target.
// LinkInbound:  edges where factID is target, plus bidirectional edges where factID is source.
// LinkBoth:     all edges touching factID.
func (s *SQLiteStore) GetLinks(ctx context.Context, factID int64, direction LinkDirection, linkTypes ...string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var q string
	var args []any

	switch direction {
	case LinkOutbound:
		q = `SELECT ` + linkColumns + ` FROM memstore_links WHERE namespace = ? AND (source_id = ? OR (target_id = ? AND bidirectional = 1))`
		args = []any{s.namespace, factID, factID}
	case LinkInbound:
		q = `SELECT ` + linkColumns + ` FROM memstore_links WHERE namespace = ? AND (target_id = ? OR (source_id = ? AND bidirectional = 1))`
		args = []any{s.namespace, factID, factID}
	default: // LinkBoth
		q = `SELECT ` + linkColumns + ` FROM memstore_links WHERE namespace = ? AND (source_id = ? OR target_id = ?)`
		args = []any{s.namespace, factID, factID}
	}

	if len(linkTypes) > 0 {
		placeholders := "?" + strings.Repeat(", ?", len(linkTypes)-1)
		q += ` AND link_type IN (` + placeholders + `)`
		for _, lt := range linkTypes {
			args = append(args, lt)
		}
	}

	q += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memstore: getting links for fact %d: %w", factID, err)
	}
	defer rows.Close()

	return scanLinks(rows)
}

// UpdateLink patches the label and/or metadata of an existing link.
// An empty label leaves the existing label unchanged.
// Metadata keys with nil values are deleted; non-nil values are set.
func (s *SQLiteStore) UpdateLink(ctx context.Context, linkID int64, label string, metadata map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var currentLabel string
	var metaRaw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT label, metadata FROM memstore_links WHERE id = ? AND namespace = ?`,
		linkID, s.namespace,
	).Scan(&currentLabel, &metaRaw)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memstore: link %d not found", linkID)
	}
	if err != nil {
		return fmt.Errorf("memstore: reading link %d: %w", linkID, err)
	}

	newLabel := currentLabel
	if label != "" {
		newLabel = label
	}

	existing := make(map[string]any)
	if metaRaw.Valid && metaRaw.String != "" {
		if err := json.Unmarshal([]byte(metaRaw.String), &existing); err != nil {
			return fmt.Errorf("memstore: unmarshaling link metadata %d: %w", linkID, err)
		}
	}
	for k, v := range metadata {
		if v == nil {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}

	var metaStr *string
	if len(existing) > 0 {
		b, err := json.Marshal(existing)
		if err != nil {
			return fmt.Errorf("memstore: marshaling link metadata %d: %w", linkID, err)
		}
		ms := string(b)
		metaStr = &ms
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE memstore_links SET label = ?, metadata = ? WHERE id = ? AND namespace = ?`,
		newLabel, metaStr, linkID, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: updating link %d: %w", linkID, err)
	}
	return nil
}

// DeleteLink removes a link by ID. Returns an error if not found.
func (s *SQLiteStore) DeleteLink(ctx context.Context, linkID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM memstore_links WHERE id = ? AND namespace = ?`, linkID, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: deleting link %d: %w", linkID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memstore: checking delete result: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("memstore: link %d not found", linkID)
	}
	return nil
}

// scanner abstracts *sql.Row and *sql.Rows for scanFact.
type scanner interface {
	Scan(dest ...any) error
}

func scanFact(row scanner) (*Fact, error) {
	var f Fact
	var userID sql.NullInt64
	var metadata sql.NullString
	var supersededBy *int64
	var supersededAt sql.NullString
	var lastConfirmedAt sql.NullString
	var lastUsedAt sql.NullString
	var embBlob []byte
	var createdAt string

	err := row.Scan(
		&f.ID, &f.Namespace, &userID, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
		&metadata, &supersededBy, &supersededAt,
		&f.ConfirmedCount, &lastConfirmedAt,
		&f.UseCount, &lastUsedAt,
		&embBlob, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	if userID.Valid {
		f.UserID = userID.Int64
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
	if lastUsedAt.Valid {
		t, _ := time.Parse(time.RFC3339, lastUsedAt.String)
		f.LastUsedAt = &t
	}
	if len(embBlob) > 0 {
		f.Embedding = embedding.DecodeFloat32s(embBlob)
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
