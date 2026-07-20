// Package pgstore implements the memstore.Store interface backed by PostgreSQL
// with pgvector for vector search and tsvector/GIN for full-text search.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	pgvector "github.com/pgvector/pgvector-go"
)

const schemaVersion = 5

// factColumns is the canonical SELECT list for fact queries.
// searchFTS has its own column list because it joins and adds ts_rank.
const factColumns = `id, namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, superseded_at, confirmed_count, last_confirmed_at, use_count, last_used_at, embedding, created_at`

// PostgresStore implements memstore.Store backed by PostgreSQL.
// It uses pgvector for vector similarity search and tsvector with GIN
// indexing for full-text search. No mutex is needed -- Postgres handles
// concurrency natively via MVCC.
type PostgresStore struct {
	pool       *pgxpool.Pool
	embedder   embedding.Embedder
	namespace  string
	userID     int64                 // resolved owner for this store; set after migrateV4
	vecDim     int                   // embedding dimension, set at construction or first embed
	queryCache *embedding.QueryCache // caches query embeddings on the search path; nil if disabled
	reranker   embedding.Reranker    // nil means no second-stage rerank; set via SetReranker
}

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
		return 0, fmt.Errorf("no default user recorded -- run 'memstore admin tier3-init --default-user <name>' before starting memstored")
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
var _ memstore.UserScoper = (*PostgresStore)(nil)

// ForUser returns a cheap clone of the store scoped to the given user: every
// read and write the clone performs carries the owner predicate for userID.
// The clone shares the pool, embedder, query cache, and reranker with the
// receiver and runs no migrations. userID must be positive.
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
// NeedingEmbedding stops handing it back every poll — without this the queue
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
		return &embedding.MismatchError{
			Stored:  embedding.Fingerprint{Model: stored},
			Current: embedding.Fingerprint{Model: got},
		}
	}
	return nil
}

func (s *PostgresStore) recordEmbedder(ctx context.Context, dim int) error {
	var storedDim string
	err := s.pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_dim'`).Scan(&storedDim)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("pgstore: checking meta: %w", err)
	}
	if err == nil {
		var existing int
		fmt.Sscanf(storedDim, "%d", &existing)
		if existing != dim {
			return &embedding.MismatchError{
				Stored:  embedding.Fingerprint{Model: s.embedder.Model(), Dim: existing},
				Current: embedding.Fingerprint{Model: s.embedder.Model(), Dim: dim},
			}
		}
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

	userID, err := s.ownerFor(f)
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO memstore_facts (namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING id`,
		s.namespace, userID, f.Content, f.Subject, f.Category, f.Kind, f.Subsystem,
		nullableJSON(f.Metadata), f.SupersededBy, emb, f.CreatedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pgstore: inserting fact: %w", err)
	}
	return id, nil
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

		var emb *pgvector.Vector
		if len(facts[i].Embedding) > 0 {
			v := pgvector.NewVector(facts[i].Embedding)
			emb = &v
		}

		userID := owners[i]

		err := tx.QueryRow(ctx,
			`INSERT INTO memstore_facts (namespace, user_id, content, subject, category, kind, subsystem, metadata, superseded_by, embedding, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			 RETURNING id`,
			s.namespace, userID, facts[i].Content, facts[i].Subject, facts[i].Category, facts[i].Kind, facts[i].Subsystem,
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
		return fmt.Errorf("pgstore: fact %d not found or already superseded", oldID)
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

// UpdateMetadata merges a patch into the metadata JSON for a fact.
func (s *PostgresStore) UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error {
	// Read current metadata.
	var raw []byte
	readQ, readArgs := s.userPredicate(
		`SELECT metadata FROM memstore_facts WHERE id = $1 AND namespace = $2`,
		[]any{id, s.namespace})
	err := s.pool.QueryRow(ctx, readQ, readArgs...).Scan(&raw)
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

	updQ, updArgs := s.userPredicate(
		`UPDATE memstore_facts SET metadata = $1 WHERE id = $2 AND namespace = $3`,
		[]any{merged, id, s.namespace})
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
		return fmt.Errorf("pgstore: fact %d not found", id)
	}
	return nil
}

// Get retrieves a single fact by ID. Returns nil if not found.
func (s *PostgresStore) Get(ctx context.Context, id int64) (*memstore.Fact, error) {
	q, args := s.userPredicate(
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`,
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
	b.write(`SELECT ` + factColumns + ` FROM memstore_facts WHERE 1=1`)
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
		`SELECT COUNT(*) FROM memstore_facts WHERE content = $1 AND subject = $2 AND namespace = $3`,
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
		`SELECT COUNT(*) FROM memstore_facts WHERE superseded_by IS NULL AND namespace = $1`,
		[]any{s.namespace})
	err := s.pool.QueryRow(ctx, q, args...).Scan(&count)
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

	q, args := s.userPredicate(
		`SELECT `+factColumns+`
		 FROM memstore_facts
		 WHERE embedding IS NULL AND embed_failed_at IS NULL AND namespace = $1`,
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

// EmbedFacts generates embeddings for all facts that don't have one yet.
func (s *PostgresStore) EmbedFacts(ctx context.Context, batchSize int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("pgstore: no embedder configured")
	}
	if batchSize <= 0 {
		batchSize = 50
	}

	q, args := s.userPredicate(
		`SELECT id, content FROM memstore_facts WHERE embedding IS NULL AND namespace = $1`,
		[]any{s.namespace})
	q += ` ORDER BY id`
	rows, err := s.pool.Query(ctx, q, args...)
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

		embeddings, err := embedding.EmbedWithRetry(ctx, s.embedder, texts)
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
			// The ids come from the user-scoped select above; the predicate
			// here is defense in depth.
			updQ, updArgs := s.userPredicate(
				`UPDATE memstore_facts SET embedding = $1 WHERE id = $2`,
				[]any{v, batch[j].id})
			if _, err := tx.Exec(ctx, updQ, updArgs...); err != nil {
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
	anchorQ, anchorArgs := s.userPredicate(
		`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`,
		[]any{id, s.namespace})
	row := s.pool.QueryRow(ctx, anchorQ, anchorArgs...)
	anchor, err := scanFact(row)
	if err != nil {
		return nil, fmt.Errorf("pgstore: fact %d not found: %w", id, err)
	}

	// Walk backward.
	visited := map[int64]bool{anchor.ID: true}
	var backward []memstore.Fact
	current := anchor.ID
	for {
		// The user predicate makes a forged superseded_by pointing into
		// another user's chain terminate like a dangling pointer.
		backQ, backArgs := s.userPredicate(
			`SELECT `+factColumns+` FROM memstore_facts WHERE superseded_by = $1 AND namespace = $2`,
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
				`SELECT `+factColumns+` FROM memstore_facts WHERE id = $1 AND namespace = $2`,
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
		`SELECT `+factColumns+` FROM memstore_facts WHERE subject = $1 AND namespace = $2`,
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
		`SELECT COUNT(*) FROM memstore_facts WHERE namespace = $1 AND superseded_by IS NULL`,
		[]any{s.namespace})
	err := s.pool.QueryRow(ctx, countQ, countArgs...).Scan(&totalDocs)
	if err != nil {
		return nil, 0, fmt.Errorf("pgstore: counting docs: %w", err)
	}

	// Use ts_stat to get document frequencies for the requested terms.
	// ts_stat takes the inner query as a string literal, so the user
	// predicate is inlined; userID is an int64, not attacker-controlled text.
	statsQuery := fmt.Sprintf(
		`SELECT fts FROM memstore_facts WHERE namespace = %s AND superseded_by IS NULL`,
		quoteLiteral(s.namespace))
	if s.userID != 0 {
		statsQuery += fmt.Sprintf(` AND user_id = %d`, s.userID)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT word, ndoc FROM ts_stat($1) WHERE word = ANY($2)`,
		statsQuery, terms)
	if err != nil {
		return nil, 0, fmt.Errorf("pgstore: querying term frequencies: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int, len(terms))
	for rows.Next() {
		var word string
		var ndoc int
		if err := rows.Scan(&word, &ndoc); err != nil {
			return nil, 0, fmt.Errorf("pgstore: scanning term freq: %w", err)
		}
		counts[word] = ndoc
	}
	return counts, totalDocs, rows.Err()
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
			return 0, fmt.Errorf("pgstore: creating link %d->%d: facts not found or not owned by one user", sourceID, targetID)
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
			return 0, fmt.Errorf("pgstore: creating link %d->%d: fact not found", sourceID, targetID)
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
		&f.ID, &f.Namespace, &f.UserID, &f.Content, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
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
// SQLite backend's behavior.
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
			b.args = append(b.args, mf.Value)
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
