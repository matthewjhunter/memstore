// Package pgstore implements the memstore.Store interface backed by PostgreSQL
// with pgvector for vector search and tsvector/GIN for full-text search.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/airlock/detect"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	pgvector "github.com/pgvector/pgvector-go"
)

const schemaVersion = 11

// factColumns is the canonical SELECT list for fact queries.
// searchFTS has its own column list because it joins and adds ts_rank.
const factColumns = `id, namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, superseded_at, confirmed_count, last_confirmed_at, use_count, last_used_at, inject_count, last_injected_at, embedding, created_at`

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

// PostgresStore implements memstore.Store backed by PostgreSQL.
// It uses pgvector for vector similarity search and tsvector with GIN
// indexing for full-text search. No mutex is needed -- Postgres handles
// concurrency natively via MVCC.
type PostgresStore struct {
	// embedCeiling is the hard byte bound on a single embed request; see
	// SetEmbedCeiling.
	embedCeiling int

	pool         *pgxpool.Pool
	embedder     embedding.Embedder
	namespace    string
	userID       int64                     // resolved owner for this store; set after migrateV4
	vecDim       int                       // embedding dimension, set at construction or first embed
	queryCache   *embedding.QueryCache     // caches query embeddings on the search path; nil if disabled
	reranker     embedding.Reranker        // nil means no second-stage rerank; set via SetReranker
	screenMode   memstore.ScreenMode       // how the model screen participates in writes
	rejectAt     int                       // detect score at which the inline screen rejects; 0 = default
	detectWrite  memstore.ScreenDetectMode // what the regex screen does to a tripping write
	detectRead   memstore.ScreenDetectMode // what it does to a tripping read
	detectReadAt int                       // read threshold; 0 = DefaultDetectReadScore
}

// SetInlineRejectScore sets the detect score at which the inline regex screen rejects
// a write. Zero restores memstore.InlineRejectScore.
func (s *PostgresStore) SetInlineRejectScore(score int) { s.rejectAt = score }

func (s *PostgresStore) inlineRejectScore() int {
	if s.rejectAt > 0 {
		return s.rejectAt
	}
	return memstore.InlineRejectScore
}

// SetScreenMode selects how the model half of screening participates in writes.
// The read side and the write side must agree, since a
// deployment can move between them.
func (s *PostgresStore) SetScreenMode(m memstore.ScreenMode) { s.screenMode = m }

// SetDetectModes selects what the regex screen does on each edge. Empty values
// restore the block default. See [memstore.ScreenDetectMode] for why the two edges
// are configured separately.
func (s *PostgresStore) SetDetectModes(write, read memstore.ScreenDetectMode) {
	s.detectWrite, s.detectRead = write, read
}

// SetDetectReadScore sets the score at which a read is withheld. Zero restores
// memstore.DefaultDetectReadScore.
func (s *PostgresStore) SetDetectReadScore(n int) { s.detectReadAt = n }

func (s *PostgresStore) detectWriteMode() memstore.ScreenDetectMode {
	if s.detectWrite == "" {
		return memstore.ScreenDetectBlock
	}
	return s.detectWrite
}

func (s *PostgresStore) detectReadMode() memstore.ScreenDetectMode {
	if s.detectRead == "" {
		return memstore.ScreenDetectBlock
	}
	return s.detectRead
}

func (s *PostgresStore) detectReadScore() int {
	if s.detectReadAt > 0 {
		return s.detectReadAt
	}
	return memstore.DefaultDetectReadScore
}

// readableSQL is every unconditional read filter, together: the screening state and
// the regex score. Bound into one call so a new query cannot acquire one without the
// other.
func (s *PostgresStore) readableSQL(prefix string) string {
	out := memstore.ScreenReadableSQL(prefix)
	if s.detectReadMode() == memstore.ScreenDetectBlock {
		out += memstore.DetectReadableSQL(prefix, s.detectReadScore())
	}
	return out
}

// screenInline applies the mandatory write-time screen and returns the state a new
// fact starts in, plus the regex score to record.
func (s *PostgresStore) screenInline(f memstore.Fact) (memstore.ScreenState, int, error) {
	det := detect.Detect(memstore.ScreenableText(f.Content, string(f.Metadata)))
	score := det.Score()

	if score >= s.inlineRejectScore() {
		switch s.detectWriteMode() {
		case memstore.ScreenDetectBlock:
			suffix := ""
			if s.screenMode == memstore.ScreenModeOff {
				suffix = "; no model screen is configured, so the regex screen is authoritative"
			}
			return "", score, fmt.Errorf("%w: detect score %d (%s)%s",
				memstore.ErrScreenRejected, score, strings.Join(memstore.DetectRuleIDs(det), ","), suffix)
		case memstore.ScreenDetectWarn:
			// Rules and score only; stored content must not reach the logs.
			log.Printf("pgstore: detect score %d (%s) admitted by warn mode (subject %q)",
				score, strings.Join(memstore.DetectRuleIDs(det), ","), f.Subject)
		}
	}

	switch s.screenMode {
	case memstore.ScreenModeGate:
		return memstore.ScreenPending, score, nil
	case memstore.ScreenModeObserve:
		return memstore.ScreenScreening, score, nil
	default:
		return memstore.ScreenRegexClean, score, nil
	}
}

// SetEmbedCeiling sets the hard byte bound on any single embed request,
// normally the configured embedder's effective budget
// (embedding.Config.Limits().MaxBytes). Zero leaves chunk sizing to the
// retrieval target alone.
func (s *PostgresStore) SetEmbedCeiling(n int) { s.embedCeiling = n }

// SetReranker configures a second-stage cross-encoder reranker for Search.
// Pass a Reranker built with embedding.NewReranker (configured with
// NormalizeScores so its scores arrive on a [0,1] scale). Intended to be called
// once at startup before the store serves queries; nil disables reranking.
func (s *PostgresStore) SetReranker(rr embedding.Reranker) { s.reranker = rr }

// New creates a new PostgresStore using the given connection pool.
// It creates memstore_* tables if needed and runs any pending migrations.
//
// The namespace parameter partitions facts for multi-tenant isolation.
// vecDim is the embedding vector dimension (e.g. 768 for embeddinggemma).
// If vecDim is 0, embedding columns are created without a dimension constraint.
//
// cacheSize bounds the in-process LRU that caches query embeddings on the
// search path; a value <= 0 disables it.
func New(ctx context.Context, pool *pgxpool.Pool, embedder embedding.Embedder, namespace string, vecDim, cacheSize int) (*PostgresStore, error) {
	s := &PostgresStore{
		pool:       pool,
		embedder:   embedder,
		namespace:  namespace,
		vecDim:     vecDim,
		queryCache: embedding.NewQueryCache(cacheSize),
	}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("pgstore: migration: %w", err)
	}
	// Resolve the owning user from memstore_meta['default_user'].
	// migrateV4 must have recorded it; if not, the operator needs to run
	// 'memstore admin tier3-init --default-user <name>' first.
	uid, err := s.resolveUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgstore: resolving user: %w", err)
	}
	s.userID = uid
	if embedder != nil {
		if err := s.validateEmbedder(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ErrNoDefaultUser is returned when the database has a schema but no owner
// recorded in memstore_meta. It is the one startup failure a deployment can
// resolve by naming a user -- `memstore admin tier3-init --default-user`, or
// memstored's --default-user on first start -- so callers match on it.
var ErrNoDefaultUser = errors.New("no default user recorded -- run 'memstore admin tier3-init --default-user <name>' before starting memstored")

// resolveUser reads the default_user from memstore_meta and resolves or
// creates the user row for the store's namespace. A namespace seen for the
// first time gets a fresh row for the default user (a user belongs to
// exactly one namespace, so each namespace carries its own row). It errors
// only when no default_user is recorded at all -- the operator must run
// 'memstore admin tier3-init --default-user <name>' once.
func (s *PostgresStore) resolveUser(ctx context.Context) (int64, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'default_user'`).Scan(&name)
	if err == pgx.ErrNoRows || (err == nil && name == "") {
		return 0, ErrNoDefaultUser
	}
	if err != nil {
		return 0, fmt.Errorf("reading default_user: %w", err)
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO memstore_users (namespace, name)
		 VALUES ($1, $2)
		 ON CONFLICT (namespace, name) DO NOTHING`,
		s.namespace, name,
	); err != nil {
		return 0, fmt.Errorf("creating user %q for namespace %q: %w", name, s.namespace, err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM memstore_users WHERE namespace = $1 AND name = $2`,
		s.namespace, name,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("looking up user %q: %w", name, err)
	}
	return id, nil
}

// PostgresStore supports per-user scoping.
var (
	_ memstore.UserScoper  = (*PostgresStore)(nil)
	_ memstore.StoreScoper = (*PostgresStore)(nil)
)

// ForUser returns a cheap clone of the store scoped to the given user: every
// read and write the clone performs carries the owner predicate for userID.
// The clone shares the pool, embedder, query cache, and reranker with the
// receiver and runs no migrations. userID must be positive.
// ReadableFor and WritableFor implement memstore.StoreScoper, narrowing what
// ForUser already returns. ForUser has always bound the identity into the
// handle; these decide how much authority the handle carries, so the one place
// a writable store can come from is also the one place the decision is made.
func (s *PostgresStore) ReadableFor(p memstore.Principal) (memstore.ReadableStore, error) {
	scoped, err := s.scopedTo(p)
	if err != nil {
		return nil, err
	}
	return memstore.ReadOnly(scoped), nil
}

func (s *PostgresStore) WritableFor(p memstore.Principal) (memstore.WritableStore, error) {
	if !p.Write {
		return nil, fmt.Errorf("pgstore: writable store for user %d: %w", p.UserID, memstore.ErrNotPermitted)
	}
	return s.scopedTo(p)
}

// scopedTo resolves the per-user handle. A zero user id is the legacy
// unscoped path, which ForUser cannot narrow and must not be asked to.
func (s *PostgresStore) scopedTo(p memstore.Principal) (memstore.Store, error) {
	if p.UserID == 0 {
		return s, nil
	}
	return s.ForUser(p.UserID)
}

func (s *PostgresStore) ForUser(userID int64) (memstore.Store, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("pgstore: ForUser: invalid user id %d", userID)
	}
	c := *s
	c.userID = userID
	return &c, nil
}

// ServiceScope returns a clone of the store with NO user predicate: it sees
// and can touch every user's facts and links in the namespace.
//
// This scope is PRIVILEGED. It exists only for daemon-internal workers
// (embedding backfill, curation) that must operate across users; never hand
// it to anything serving an end-user request.
func (s *PostgresStore) ServiceScope() *PostgresStore {
	c := *s
	c.userID = 0
	return &c
}

// EnsureUser resolves or creates the user row for name in namespace and
// returns its id. Idempotent. Intended for admin tooling (user provisioning)
// and tests; it does not touch memstore_meta['default_user'].
func EnsureUser(ctx context.Context, pool *pgxpool.Pool, namespace, name string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("pgstore: EnsureUser: name must not be empty")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memstore_users (namespace, name)
		 VALUES ($1, $2)
		 ON CONFLICT (namespace, name) DO NOTHING`,
		namespace, name,
	); err != nil {
		return 0, fmt.Errorf("pgstore: EnsureUser: creating user %q: %w", name, err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM memstore_users WHERE namespace = $1 AND name = $2`,
		namespace, name,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("pgstore: EnsureUser: looking up user %q: %w", name, err)
	}
	return id, nil
}

// ErrUserNotFound reports that no user of that name exists in any namespace.
// Callers may safely suggest creating one.
var ErrUserNotFound = errors.New("user not found")

// ErrUserWrongNamespace reports that the user exists, but under a different
// namespace than the caller asked for. It is deliberately NOT an
// ErrUserNotFound: creating the user would produce a duplicate in the wrong
// namespace rather than fix anything. The caller passed the wrong --namespace.
var ErrUserWrongNamespace = errors.New("user found in a different namespace")

// LookupUserID returns the id of an existing user in the namespace. Unlike
// EnsureUser it never creates a row -- it returns a not-found error when the
// user does not exist, so callers (e.g. disable-user) cannot accidentally
// create the principal they meant to act on.
//
// When the name exists in some other namespace, the error names those
// namespaces and wraps ErrUserWrongNamespace.
func LookupUserID(ctx context.Context, pool *pgxpool.Pool, namespace, name string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("pgstore: LookupUserID: name must not be empty")
	}
	var id int64
	err := pool.QueryRow(ctx,
		`SELECT id FROM memstore_users WHERE namespace = $1 AND name = $2`,
		namespace, name,
	).Scan(&id)
	if err == pgx.ErrNoRows {
		others, oerr := userNamespaces(ctx, pool, name)
		if oerr == nil && len(others) > 0 {
			return 0, fmt.Errorf("pgstore: user %q not found in namespace %q, but exists in %s -- pass --namespace: %w",
				name, namespace, quoteJoin(others), ErrUserWrongNamespace)
		}
		return 0, fmt.Errorf("pgstore: user %q not found in namespace %q: %w", name, namespace, ErrUserNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("pgstore: LookupUserID %q: %w", name, err)
	}
	return id, nil
}

// userNamespaces returns every namespace that holds a user of this name.
func userNamespaces(ctx context.Context, pool *pgxpool.Pool, name string) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT namespace FROM memstore_users WHERE name = $1 ORDER BY namespace`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// quoteJoin renders namespaces for an error message. Namespaces can be the
// empty string, so they are quoted -- an unquoted one would vanish from the
// message entirely.
func quoteJoin(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

// InitIdentity seeds the identity schema with an operator-supplied default
// user. This is the implementation behind
// 'memstore admin tier3-init --default-user <name>'.
//
// Two cases:
//   - Schema already at V4 (fresh DB whose migration took the no-user path):
//     ensure the user row and the default_user meta key exist. Idempotent.
//   - Schema below V4 (typically because migrateV4's inference failed and the
//     whole transaction rolled back): run the full V4 work -- shared with
//     migrateV4 via migrateV4As so the two paths cannot drift -- with the
//     explicit user, and record schema version 4.
func InitIdentity(ctx context.Context, pool *pgxpool.Pool, namespace, defaultUser string) error {
	if defaultUser == "" {
		return fmt.Errorf("pgstore: InitIdentity: default-user must not be empty")
	}

	var versionTableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'memstore_version')`,
	).Scan(&versionTableExists); err != nil {
		return fmt.Errorf("pgstore: InitIdentity: checking memstore_version: %w", err)
	}
	version := 0
	hasVersionRow := false
	if versionTableExists {
		err := pool.QueryRow(ctx, `SELECT version FROM memstore_version`).Scan(&version)
		switch {
		case err == pgx.ErrNoRows:
			version = 0
		case err != nil:
			return fmt.Errorf("pgstore: InitIdentity: reading schema version: %w", err)
		default:
			hasVersionRow = true
		}
	}

	if version >= 4 {
		// Idempotent path: the V4 schema is in place; only make sure the user
		// row and meta key exist.
		if _, err := pool.Exec(ctx,
			`INSERT INTO memstore_users (namespace, name)
			 VALUES ($1, $2)
			 ON CONFLICT (namespace, name) DO NOTHING`,
			namespace, defaultUser,
		); err != nil {
			return fmt.Errorf("pgstore: InitIdentity: creating user %q: %w", defaultUser, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO memstore_meta (key, value) VALUES ('default_user', $1)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			defaultUser,
		); err != nil {
			return fmt.Errorf("pgstore: InitIdentity: recording default_user: %w", err)
		}
		return nil
	}

	if hasVersionRow && version < 3 {
		return fmt.Errorf("pgstore: InitIdentity: schema is at version %d; open the store once to migrate to V3 first, then re-run tier3-init", version)
	}
	var factsTableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'memstore_facts')`,
	).Scan(&factsTableExists); err != nil {
		return fmt.Errorf("pgstore: InitIdentity: checking memstore_facts: %w", err)
	}
	if !factsTableExists {
		return fmt.Errorf("pgstore: InitIdentity: base schema missing (memstore_facts not found); open the store once to create it, then re-run tier3-init")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: InitIdentity: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := migrateV4As(ctx, tx, namespace, defaultUser); err != nil {
		return fmt.Errorf("pgstore: InitIdentity: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS memstore_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("pgstore: InitIdentity: creating version table: %w", err)
	}
	if hasVersionRow {
		_, err = tx.Exec(ctx, `UPDATE memstore_version SET version = 4`)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO memstore_version (version) VALUES (4)`)
	}
	if err != nil {
		return fmt.Errorf("pgstore: InitIdentity: recording schema version: %w", err)
	}
	return tx.Commit(ctx)
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

	if version < 2 {
		if err := s.migrateV2(ctx); err != nil {
			return err
		}
	}

	if version < 3 {
		if err := s.migrateV3(ctx); err != nil {
			return err
		}
	}

	if version < 4 {
		if err := s.migrateV4(ctx); err != nil {
			return err
		}
	}

	if version < 5 {
		if err := s.migrateV5(ctx); err != nil {
			return err
		}
	}

	if version < 6 {
		if err := s.migrateV6(ctx); err != nil {
			return err
		}
	}

	if version < 7 {
		if err := s.migrateV7(ctx); err != nil {
			return err
		}
	}

	if version < 8 {
		if err := s.migrateV8(ctx); err != nil {
			return err
		}
	}

	if version < 9 {
		if err := s.migrateV9(ctx); err != nil {
			return err
		}
	}

	if version < 10 {
		if err := s.migrateV10(ctx); err != nil {
			return err
		}
	}

	if version < 11 {
		if err := s.migrateV11(ctx); err != nil {
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
	vecType := s.vectorColumnType()

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

// migrateV2 caps Fact.Content length at memstore.MaxContentLength.
// This is enforcement against the embedder's context window: an oversized
// content row would otherwise poison the embed queue with repeated 400s.
func (s *PostgresStore) migrateV2(ctx context.Context) error {
	stmt := fmt.Sprintf(
		`ALTER TABLE memstore_facts ADD CONSTRAINT memstore_facts_content_length CHECK (length(content) <= %d)`,
		memstore.MaxContentLength,
	)
	if _, err := s.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgstore V2 migration: %w", err)
	}
	return nil
}

// migrateV3 adds quarantine columns for the embed queue. A fact whose embed
// fails permanently (see embedding.IsRetryable) is marked here so
// NeedingEmbedding stops handing it back every poll -- without this the queue
// re-attempts a poison fact forever.
func (s *PostgresStore) migrateV3(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx,
		`ALTER TABLE memstore_facts
		   ADD COLUMN IF NOT EXISTS embed_failed_at TIMESTAMPTZ,
		   ADD COLUMN IF NOT EXISTS embed_error TEXT`,
	); err != nil {
		return fmt.Errorf("pgstore V3 migration: %w", err)
	}
	return nil
}

// vectorColumnType is the pgvector column type for this store's configured
// dimension, and is the single place the vecDim==0 rule lives.
//
// An unset dimension yields an unconstrained "vector" rather than "vector(0)",
// which Postgres rejects outright. That is not a corner case: no deployment here
// sets vec-dim, so the live store's columns are all unconstrained, and a migration
// that interpolated the dimension unconditionally failed on the real database while
// passing every test -- the test harness passes a dimension, production does not.
func (s *PostgresStore) vectorColumnType() string {
	if s.vecDim > 0 {
		return fmt.Sprintf("vector(%d)", s.vecDim)
	}
	return "vector"
}

// migrateV9 records each fact's regex detect score, so the read filter can be a
// WHERE clause instead of a scan.
//
// Nullable on purpose: NULL means not yet computed, which is what every existing
// fact is. Backfilling is a separate pass rather than part of the migration, because
// scoring needs the regex engine rather than SQL -- and a migration that withheld the
// whole corpus until it finished would be the very failure grandfathering avoids.
// (Mirrored SQLite migrateV16 while that backend existed.)
func (s *PostgresStore) migrateV9(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS detect_score INTEGER`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_detect_score
			ON memstore_facts (namespace, detect_score) WHERE detect_score IS NOT NULL`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrateV9: %w", err)
		}
	}
	return nil
}

// migrateV10 records recall injections separately from search hits, and seeds
// the new counter from the historical context_injections log.
//
// use_count only ever counted explicit memory_search results, so the
// highest-volume read path -- per-prompt recall injection -- left no trace and
// a fact surfaced in hundreds of prompts read as never used. Kept as its own
// pair rather than folded into use_count because the two are different
// evidence: being sought is stronger than being offered, and #157's prune
// predicate has to tell them apart.
//
// The backfill is guarded on context_injections existing. That table belongs to
// the session store, which migrates independently and is absent on a fresh
// facts-only database; an unguarded reference would make this migration fail
// there. Where it does exist it holds the injections recorded before the
// client-side logging stopped, which is the only history of this signal.
// (Mirrored SQLite migrateV17, which had no backfill because SQLite had no
// session store.)
func (s *PostgresStore) migrateV10(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS inject_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS last_injected_at TIMESTAMPTZ`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrateV10: %w", err)
		}
	}

	// ref_id is text and holds a fact id only when ref_type = 'fact'; the
	// regex guard keeps a non-numeric ref from aborting the cast.
	backfill := `
		UPDATE memstore_facts f
		SET inject_count = agg.n, last_injected_at = agg.last_at
		FROM (
			SELECT ref_id::bigint AS id, count(*) AS n, max(injected_at) AS last_at
			FROM context_injections
			WHERE ref_type = 'fact' AND ref_id ~ '^[0-9]+$'
			GROUP BY 1
		) agg
		WHERE f.id = agg.id`
	if _, err := s.pool.Exec(ctx, `
		DO $$
		BEGIN
			IF to_regclass('public.context_injections') IS NOT NULL THEN
				`+backfill+`;
			END IF;
		END $$`); err != nil {
		return fmt.Errorf("pgstore: migrateV10 backfill: %w", err)
	}
	return nil
}

// migrateV4 introduces first-class user identity (Phase 0 of tier-3 permissions).
// It creates memstore_users, adds user_id to facts and links, backfills, rewrites
// subject for ownership-only usages, and enforces NOT NULL + FK after backfill.
//
// Default user inference (pgstore, multi-user capable):
//   - Parse all non-legacy api_tokens names on the first hyphen.
//   - Unanimous prefix (e.g. all "matthew-*") -> that user is the default.
//   - No non-legacy tokens (only "legacy" or empty table) -> fresh-DB path:
//     schema migrated with no user rows, no error. Operator must call
//     InitIdentity before starting the daemon.
//   - Ambiguous prefixes (multiple distinct users) -> hard error pointing
//     at 'memstore admin tier3-init --default-user <name>'.
//
// migrateV5 introduces asynchronous injection screening.
//
// Facts gain a screening lifecycle (memstore.ScreenState) enforced on every read, plus
// the attempt bookkeeping the background worker needs so one unscreenable fact cannot
// head the queue forever. The findings table records what screening decided, including
// for writes that were blocked and so never became readable facts -- the rows an
// operator most needs, and ones a column on the fact could not carry.
//
// Existing facts are grandfathered rather than defaulted to pending. This is the
// backend the daemon runs, so the corpus here is the live one: defaulting it to pending
// would make every stored memory vanish the moment the migration ran and stay gone
// until the backlog drained. Grandfathered is distinct from clean so "checked" and
// "predates checking" stay tellable apart.
func (s *PostgresStore) migrateV8(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore V8 migration: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	stmts := []string{
		// New rows default to pending: a write is unreadable until screened, even if
		// some insert path forgets to say so.
		`ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS screen_state TEXT NOT NULL DEFAULT 'pending'`,
		`ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS screen_attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS screened_at TIMESTAMPTZ`,

		// Everything already here predates screening.
		`UPDATE memstore_facts SET screen_state = 'grandfathered'`,

		`CREATE INDEX IF NOT EXISTS idx_memstore_screen_state
			ON memstore_facts (namespace, screen_state)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_screen_pending
			ON memstore_facts (namespace, id) WHERE screen_state = 'pending'`,

		`CREATE TABLE IF NOT EXISTS memstore_screen_findings (
			id             BIGSERIAL   PRIMARY KEY,
			namespace      TEXT        NOT NULL,
			fact_id        BIGINT,
			outcome        TEXT        NOT NULL,
			threat         INTEGER     NOT NULL DEFAULT 0,
			category       TEXT        NOT NULL DEFAULT '',
			verified       BOOLEAN     NOT NULL DEFAULT false,
			detect_score   INTEGER     NOT NULL DEFAULT 0,
			detect_rules   TEXT        NOT NULL DEFAULT '',
			obfuscated     BOOLEAN     NOT NULL DEFAULT false,
			model_screened BOOLEAN     NOT NULL DEFAULT false,
			reason         TEXT        NOT NULL DEFAULT '',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_findings_fact
			ON memstore_screen_findings (namespace, fact_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_findings_outcome
			ON memstore_screen_findings (namespace, outcome, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pgstore V8 migration: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) migrateV4(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore V4 migration: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Infer default user from api_tokens (if the table exists).
	var tokensExist bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'api_tokens')`,
	).Scan(&tokensExist); err != nil {
		return fmt.Errorf("pgstore V4 migration: checking api_tokens: %w", err)
	}

	var factsExist bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM memstore_facts LIMIT 1)`,
	).Scan(&factsExist); err != nil {
		return fmt.Errorf("pgstore V4 migration: checking memstore_facts: %w", err)
	}

	defaultUser := ""
	if tokensExist {
		// Collect non-legacy token name prefixes (split on first hyphen).
		rows, err := tx.Query(ctx, `SELECT name FROM api_tokens WHERE name <> 'legacy' AND revoked_at IS NULL`)
		if err != nil {
			return fmt.Errorf("pgstore V4 migration: querying tokens: %w", err)
		}
		prefixes := map[string]struct{}{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("pgstore V4 migration: scanning token: %w", err)
			}
			if idx := strings.IndexByte(name, '-'); idx > 0 {
				prefixes[name[:idx]] = struct{}{}
			}
			// names without a hyphen: skip (operator will handle via tier3-init)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("pgstore V4 migration: iterating tokens: %w", err)
		}

		switch len(prefixes) {
		case 1:
			for p := range prefixes {
				defaultUser = p
			}
		case 0:
			// Only a legacy token or an empty table: fall through to the
			// facts-exist guard below.
		default:
			names := make([]string, 0, len(prefixes))
			for p := range prefixes {
				names = append(names, p)
			}
			return fmt.Errorf("pgstore V4 migration: ambiguous token prefixes %v -- run 'memstore admin tier3-init --default-user <name>' before starting memstored", names)
		}
	}

	// Facts with no inferable owner -- regardless of whether the tokens table
	// exists -- cannot be backfilled silently. Only a truly fresh database
	// (no facts) may migrate without a default user.
	if defaultUser == "" && factsExist {
		return fmt.Errorf("pgstore V4 migration: tier 3 migration cannot infer default user; run 'memstore admin tier3-init --default-user <name>' before starting memstored")
	}

	if err := migrateV4As(ctx, tx, s.namespace, defaultUser); err != nil {
		return fmt.Errorf("pgstore V4 migration: %w", err)
	}

	return tx.Commit(ctx)
}

// migrateV4As performs the V4 identity work with an explicit default user.
// It is shared by migrateV4 (user inferred from token names) and InitIdentity
// (user supplied by the operator) so the two paths cannot drift.
//
// defaultUser may be "" only when the database holds no facts (fresh DB):
// the schema is created with no user rows and nothing is backfilled. The
// NOT NULL and FK constraints are applied either way -- on a fresh DB the
// tables are empty, so the constraints succeed trivially.
func migrateV4As(ctx context.Context, tx pgx.Tx, namespace, defaultUser string) error {
	// 1. Create users table.
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS memstore_users (
			id         BIGSERIAL   PRIMARY KEY,
			namespace  TEXT        NOT NULL,
			name       TEXT        NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (namespace, name)
		)`); err != nil {
		return fmt.Errorf("create memstore_users: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_memstore_users_namespace ON memstore_users (namespace)`); err != nil {
		return fmt.Errorf("create users index: %w", err)
	}

	// 2. Add user_id columns (nullable first for backfill).
	if _, err := tx.Exec(ctx, `ALTER TABLE memstore_facts ADD COLUMN IF NOT EXISTS user_id BIGINT`); err != nil {
		return fmt.Errorf("add facts.user_id: %w", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE memstore_links ADD COLUMN IF NOT EXISTS user_id BIGINT`); err != nil {
		return fmt.Errorf("add links.user_id: %w", err)
	}

	if defaultUser != "" {
		// 3. One user row per distinct namespace present in facts or links
		// (plus the store's own namespace), each backfilled to its own row.
		// This keeps UNIQUE(namespace, name) meaningful: a user belongs to
		// exactly one namespace.
		nsSet := map[string]struct{}{namespace: {}}
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT namespace FROM memstore_facts
			 UNION SELECT DISTINCT namespace FROM memstore_links`)
		if err != nil {
			return fmt.Errorf("listing namespaces: %w", err)
		}
		for rows.Next() {
			var ns string
			if err := rows.Scan(&ns); err != nil {
				rows.Close()
				return fmt.Errorf("scanning namespace: %w", err)
			}
			nsSet[ns] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating namespaces: %w", err)
		}

		for ns := range nsSet {
			if _, err := tx.Exec(ctx,
				`INSERT INTO memstore_users (namespace, name)
				 VALUES ($1, $2)
				 ON CONFLICT (namespace, name) DO NOTHING`,
				ns, defaultUser,
			); err != nil {
				return fmt.Errorf("insert user %q ns %q: %w", defaultUser, ns, err)
			}
			var uid int64
			if err := tx.QueryRow(ctx,
				`SELECT id FROM memstore_users WHERE namespace = $1 AND name = $2`,
				ns, defaultUser,
			).Scan(&uid); err != nil {
				return fmt.Errorf("resolve user %q ns %q: %w", defaultUser, ns, err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE memstore_facts SET user_id = $1 WHERE namespace = $2 AND user_id IS NULL`,
				uid, ns,
			); err != nil {
				return fmt.Errorf("backfill facts ns %q: %w", ns, err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE memstore_links SET user_id = $1 WHERE namespace = $2 AND user_id IS NULL`,
				uid, ns,
			); err != nil {
				return fmt.Errorf("backfill links ns %q: %w", ns, err)
			}
		}

		// 4. Subject rewrite: user_id now carries ownership, so subjects that
		// merely named the owner are freed to '' (empty string -- subject
		// stays NOT NULL). Identity and preference facts keep the name as a
		// genuine topic.
		if _, err := tx.Exec(ctx,
			`UPDATE memstore_facts SET subject = ''
			 WHERE subject = $1 AND category NOT IN ('identity', 'preference')`,
			defaultUser,
		); err != nil {
			return fmt.Errorf("subject rewrite: %w", err)
		}

		// 5. Record default_user for subsequent opens.
		if _, err := tx.Exec(ctx,
			`INSERT INTO memstore_meta (key, value) VALUES ('default_user', $1)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			defaultUser,
		); err != nil {
			return fmt.Errorf("record default_user: %w", err)
		}
	}

	// 6. Enforce NOT NULL + FK now that backfill is complete.
	if _, err := tx.Exec(ctx, `
		ALTER TABLE memstore_facts
			ALTER COLUMN user_id SET NOT NULL,
			ADD CONSTRAINT memstore_facts_user_id_fkey
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT`); err != nil {
		return fmt.Errorf("enforce facts constraints: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		ALTER TABLE memstore_links
			ALTER COLUMN user_id SET NOT NULL,
			ADD CONSTRAINT memstore_links_user_id_fkey
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT`); err != nil {
		return fmt.Errorf("enforce links constraints: %w", err)
	}

	// 7. Indexes for user-scoped queries.
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_memstore_facts_user ON memstore_facts (namespace, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_facts_user_subj ON memstore_facts (namespace, user_id, subject)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_links_user ON memstore_links (namespace, user_id)`,
	} {
		if _, err := tx.Exec(ctx, idx); err != nil {
			return fmt.Errorf("create user index: %w", err)
		}
	}

	return nil
}

// storedFingerprint reads the persisted fingerprint. Absent fields come back as
// their zero value, which Reconcile reads as "not known yet".
func (s *PostgresStore) storedFingerprint(ctx context.Context) (embedding.Fingerprint, error) {
	var fp embedding.Fingerprint
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM memstore_meta
		 WHERE key IN ('embedding_model', 'embedding_dim', 'embedding_recipe')`)
	if err != nil {
		return fp, fmt.Errorf("pgstore: reading embedder fingerprint: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return fp, fmt.Errorf("pgstore: scanning embedder fingerprint: %w", err)
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
func (s *PostgresStore) persistFingerprint(ctx context.Context, fp embedding.Fingerprint) error {
	vals := map[string]string{"embedding_model": fp.Model, "embedding_recipe": fp.Recipe}
	if fp.Dim > 0 {
		vals["embedding_dim"] = fmt.Sprintf("%d", fp.Dim)
	}
	for k, v := range vals {
		if v == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO memstore_meta (key, value) VALUES ($1, $2)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k, v); err != nil {
			return fmt.Errorf("pgstore: recording %s: %w", k, err)
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
// removes the hand-written-migration-per-recipe-change pattern.
func (s *PostgresStore) reconcileEmbedder(ctx context.Context, current embedding.Fingerprint) error {
	stored, err := s.storedFingerprint(ctx)
	if err != nil {
		return err
	}

	merged, err := embedding.Reconcile(stored, current)
	if err != nil {
		if !embedding.RecipeOnly(err) {
			return fmt.Errorf("%w (stored vectors would not compare with the configured model; "+
				"to switch models deliberately, run 'memstore admin reset-embeddings' and start again)", err)
		}
		log.Printf("pgstore: embedding recipe changed (%s -> %s); clearing vectors for re-embedding",
			stored.Recipe, current.Recipe)
		if err := s.clearVectors(ctx); err != nil {
			return err
		}
		// The dimension is re-learned on the next embed; keeping the old one
		// would assert something about vectors that no longer exist.
		merged = current
	}
	return s.persistFingerprint(ctx, merged)
}

// clearVectors drops every stored vector so the backfill repopulates them.
func (s *PostgresStore) clearVectors(ctx context.Context) error {
	for _, q := range []string{
		`DELETE FROM memstore_fact_chunks`,
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
		`DELETE FROM memstore_meta WHERE key = 'embedding_dim'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: clearing vectors: %w", err)
		}
	}
	return nil
}

// validateEmbedder checks, at store-open, everything knowable from
// configuration alone: the model and the recipe. Only the dimension has to
// wait, since it is not known until a vector comes back.
func (s *PostgresStore) validateEmbedder(ctx context.Context) error {
	if s.embedder == nil {
		return nil
	}
	return s.reconcileEmbedder(ctx, embedding.Fingerprint{
		Model:  s.embedder.Model(),
		Recipe: memstore.EmbedRecipe(s.embedder.Model()),
	})
}

// recordEmbedder reconciles everything now knowable -- model, dimension and
// recipe -- on the first embedding operation.
func (s *PostgresStore) recordEmbedder(ctx context.Context, dim int) error {
	return s.reconcileEmbedder(ctx, embedding.Fingerprint{
		Model:  s.embedder.Model(),
		Dim:    dim,
		Recipe: memstore.EmbedRecipe(s.embedder.Model()),
	})
}
func (s *PostgresStore) Insert(ctx context.Context, f memstore.Fact) (int64, error) {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}

	state, detectScore, err := s.screenInline(f)
	if err != nil {
		return 0, err
	}

	var emb *pgvector.Vector
	if len(f.Embedding) > 0 {
		v := pgvector.NewVector(f.Embedding)
		emb = &v
	}

	userID, err := s.ownerFor(f)
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO memstore_facts (namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at, screen_state, detect_score)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 RETURNING id`,
		s.namespace, userID, f.Content, f.Subject, f.Category, f.Kind, f.Subsystem,
		nullableJSON(f.Metadata), f.SupersededBy, emb, f.CreatedAt, string(state), detectScore,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pgstore: inserting fact: %w", err)
	}
	// A caller that supplies a precomputed vector gets a matching chunk row.
	// Vector search reads chunks, so a fact carrying only the marker would be
	// invisible to it -- embedded as far as the queue is concerned, and
	// unfindable in practice.
	if len(f.Embedding) > 0 {
		if err := insertWholeFactChunk(ctx, s.pool, id, f); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// pgExecer is the subset of the pool / transaction API the chunk insert needs,
// so it can run inside a batch transaction or on its own.
type pgExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertWholeFactChunk records a precomputed whole-fact vector as chunk 0
// spanning the entire content. It is the single-chunk case of what
// SetFactChunks writes, for callers that embedded the fact themselves rather
// than going through memstore.EmbedFact.
func insertWholeFactChunk(ctx context.Context, db pgExecer, id int64, f memstore.Fact) error {
	_, err := db.Exec(ctx,
		`INSERT INTO memstore_fact_chunks (fact_id, ordinal, embedding, byte_start, byte_end)
		 VALUES ($1, 0, $2, 0, $3)
		 ON CONFLICT (fact_id, ordinal) DO UPDATE
		   SET embedding = EXCLUDED.embedding,
		       byte_start = EXCLUDED.byte_start,
		       byte_end = EXCLUDED.byte_end`,
		id, pgvector.NewVector(f.Embedding), len(f.Content),
	)
	if err != nil {
		return fmt.Errorf("pgstore: inserting chunk for fact %d: %w", id, err)
	}
	return nil
}

// InsertBatch inserts multiple facts in a single transaction.
func (s *PostgresStore) InsertBatch(ctx context.Context, facts []memstore.Fact) error {
	// Resolve ownership for every fact before opening the transaction. A
	// rejected owner is a caller bug, not a data condition, and finding it on
	// fact 400 of 500 would mean a pointless round trip and rollback.
	owners := make([]int64, len(facts))
	for i := range facts {
		owner, err := s.ownerFor(facts[i])
		if err != nil {
			return fmt.Errorf("pgstore: fact %d of %d: %w", i+1, len(facts), err)
		}
		owners[i] = owner
	}

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

		state, detectScore, err := s.screenInline(facts[i])
		if err != nil {
			return err
		}

		var emb *pgvector.Vector
		if len(facts[i].Embedding) > 0 {
			v := pgvector.NewVector(facts[i].Embedding)
			emb = &v
		}

		userID := owners[i]

		err = tx.QueryRow(ctx,
			`INSERT INTO memstore_facts (namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at, screen_state, detect_score)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			 RETURNING id`,
			s.namespace, userID, facts[i].Content, facts[i].Subject, facts[i].Category, facts[i].Kind, facts[i].Subsystem,
			nullableJSON(facts[i].Metadata), facts[i].SupersededBy, emb, facts[i].CreatedAt, string(state), detectScore,
		).Scan(&facts[i].ID)
		if err != nil {
			return fmt.Errorf("pgstore: inserting fact %q: %w", facts[i].Content, err)
		}

		if len(facts[i].Embedding) > 0 {
			if err := insertWholeFactChunk(ctx, tx, facts[i].ID, facts[i]); err != nil {
				return err
			}
		}
	}

	return tx.Commit(ctx)
}

// Supersede marks an old fact as superseded by a new fact.
func (s *PostgresStore) Supersede(ctx context.Context, oldID, newID int64) error {
	now := time.Now().UTC()
	q := `UPDATE memstore_facts SET superseded_by = $1, superseded_at = $2
		 WHERE id = $3 AND namespace = $4 AND superseded_by IS NULL`
	args := []any{newID, now, oldID, s.namespace}
	if s.userID != 0 {
		// Both ends of the supersession must belong to the store's user. A
		// foreign or missing newID fails exactly like a missing oldID (0 rows),
		// so existence of other users' facts does not leak.
		args = append(args, s.userID)
		q += ` AND user_id = $5 AND EXISTS (
			SELECT 1 FROM memstore_facts WHERE id = $1 AND namespace = $4 AND user_id = $5)`
	}
	ct, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: superseding fact %d: %w", oldID, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: fact %d %w or already superseded", oldID, memstore.ErrNotFound)
	}
	return nil
}

// Confirm increments a fact's confirmed_count and updates last_confirmed_at.
func (s *PostgresStore) Confirm(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	q, args := s.userPredicate(
		`UPDATE memstore_facts SET confirmed_count = confirmed_count + 1, last_confirmed_at = $1
		 WHERE id = $2 AND namespace = $3`,
		[]any{now, id, s.namespace})
	ct, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: confirming fact %d: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: fact %d %w", id, memstore.ErrNotFound)
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
	q, args := s.userPredicate(
		`UPDATE memstore_facts SET use_count = use_count + 1, last_used_at = $1
		 WHERE namespace = $2 AND id = ANY($3::bigint[])`,
		[]any{now, s.namespace, ids})
	_, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: touching facts: %w", err)
	}
	return nil
}

// RecordInjection increments inject_count and updates last_injected_at for the
// given fact IDs.
//
// Separate from Touch because recall injection and an explicit search are
// different evidence: a fact the model went looking for is a stronger signal
// than one the daemon offered unprompted.
func (s *PostgresStore) RecordInjection(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	now := time.Now().UTC()
	q, args := s.userPredicate(
		`UPDATE memstore_facts SET inject_count = inject_count + 1, last_injected_at = $1
		 WHERE namespace = $2 AND id = ANY($3::bigint[])`,
		[]any{now, s.namespace, ids})
	_, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: recording injection: %w", err)
	}
	return nil
}

// UpdateMetadata merges a patch into the metadata JSON for a fact.
func (s *PostgresStore) UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error {
	// Read current metadata.
	var raw []byte
	readQ, readArgs := s.userPredicate(
		`SELECT metadata FROM memstore_facts WHERE id = $1 AND namespace = $2`,
		[]any{id, s.namespace})
	err := s.pool.QueryRow(ctx, readQ, readArgs...).Scan(&raw)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("pgstore: fact %d %w", id, memstore.ErrNotFound)
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

	// Patching metadata sends the fact back through screening: metadata is rendered to
	// models alongside content, and this is a second write path that would otherwise
	// let a payload onto an already-cleared fact.
	var content string
	if err := s.pool.QueryRow(ctx,
		`SELECT content FROM memstore_facts WHERE id = $1 AND namespace = $2`, id, s.namespace,
	).Scan(&content); err != nil {
		return fmt.Errorf("pgstore: reading content for fact %d: %w", id, err)
	}
	newState, detectScore, err := s.screenInline(memstore.Fact{Content: content, Metadata: merged})
	if err != nil {
		return err
	}

	updQ, updArgs := s.userPredicate(
		`UPDATE memstore_facts
		 SET metadata = $1, screen_state = '`+string(newState)+`', screen_attempts = 0, screened_at = NULL,
		     detect_score = $2
		 WHERE id = $3 AND namespace = $4`,
		[]any{merged, detectScore, id, s.namespace})
	_, err = s.pool.Exec(ctx, updQ, updArgs...)
	if err != nil {
		return fmt.Errorf("pgstore: updating metadata for fact %d: %w", id, err)
	}
	return nil
}

// Delete removes a fact by ID.
func (s *PostgresStore) Delete(ctx context.Context, id int64) error {
	q, args := s.userPredicate(
		`DELETE FROM memstore_facts WHERE id = $1 AND namespace = $2`,
		[]any{id, s.namespace})
	ct, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: deleting fact %d: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: fact %d %w", id, memstore.ErrNotFound)
	}
	return nil
}

// Get retrieves a single fact by ID. Returns nil if not found.
func (s *PostgresStore) Get(ctx context.Context, id int64) (*memstore.Fact, error) {
	q, args := s.userPredicate(
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`+s.readableSQL(""),
		[]any{id, s.namespace})
	row := s.pool.QueryRow(ctx, q, args...)
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
	b.write(`SELECT ` + factColumns + ` FROM memstore_facts WHERE 1=1` + s.readableSQL(""))
	s.appendNamespaceFilter(&b, "namespace", false, opts.Namespaces)
	s.appendUserFilter(&b, "user_id")

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
	if err := appendMetadataFilters(&b, "", opts.MetadataFilters); err != nil {
		return nil, err
	}
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
	b.q += s.readableSQL("")
	b.write(` AND namespace = `, s.namespace)
	s.appendUserFilter(&b, "user_id")
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
	q, args := s.userPredicate(
		`SELECT COUNT(*) FROM memstore_facts WHERE content = $1 AND subject = $2 AND namespace = $3`+memstore.ScreenNotRejectedSQL(""),
		[]any{content, subject, s.namespace})
	err := s.pool.QueryRow(ctx, q, args...).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("pgstore: checking existence: %w", err)
	}
	return count > 0, nil
}

// ActiveCount returns the number of non-superseded facts.
func (s *PostgresStore) ActiveCount(ctx context.Context) (int64, error) {
	var count int64
	q, args := s.userPredicate(
		`SELECT COUNT(*) FROM memstore_facts WHERE superseded_by IS NULL AND namespace = $1`+s.readableSQL(""),
		[]any{s.namespace})
	err := s.pool.QueryRow(ctx, q, args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("pgstore: counting active facts: %w", err)
	}
	return count, nil
}

// Breakdown aggregates the active-fact histograms in SQL. See the Store
// interface for why this is not a List plus a loop in Go.
func (s *PostgresStore) Breakdown(ctx context.Context) (memstore.FactBreakdown, error) {
	bd := memstore.FactBreakdown{
		Subjects:   make(map[string]int),
		Categories: make(map[string]int),
		Kinds:      make(map[string]int),
	}

	// The empty kind is dropped in SQL rather than after scanning, so an
	// unclassified fact never becomes a "" bucket in the result.
	for _, dim := range []struct {
		column string
		extra  string
		into   map[string]int
	}{
		{"subject", "", bd.Subjects},
		{"category", "", bd.Categories},
		{"kind", ` AND kind != ''`, bd.Kinds},
	} {
		q, args := s.userPredicate(
			`SELECT `+dim.column+`, COUNT(*) FROM memstore_facts
			 WHERE superseded_by IS NULL AND namespace = $1`+dim.extra+s.readableSQL(""),
			[]any{s.namespace})
		q += ` GROUP BY ` + dim.column
		if err := s.scanCounts(ctx, q, args, dim.into); err != nil {
			return memstore.FactBreakdown{}, fmt.Errorf("pgstore: breakdown by %s: %w", dim.column, err)
		}
	}
	return bd, nil
}

// scanCounts runs a two-column (value, count) aggregate into dst.
func (s *PostgresStore) scanCounts(ctx context.Context, q string, args []any, dst map[string]int) error {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		dst[key] = n
	}
	return rows.Err()
}

// NeedingEmbedding returns facts that don't have embeddings yet.
func (s *PostgresStore) NeedingEmbedding(ctx context.Context, limit int) ([]memstore.Fact, error) {
	if limit <= 0 {
		limit = 100
	}

	q, args := s.userPredicate(
		`SELECT `+factColumns+`
		 FROM memstore_facts
		 WHERE embedding IS NULL AND embed_failed_at IS NULL AND namespace = $1`+memstore.ScreenNotRejectedSQL(""),
		[]any{s.namespace})
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: querying unembedded facts: %w", err)
	}
	defer rows.Close()

	return scanFacts(rows)
}

// MarkEmbedFailed quarantines a fact whose embedding failed permanently, so
// NeedingEmbedding no longer returns it. reason is stored for diagnostics.
// Clearing the embedding (e.g. on a content edit) re-queues the fact only if
// the caller also resets embed_failed_at; superseding replaces the fact with a
// fresh row that starts unquarantined.
func (s *PostgresStore) MarkEmbedFailed(ctx context.Context, id int64, reason string) error {
	q, args := s.userPredicate(
		`UPDATE memstore_facts
		 SET embed_failed_at = now(), embed_error = $1
		 WHERE id = $2 AND namespace = $3`,
		[]any{reason, id, s.namespace})
	_, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: marking embed failed for fact %d: %w", id, err)
	}
	return nil
}

// migrateV6 gives a fact a set of chunk vectors rather than a single point.
//
// A fact was previously one vector over its whole content, clipped to the
// model's byte budget. That is the wrong target for retrieval: a vector has
// fixed capacity, so filling the model's context window averages away the
// specificity retrieval depends on, and the longest facts -- the substantive
// ones -- lost the most. It also silently discarded the tail of anything over
// the budget. Chunking answers both (see memstore.ChunkFact).
//
// Existing vectors are cleared rather than carried forward. They were produced
// from whole-fact text against a different budget, so they sit in a different
// region of the space than anything produced after this; mixing the two would
// quietly degrade ranking rather than loudly fail. Clearing makes every fact
// look unembedded, and the existing backfill (NeedingEmbedding, which keys off
// embedding IS NULL) repopulates them through its normal path.
func (s *PostgresStore) migrateV6(ctx context.Context) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memstore_fact_chunks (
			fact_id    BIGINT NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			ordinal    INTEGER NOT NULL,
			embedding  %s NOT NULL,
			byte_start INTEGER NOT NULL,
			byte_end   INTEGER NOT NULL,
			PRIMARY KEY (fact_id, ordinal)
		)`, s.vectorColumnType()),
		`DELETE FROM memstore_fact_chunks`,
		// Re-queue every fact for the backfill. embed_failed_at is cleared too:
		// a fact quarantined for overrunning the old whole-fact budget is
		// exactly the fact chunking fixes, so it deserves a fresh attempt.
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrateV6: %w", err)
		}
	}
	return nil
}

// migrateV7 clears every vector because the embed recipe changed: stored text
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
// As with migrateV6, clearing makes every fact look unembedded and the existing
// backfill repopulates them.
func (s *PostgresStore) migrateV7(ctx context.Context) error {
	stmts := []string{
		`DELETE FROM memstore_fact_chunks`,
		`UPDATE memstore_facts SET embedding = NULL, embed_failed_at = NULL, embed_error = NULL`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrateV7: %w", err)
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
func (s *PostgresStore) SetFactVectors(ctx context.Context, id int64, v memstore.FactVectors) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: setting chunks for fact %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM memstore_fact_chunks WHERE fact_id = $1`, id); err != nil {
		return fmt.Errorf("pgstore: clearing chunks for fact %d: %w", id, err)
	}

	for _, c := range v.Chunks {
		cv := pgvector.NewVector(c.Vector)
		if _, err := tx.Exec(ctx,
			`INSERT INTO memstore_fact_chunks (fact_id, ordinal, embedding, byte_start, byte_end)
			 VALUES ($1, $2, $3, $4, $5)`,
			id, c.Ordinal, cv, c.ByteStart, c.ByteEnd,
		); err != nil {
			return fmt.Errorf("pgstore: inserting chunk %d for fact %d: %w", c.Ordinal, id, err)
		}
	}

	var marker *pgvector.Vector
	if len(v.Whole) > 0 {
		wv := pgvector.NewVector(v.Whole)
		marker = &wv
	}

	q, args := s.userPredicate(
		`UPDATE memstore_facts SET embedding = $1 WHERE id = $2 AND namespace = $3`,
		[]any{marker, id, s.namespace})
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("pgstore: setting embedding marker for fact %d: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: committing chunks for fact %d: %w", id, err)
	}
	return nil
}

// FactChunks returns a fact's chunk vectors in ordinal order.
func (s *PostgresStore) FactChunks(ctx context.Context, id int64) ([]memstore.FactChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ordinal, embedding, byte_start, byte_end
		 FROM memstore_fact_chunks WHERE fact_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("pgstore: reading chunks for fact %d: %w", id, err)
	}
	defer rows.Close()

	var out []memstore.FactChunk
	for rows.Next() {
		var c memstore.FactChunk
		var v pgvector.Vector
		if err := rows.Scan(&c.Ordinal, &v, &c.ByteStart, &c.ByteEnd); err != nil {
			return nil, fmt.Errorf("pgstore: scanning chunk for fact %d: %w", id, err)
		}
		c.Vector = v.Slice()
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetEmbedding stores a computed embedding for a fact.
func (s *PostgresStore) SetEmbedding(ctx context.Context, id int64, emb []float32) error {
	v := pgvector.NewVector(emb)
	q, args := s.userPredicate(
		`UPDATE memstore_facts SET embedding = $1 WHERE id = $2 AND namespace = $3`,
		[]any{v, id, s.namespace})
	_, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: setting embedding for fact %d: %w", id, err)
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
func (s *PostgresStore) EmbedFacts(ctx context.Context, batchSize int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("pgstore: EmbedFacts requires an embedder")
	}
	if batchSize <= 0 {
		batchSize = 32
	}

	q, args := s.userPredicate(
		`SELECT id, subject, content FROM memstore_facts
		 WHERE embedding IS NULL AND namespace = $1`+memstore.ScreenNotRejectedSQL(""),
		[]any{s.namespace})
	q += ` ORDER BY id`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("pgstore: querying unembedded facts: %w", err)
	}
	var pending []memstore.Fact
	for rows.Next() {
		var f memstore.Fact
		if err := rows.Scan(&f.ID, &f.Subject, &f.Content); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pgstore: scanning fact: %w", err)
		}
		pending = append(pending, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pgstore: iterating facts: %w", err)
	}

	// Batched across facts: this is bulk work, and one request per fact would
	// pay the per-request overhead thousands of times over an import.
	results := memstore.EmbedFactsBatch(ctx, s.embedder, s.embedder.Model(), pending, s.embedCeiling, batchSize)

	total := 0
	for i, r := range results {
		if r.Err != nil {
			return total, r.Err
		}
		if len(r.Vectors.Chunks) == 0 {
			continue // nothing embeddable in the content
		}
		if err := s.recordEmbedder(ctx, len(r.Vectors.Whole)); err != nil {
			return total, err
		}
		if err := s.SetFactVectors(ctx, pending[i].ID, r.Vectors); err != nil {
			return total, err
		}
		total++
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
	anchorQ, anchorArgs := s.userPredicate(
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`+s.readableSQL(""),
		[]any{id, s.namespace})
	row := s.pool.QueryRow(ctx, anchorQ, anchorArgs...)
	anchor, err := scanFact(row)
	if err != nil {
		// Only a missing anchor is the caller's mistake; a scan or query failure
		// on the same statement is still the store's (#172).
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pgstore: fact %d %w", id, memstore.ErrNotFound)
		}
		return nil, fmt.Errorf("pgstore: reading fact %d: %w", id, err)
	}

	// Walk backward.
	visited := map[int64]bool{anchor.ID: true}
	var backward []memstore.Fact
	current := anchor.ID
	for {
		// The user predicate makes a forged superseded_by pointing into
		// another user's chain terminate like a dangling pointer.
		backQ, backArgs := s.userPredicate(
			`SELECT `+factColumns+` FROM memstore_facts WHERE superseded_by = $1 AND namespace = $2`+s.readableSQL(""),
			[]any{current, s.namespace})
		row := s.pool.QueryRow(ctx, backQ, backArgs...)
		pred, err := scanFact(row)
		if err != nil {
			break
		}
		if visited[pred.ID] {
			break // cycle detected
		}
		visited[pred.ID] = true
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
		// Walk until the chain ends or repeats.
		for !visited[next] {
			fwdQ, fwdArgs := s.userPredicate(
				`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`+s.readableSQL(""),
				[]any{next, s.namespace})
			row := s.pool.QueryRow(ctx, fwdQ, fwdArgs...)
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
	}

	entries := make([]memstore.HistoryEntry, len(chain))
	for i, f := range chain {
		entries[i] = memstore.HistoryEntry{Fact: f, Position: i, ChainLength: len(chain)}
	}
	return entries, nil
}

func (s *PostgresStore) historyBySubject(ctx context.Context, subject string) ([]memstore.HistoryEntry, error) {
	q, args := s.userPredicate(
		`SELECT `+factColumns+` FROM memstore_facts WHERE subject = $1 AND namespace = $2`+s.readableSQL(""),
		[]any{subject, s.namespace})
	q += ` ORDER BY created_at, id`
	rows, err := s.pool.Query(ctx, q, args...)
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
	b.q += s.readableSQL("")
	s.appendUserFilter(&b, "user_id")
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

// TermDocCounts returns the number of documents containing each term and the
// total number of active documents. Uses ts_stat for efficient term frequency lookup.
func (s *PostgresStore) TermDocCounts(ctx context.Context, terms []string) (map[string]int, int, error) {
	if len(terms) == 0 {
		return nil, 0, nil
	}

	// Get total active document count.
	var totalDocs int
	countQ, countArgs := s.userPredicate(
		`SELECT COUNT(*) FROM memstore_facts WHERE namespace = $1 AND superseded_by IS NULL`+s.readableSQL(""),
		[]any{s.namespace})
	err := s.pool.QueryRow(ctx, countQ, countArgs...).Scan(&totalDocs)
	if err != nil {
		return nil, 0, fmt.Errorf("pgstore: counting docs: %w", err)
	}

	// Use ts_stat to get document frequencies for the requested terms.
	// ts_stat takes the inner query as a string literal, so the user
	// predicate is inlined; userID is an int64, not attacker-controlled text.
	statsQuery := fmt.Sprintf(
		`SELECT fts FROM memstore_facts WHERE namespace = %s AND superseded_by IS NULL`+s.readableSQL(""),
		quoteLiteral(s.namespace))
	if s.userID != 0 {
		statsQuery += fmt.Sprintf(` AND user_id = %d`, s.userID)
	}

	// ts_stat reports lexemes, not words: the fts column holds "memstor" for
	// "memstore", so a raw term looked up by name finds nothing. Run each term
	// through the same configuration the column uses and look its lexeme up
	// instead, then answer under the term the caller asked about. A stop word
	// yields no lexeme and is counted in no document.
	lexRows, err := s.pool.Query(ctx,
		`SELECT t, coalesce((SELECT string_agg(lexeme, ' ') FROM unnest(to_tsvector('english', t))), '')
		   FROM unnest($1::text[]) AS t`, terms)
	if err != nil {
		return nil, 0, fmt.Errorf("pgstore: stemming terms: %w", err)
	}
	lexemeOf := make(map[string]string, len(terms))
	lexemes := make([]string, 0, len(terms))
	for lexRows.Next() {
		var term, lexeme string
		if err := lexRows.Scan(&term, &lexeme); err != nil {
			lexRows.Close()
			return nil, 0, fmt.Errorf("pgstore: scanning stemmed term: %w", err)
		}
		lexemeOf[term] = lexeme
		if lexeme != "" {
			lexemes = append(lexemes, lexeme)
		}
	}
	lexRows.Close()
	if err := lexRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("pgstore: stemming terms: %w", err)
	}

	byLexeme := make(map[string]int, len(lexemes))
	if len(lexemes) > 0 {
		rows, err := s.pool.Query(ctx,
			`SELECT word, ndoc FROM ts_stat($1) WHERE word = ANY($2)`,
			statsQuery, lexemes)
		if err != nil {
			return nil, 0, fmt.Errorf("pgstore: querying term frequencies: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var word string
			var ndoc int
			if err := rows.Scan(&word, &ndoc); err != nil {
				return nil, 0, fmt.Errorf("pgstore: scanning term freq: %w", err)
			}
			byLexeme[word] = ndoc
		}
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	}

	counts := make(map[string]int, len(terms))
	for _, term := range terms {
		if lx := lexemeOf[term]; lx != "" {
			counts[term] = byLexeme[lx]
		}
	}
	return counts, totalDocs, nil
}

// quoteLiteral escapes a string for use as a SQL string literal inside ts_stat queries.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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
	var err error
	if s.userID == 0 {
		// Service scope: derive the link's owner from its endpoints. A link
		// belongs to whoever owns the facts it connects, and a link may never
		// span users -- that is an isolation invariant, not a convenience. The
		// GROUP BY yields one row per distinct owner among the endpoints; only
		// a single-owner group can reach the expected DISTINCT-id count, so
		// cross-user pairs insert nothing and fail like a missing fact.
		//
		// The previous branch stamped user_id = 0, which the FK to
		// memstore_users rejects at runtime -- dead code that failed closed,
		// and inconsistent with ownerFor's handling of Insert.
		err = s.pool.QueryRow(ctx,
			`INSERT INTO memstore_links (namespace, user_id, source_id, target_id, link_type, bidirectional, label, metadata, created_at)
			 SELECT $1, o.user_id, $2, $3, $4, $5, $6, $7, $8
			 FROM (SELECT user_id, COUNT(DISTINCT id) AS n
			       FROM memstore_facts
			       WHERE id IN ($2, $3) AND namespace = $1
			       GROUP BY user_id) o
			 WHERE o.n = (CASE WHEN $2 = $3 THEN 1 ELSE 2 END)
			 RETURNING id`,
			s.namespace, sourceID, targetID, linkType, bidirectional, label, nullableBytes(metaJSON), time.Now().UTC(),
		).Scan(&id)
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("pgstore: creating link %d->%d: facts %w or not owned by one user", sourceID, targetID, memstore.ErrNotFound)
		}
	} else {
		// Guarded insert: both endpoints must exist in the store's namespace
		// and belong to the store's user. A foreign fact fails exactly like a
		// missing one (no rows inserted), so existence does not leak.
		// COUNT(DISTINCT id) with the CASE keeps self-links (source == target)
		// behaving as before.
		err = s.pool.QueryRow(ctx,
			`INSERT INTO memstore_links (namespace, user_id, source_id, target_id, link_type, bidirectional, label, metadata, created_at)
			 SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9
			 WHERE (SELECT COUNT(DISTINCT id) FROM memstore_facts
			        WHERE id IN ($3, $4) AND namespace = $1 AND user_id = $2)
			       = (CASE WHEN $3 = $4 THEN 1 ELSE 2 END)
			 RETURNING id`,
			s.namespace, s.userID, sourceID, targetID, linkType, bidirectional, label, nullableBytes(metaJSON), time.Now().UTC(),
		).Scan(&id)
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("pgstore: creating link %d->%d: fact %w", sourceID, targetID, memstore.ErrNotFound)
		}
	}
	if err != nil {
		return 0, fmt.Errorf("pgstore: creating link %d->%d: %w", sourceID, targetID, err)
	}
	return id, nil
}

// GetLink retrieves a single link by ID. Returns (nil, nil) when no link with
// that ID is visible in the caller's scope (absent, or owned by another user),
// matching Get's not-found contract.
func (s *PostgresStore) GetLink(ctx context.Context, linkID int64) (*memstore.Link, error) {
	q, args := s.userPredicate(
		`SELECT `+linkColumns+` FROM memstore_links WHERE id = $1 AND namespace = $2`,
		[]any{linkID, s.namespace})
	row := s.pool.QueryRow(ctx, q, args...)
	l, err := scanLink(row)
	if err == pgx.ErrNoRows {
		return nil, nil
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

	s.appendUserFilter(&b, "user_id")

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
	readQ, readArgs := s.userPredicate(
		`SELECT label, metadata FROM memstore_links WHERE id = $1 AND namespace = $2`,
		[]any{linkID, s.namespace})
	err := s.pool.QueryRow(ctx, readQ, readArgs...).Scan(&currentLabel, &metaRaw)
	if err == pgx.ErrNoRows {
		return fmt.Errorf("pgstore: link %d %w", linkID, memstore.ErrNotFound)
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

	updQ, updArgs := s.userPredicate(
		`UPDATE memstore_links SET label = $1, metadata = $2 WHERE id = $3 AND namespace = $4`,
		[]any{newLabel, nullableBytes(metaJSON), linkID, s.namespace})
	_, err = s.pool.Exec(ctx, updQ, updArgs...)
	if err != nil {
		return fmt.Errorf("pgstore: updating link %d: %w", linkID, err)
	}
	return nil
}

// DeleteLink removes a link by ID.
func (s *PostgresStore) DeleteLink(ctx context.Context, linkID int64) error {
	q, args := s.userPredicate(
		`DELETE FROM memstore_links WHERE id = $1 AND namespace = $2`,
		[]any{linkID, s.namespace})
	ct, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("pgstore: deleting link %d: %w", linkID, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pgstore: link %d %w", linkID, memstore.ErrNotFound)
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
	var lastInjectedAt *time.Time
	var emb *pgvector.Vector

	err := row.Scan(
		&f.ID, &f.Namespace, &f.UserID, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
		&metadata, &supersededBy, &supersededAt,
		&f.ConfirmedCount, &lastConfirmedAt,
		&f.UseCount, &lastUsedAt,
		&f.InjectCount, &lastInjectedAt,
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
	f.LastInjectedAt = lastInjectedAt
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

func (s *PostgresStore) appendNamespaceFilter(b *queryBuilder, nsCol string, allNS bool, namespaces []string) {
	if allNS {
		return
	}
	if len(namespaces) > 0 {
		b.args = append(b.args, namespaces)
		b.q += fmt.Sprintf(` AND %s = ANY($%d::text[])`, nsCol, len(b.args))
	} else {
		b.write(` AND `+nsCol+` = `, s.namespace)
	}
}

// appendUserFilter adds the owner predicate for scoped stores. Service-scope
// stores (userID == 0) carry no user predicate and see all users' rows.
// ownerFor resolves the user_id an incoming fact should be written under. It is
// the only place that decision is made, for Insert and InsertBatch alike.
//
// A scoped store writes its own user's facts and no one else's. Insert used to
// prefer f.UserID whenever it was non-zero, which was harmless only because no
// caller set it and no handler decoded into memstore.Fact -- the field carries a
// json tag, so a future handler that decoded a request body directly into a Fact
// would have handed any authenticated caller a cross-user write with nothing
// failing to signal it. A mismatch is rejected rather than silently corrected,
// because a caller that supplied the wrong owner has a bug worth surfacing.
//
// Service scope (userID == 0) is the privileged daemon-internal scope and may
// write for any user, but must name one: memstore_facts.user_id is NOT NULL with
// a foreign key, so a zero here fails in the database regardless. Failing early
// makes the reason legible.
func (s *PostgresStore) ownerFor(f memstore.Fact) (int64, error) {
	if s.userID != 0 {
		if f.UserID != 0 && f.UserID != s.userID {
			return 0, fmt.Errorf(
				"pgstore: fact carries user_id %d but this store is scoped to user %d: a scoped store cannot write another user's facts",
				f.UserID, s.userID)
		}
		return s.userID, nil
	}
	if f.UserID == 0 {
		return 0, errors.New("pgstore: service-scope insert requires an explicit Fact.UserID")
	}
	return f.UserID, nil
}

func (s *PostgresStore) appendUserFilter(b *queryBuilder, col string) {
	if s.userID == 0 {
		return
	}
	b.write(` AND `+col+` = `, s.userID)
}

// userPredicate appends " AND user_id = $N" to an inline query for scoped
// stores and returns the (possibly extended) query and args. Service-scope
// stores (userID == 0) get the query back unchanged.
func (s *PostgresStore) userPredicate(q string, args []any) (string, []any) {
	if s.userID == 0 {
		return q, args
	}
	args = append(args, s.userID)
	return q + fmt.Sprintf(" AND user_id = $%d", len(args)), args
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

// numericFilterValue reports whether a metadata filter value should use
// numeric comparison. JSON-decoded values arrive as float64; Go callers may
// pass any integer or float type.
func numericFilterValue(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return true
	}
	return false
}

// appendMetadataFilters adds jsonb-based WHERE clauses for each metadata
// filter. Returns an error for invalid keys or operators, matching the
// former SQLite backend's behavior, which the tests still pin.
//
// When the filter value is numeric, the comparison uses a CASE expression
// that casts the JSON value to numeric only when it is actually a JSON number.
// This prevents cast errors on rows whose value for the key is non-numeric,
// and reproduces SQLite's semantics: missing key -> excluded (or included
// with IncludeNull); present non-numeric value -> excluded even with
// IncludeNull (the value is not NULL).
func appendMetadataFilters(b *queryBuilder, alias string, filters []memstore.MetadataFilter) error {
	for _, mf := range filters {
		if !validMetadataKey(mf.Key) {
			return fmt.Errorf("pgstore: invalid metadata filter key: %q", mf.Key)
		}
		if !validMetadataOps[mf.Op] {
			return fmt.Errorf("pgstore: invalid metadata filter operator: %q", mf.Op)
		}

		if numericFilterValue(mf.Value) {
			// Bind the key once; reuse the arg index for both the typeof check
			// and the extract-and-cast expression.
			b.args = append(b.args, mf.Key)
			keyIdx := len(b.args)

			// Convert json.Number to float64 for deterministic pgx encoding.
			val := mf.Value
			if n, ok := val.(json.Number); ok {
				f, err := n.Float64()
				if err != nil {
					return fmt.Errorf("pgstore: cannot convert json.Number filter value: %w", err)
				}
				val = f
			}
			b.args = append(b.args, val)
			valIdx := len(b.args)

			// caseExpr evaluates to NULL when the key is absent or its JSON type
			// is not 'number', so non-numeric rows are silently excluded.
			caseExpr := fmt.Sprintf(
				"CASE WHEN jsonb_typeof(jsonb_extract_path(%smetadata, $%d)) = 'number' THEN (jsonb_extract_path_text(%smetadata, $%d))::numeric END",
				alias, keyIdx, alias, keyIdx,
			)
			if mf.IncludeNull {
				// IncludeNull includes rows where the key is absent entirely.
				// Present-but-non-numeric rows evaluate the caseExpr to NULL
				// and are excluded, matching SQLite's reference behavior.
				nullCheck := fmt.Sprintf("jsonb_extract_path(%smetadata, $%d) IS NULL", alias, keyIdx)
				b.q += fmt.Sprintf(` AND (%s OR %s %s $%d)`, nullCheck, caseExpr, mf.Op, valIdx)
			} else {
				b.q += fmt.Sprintf(` AND %s %s $%d`, caseExpr, mf.Op, valIdx)
			}
		} else {
			b.args = append(b.args, mf.Key)
			extract := fmt.Sprintf("jsonb_extract_path_text(%smetadata, $%d)", alias, len(b.args))
			// The extracted side is text, so the bound side must be too: a Go
			// bool has no text encoding in pgx and would fail to bind, while
			// its JSON spelling ("true"/"false") is exactly what
			// jsonb_extract_path_text yields for a JSON boolean.
			val := mf.Value
			if bv, ok := val.(bool); ok {
				val = strconv.FormatBool(bv)
			}
			b.args = append(b.args, val)
			if mf.IncludeNull {
				b.q += fmt.Sprintf(` AND (%s IS NULL OR %s %s $%d)`, extract, extract, mf.Op, len(b.args))
			} else {
				b.q += fmt.Sprintf(` AND %s %s $%d`, extract, mf.Op, len(b.args))
			}
		}
	}
	return nil
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

// DetectWithheldCount reports how many facts the read filter is currently hiding. A
// blocked read is the silent edge, so the number has to stay answerable.
func (s *PostgresStore) DetectWithheldCount(ctx context.Context) (int, error) {
	if s.detectReadMode() != memstore.ScreenDetectBlock {
		return 0, nil
	}
	q, args := s.userPredicate(
		`SELECT count(*) FROM memstore_facts
		 WHERE namespace = $1 AND detect_score IS NOT NULL AND detect_score >= $2`,
		[]any{s.namespace, s.detectReadScore()})
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("pgstore: counting withheld facts: %w", err)
	}
	return n, nil
}

// DetectScore returns the regex detect score recorded for a fact, or -1 when
// none has been computed yet.
func (s *PostgresStore) DetectScore(ctx context.Context, id int64) (int, error) {
	var score *int
	err := s.pool.QueryRow(ctx,
		`SELECT detect_score FROM memstore_facts WHERE id = $1 AND namespace = $2`,
		id, s.namespace).Scan(&score)
	if err != nil {
		return 0, fmt.Errorf("pgstore: reading detect score for fact %d: %w", id, err)
	}
	if score == nil {
		return -1, nil
	}
	return *score, nil
}

// SetDetectScoreForTest overrides a fact's recorded score. A negative score clears it,
// reproducing a fact written before the column existed.
func (s *PostgresStore) SetDetectScoreForTest(ctx context.Context, id int64, score int) error {
	var v any
	if score >= 0 {
		v = score
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET detect_score = $1 WHERE id = $2 AND namespace = $3`,
		v, id, s.namespace)
	return err
}

// BackfillDetectScores scores facts that have none, so the read filter can act on
// content written before the score was recorded. Returns how many it scored.
//
// Scoring needs the regex engine rather than SQL, which is why this is a pass rather
// than part of the migration -- and why unscored facts read normally until it runs.

func (s *PostgresStore) BackfillDetectScores(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}

	q, args := s.userPredicate(
		`SELECT id, content, COALESCE(metadata::text, '') FROM memstore_facts
		 WHERE detect_score IS NULL AND namespace = $1`,
		[]any{s.namespace})
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("pgstore: querying unscored facts: %w", err)
	}
	type pending struct {
		id       int64
		content  string
		metadata string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.content, &p.metadata); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pgstore: scanning unscored fact: %w", err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pgstore: iterating unscored facts: %w", err)
	}
	if len(todo) == 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, p := range todo {
		score := detect.Detect(memstore.ScreenableText(p.content, p.metadata)).Score()
		batch.Queue(`UPDATE memstore_facts SET detect_score = $1 WHERE id = $2 AND namespace = $3`,
			score, p.id, s.namespace)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range todo {
		if _, err := br.Exec(); err != nil {
			return 0, fmt.Errorf("pgstore: recording detect scores: %w", err)
		}
	}
	return len(todo), nil
}
