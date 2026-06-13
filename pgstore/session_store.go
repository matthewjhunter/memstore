package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
)

// SessionStore persists Claude Code session events to PostgreSQL.
// It is independent of PostgresStore -- it manages its own tables.
//
// Every instance carries a userID resolved at construction from
// memstore_meta['default_user']. ForUser returns a cheap clone scoped
// to a different user; ServiceScope returns a clone with userID 0 that
// omits the user predicate on reads and is intended for cross-user
// maintenance operations only (writes would violate NOT NULL on user_id).
type SessionStore struct {
	pool   *pgxpool.Pool
	userID int64
}

// NewSessionStore creates a SessionStore, runs its migrations, and resolves
// the default user from memstore_meta. The default user must already exist
// (recorded by the facts-layer migration or 'memstore admin tier3-init')
// unless all session tables are empty, in which case userID stays 0 until
// ForUser is called.
func NewSessionStore(ctx context.Context, pool *pgxpool.Pool) (*SessionStore, error) {
	s := &SessionStore{pool: pool}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	uid, err := resolveSessionUser(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("session store: resolving user: %w", err)
	}
	s.userID = uid
	return s, nil
}

// resolveSessionUser reads the default_user from memstore_meta and returns
// the user's id from memstore_users. Returns 0 (no-op predicate) if
// memstore_meta has no default_user entry AND all session tables are empty --
// this handles the case where the session store is initialized before the
// facts layer has seeded identity (e.g. in tests). If there is no default
// user but session data exists, it returns the same tier3-init error the
// facts layer uses.
func resolveSessionUser(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var name string
	err := pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'default_user'`).Scan(&name)
	if err == pgx.ErrNoRows || (err == nil && name == "") {
		// No default user recorded. Safe to proceed as userID=0 only if
		// all session tables are empty.
		var hasData bool
		checkQ := `
			SELECT EXISTS(
				SELECT 1 FROM session_turns    LIMIT 1 UNION ALL
				SELECT 1 FROM session_hooks    LIMIT 1 UNION ALL
				SELECT 1 FROM context_hints    LIMIT 1 UNION ALL
				SELECT 1 FROM context_injections LIMIT 1 UNION ALL
				SELECT 1 FROM context_feedback LIMIT 1
			)`
		if qerr := pool.QueryRow(ctx, checkQ).Scan(&hasData); qerr != nil {
			// Tables may not yet exist (first migration call); treat as empty.
			return 0, nil
		}
		if hasData {
			return 0, fmt.Errorf("no default user recorded -- run 'memstore admin tier3-init --default-user <name>' before starting memstored")
		}
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading default_user: %w", err)
	}

	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM memstore_users WHERE name = $1 LIMIT 1`,
		name,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("looking up user %q: %w", name, err)
	}
	return id, nil
}

// UserID returns the user ID this store is scoped to. 0 means service scope
// (no user predicate). Exposed so tests and admin tooling can pass the
// resolved default-user ID to ForUser.
func (s *SessionStore) UserID() int64 { return s.userID }

// SessionStore supports per-user scoping.
var _ memstore.SessionUserScoper = (*SessionStore)(nil)

// ForUser returns a cheap clone of the session store scoped to the given
// user. Every read and write the clone performs carries the owner predicate
// for userID. userID must be positive.
func (s *SessionStore) ForUser(userID int64) (memstore.SessionStore, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("pgstore: SessionStore.ForUser: invalid user id %d", userID)
	}
	c := *s
	c.userID = userID
	return &c, nil
}

// ServiceScope returns a clone of the session store with NO user predicate.
// Reads span all users; writes are invalid (user_id NOT NULL would fire).
// Intended for cross-user maintenance (BackfillFeedback, UnratedFactSessions).
func (s *SessionStore) ServiceScope() *SessionStore {
	c := *s
	c.userID = 0
	return &c
}

// userClause returns an SQL fragment and appended args for the user predicate.
// When userID is 0 (service scope) it returns an empty string and the args
// unchanged so callers can unconditionally append the result.
//
//	where, args := s.userClause("AND", args)
//	query += where
func (s *SessionStore) userClause(conjunction string, args []any) (string, []any) {
	if s.userID == 0 {
		return "", args
	}
	args = append(args, s.userID)
	return fmt.Sprintf(" %s user_id = $%d", conjunction, len(args)), args
}

func (s *SessionStore) migrate(ctx context.Context) error {
	// Phase 1: create tables, columns, and indexes. This includes adding the
	// user_id column (nullable for now) so the fail-loud check and backfill in
	// phase 2 have something to operate on.
	baseStmts := []string{
		`CREATE TABLE IF NOT EXISTS session_turns (
			id         BIGSERIAL PRIMARY KEY,
			session_id TEXT NOT NULL,
			uuid       TEXT NOT NULL,
			turn_index INT NOT NULL,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			cwd        TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(session_id, uuid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_turns_session ON session_turns(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_turns_cwd ON session_turns(cwd)`,
		`CREATE INDEX IF NOT EXISTS idx_session_turns_created ON session_turns(created_at)`,
		// Add cwd column to existing tables that predate this migration.
		`ALTER TABLE session_turns ADD COLUMN IF NOT EXISTS cwd TEXT NOT NULL DEFAULT ''`,

		`CREATE TABLE IF NOT EXISTS session_hooks (
			id         BIGSERIAL PRIMARY KEY,
			session_id TEXT NOT NULL,
			cwd        TEXT NOT NULL DEFAULT '',
			payload    JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_hooks_session ON session_hooks(session_id)`,

		`CREATE TABLE IF NOT EXISTS context_hints (
			id               BIGSERIAL PRIMARY KEY,
			session_id       TEXT NOT NULL,
			cwd              TEXT NOT NULL DEFAULT '',
			turn_index       INT NOT NULL DEFAULT 0,
			hint_text        TEXT NOT NULL,
			ref_ids          JSONB NOT NULL DEFAULT '[]',
			retrieved_ids    JSONB NOT NULL DEFAULT '[]',
			candidate_scores JSONB NOT NULL DEFAULT '{}',
			search_query     TEXT NOT NULL DEFAULT '',
			ranker_version   TEXT NOT NULL DEFAULT '',
			relevance        FLOAT NOT NULL DEFAULT 0,
			desirability     FLOAT NOT NULL DEFAULT 0,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			consumed_at      TIMESTAMPTZ
		)`,
		`ALTER TABLE context_hints ADD COLUMN IF NOT EXISTS cwd TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE context_hints ADD COLUMN IF NOT EXISTS retrieved_ids JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE context_hints ADD COLUMN IF NOT EXISTS candidate_scores JSONB NOT NULL DEFAULT '{}'`,
		`ALTER TABLE context_hints ADD COLUMN IF NOT EXISTS search_query TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE context_hints ADD COLUMN IF NOT EXISTS ranker_version TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_context_hints_session ON context_hints(session_id) WHERE consumed_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_context_hints_cwd ON context_hints(cwd) WHERE consumed_at IS NULL`,

		`CREATE TABLE IF NOT EXISTS context_injections (
			id          BIGSERIAL PRIMARY KEY,
			session_id  TEXT NOT NULL,
			ref_id      TEXT NOT NULL,
			ref_type    TEXT NOT NULL,
			rank        INT NOT NULL DEFAULT -1,
			injected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(session_id, ref_id, ref_type)
		)`,
		`ALTER TABLE context_injections ADD COLUMN IF NOT EXISTS rank INT NOT NULL DEFAULT -1`,
		`CREATE INDEX IF NOT EXISTS idx_context_injections_session ON context_injections(session_id)`,

		`CREATE TABLE IF NOT EXISTS context_feedback (
			id         BIGSERIAL PRIMARY KEY,
			ref_id     TEXT NOT NULL,
			ref_type   TEXT NOT NULL,
			session_id TEXT NOT NULL,
			score      INT NOT NULL,
			reason     TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(ref_id, ref_type, session_id)
		)`,
		// Backward compatibility: add constraint to tables that predate this migration.
		// PostgreSQL raises 42710 (duplicate_object) for a pre-existing constraint name,
		// but may also raise 42P07 (duplicate_table) in some versions -- catch both.
		`DO $$ BEGIN
			ALTER TABLE context_feedback ADD CONSTRAINT uq_context_feedback_ref_session UNIQUE (ref_id, ref_type, session_id);
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_context_feedback_ref ON context_feedback(ref_id, ref_type)`,
		`CREATE INDEX IF NOT EXISTS idx_context_feedback_session ON context_feedback(session_id)`,

		// --- 012a: add user_id column to all five session tables ---
		// The column is added nullable here; phase 2 backfills it and phase 3
		// applies NOT NULL once it is guaranteed null-free.
		`ALTER TABLE session_turns      ADD COLUMN IF NOT EXISTS user_id BIGINT`,
		`ALTER TABLE session_hooks      ADD COLUMN IF NOT EXISTS user_id BIGINT`,
		`ALTER TABLE context_hints      ADD COLUMN IF NOT EXISTS user_id BIGINT`,
		`ALTER TABLE context_injections ADD COLUMN IF NOT EXISTS user_id BIGINT`,
		`ALTER TABLE context_feedback   ADD COLUMN IF NOT EXISTS user_id BIGINT`,
	}
	for _, stmt := range baseStmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("session store migrate: %w\nstatement: %s", err, stmt)
		}
	}

	// Phase 2: resolve the default user and backfill existing rows. If session
	// data exists but no default user can be resolved, fail loudly HERE -- before
	// the unguarded SET NOT NULL in phase 3 would otherwise either error on a
	// null-bearing column or (if masked) leave a half-migrated table.
	if err := s.backfillSessionUser(ctx); err != nil {
		return err
	}

	// Phase 3: apply NOT NULL, FK, indexes, and unique-key widening. SET NOT NULL
	// is unguarded: the column is now guaranteed null-free (fresh/empty DB, or
	// backfilled in phase 2) and SET NOT NULL is a no-op on an already-NOT-NULL
	// column, so re-runs are safe and a genuine failure surfaces clearly.
	constraintStmts := []string{
		// NOT NULL (unguarded -- see phase 3 note above).
		`ALTER TABLE session_turns      ALTER COLUMN user_id SET NOT NULL`,
		`ALTER TABLE session_hooks      ALTER COLUMN user_id SET NOT NULL`,
		`ALTER TABLE context_hints      ALTER COLUMN user_id SET NOT NULL`,
		`ALTER TABLE context_injections ALTER COLUMN user_id SET NOT NULL`,
		`ALTER TABLE context_feedback   ALTER COLUMN user_id SET NOT NULL`,

		// FK constraints.
		`DO $$ BEGIN
			ALTER TABLE session_turns ADD CONSTRAINT fk_session_turns_user
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE session_hooks ADD CONSTRAINT fk_session_hooks_user
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE context_hints ADD CONSTRAINT fk_context_hints_user
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE context_injections ADD CONSTRAINT fk_context_injections_user
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE context_feedback ADD CONSTRAINT fk_context_feedback_user
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,

		// Indexes on user_id.
		`CREATE INDEX IF NOT EXISTS idx_session_turns_user      ON session_turns(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_hooks_user      ON session_hooks(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_context_hints_user      ON context_hints(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_context_injections_user ON context_injections(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_context_feedback_user   ON context_feedback(user_id)`,

		// Widen unique constraints to include user_id. Drop the old narrow
		// constraints first (idempotent: exception on missing), then add the
		// wide ones (idempotent: exception on duplicate_object).
		//
		//    session_turns: was UNIQUE(session_id, uuid)
		`DO $$ BEGIN
			ALTER TABLE session_turns DROP CONSTRAINT IF EXISTS session_turns_session_id_uuid_key;
		EXCEPTION WHEN undefined_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE session_turns ADD CONSTRAINT uq_session_turns_user_session_uuid
				UNIQUE (user_id, session_id, uuid);
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,

		//    context_injections: was UNIQUE(session_id, ref_id, ref_type)
		`DO $$ BEGIN
			ALTER TABLE context_injections DROP CONSTRAINT IF EXISTS context_injections_session_id_ref_id_ref_type_key;
		EXCEPTION WHEN undefined_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE context_injections ADD CONSTRAINT uq_context_injections_user_session_ref
				UNIQUE (user_id, session_id, ref_id, ref_type);
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,

		//    context_feedback: was UNIQUE(ref_id, ref_type, session_id) via named constraint
		`DO $$ BEGIN
			ALTER TABLE context_feedback DROP CONSTRAINT uq_context_feedback_ref_session;
		EXCEPTION WHEN undefined_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE context_feedback DROP CONSTRAINT IF EXISTS context_feedback_ref_id_ref_type_session_id_key;
		EXCEPTION WHEN undefined_object THEN NULL;
		END $$`,
		`DO $$ BEGIN
			ALTER TABLE context_feedback ADD CONSTRAINT uq_context_feedback_user_ref_session
				UNIQUE (user_id, ref_id, ref_type, session_id);
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,
	}
	for _, stmt := range constraintStmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("session store migrate: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// backfillSessionUser resolves the default user and stamps it onto any session
// rows whose user_id is still NULL. It is the fail-loud gate before the
// unguarded SET NOT NULL in migrate's phase 3:
//
//   - If a default user resolves, every NULL user_id row is backfilled to it.
//   - If NO default user resolves but session rows exist, it returns the
//     tier3-init error so the operator seeds identity before the column is made
//     NOT NULL. (On the daemon path the facts-layer store has already recorded
//     the default user, so this never fires there.)
//   - If no default user resolves and all session tables are empty, it is a
//     harmless no-op (a fresh DB; the column is trivially null-free).
func (s *SessionStore) backfillSessionUser(ctx context.Context) error {
	var defUID *int64
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM memstore_users
		WHERE name = (SELECT value FROM memstore_meta WHERE key = 'default_user')
		LIMIT 1
	`).Scan(&defUID)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("session store migrate: resolving default user: %w", err)
	}

	if defUID == nil {
		// No default user. Only safe if there is no session data to backfill.
		hasData, derr := s.hasSessionData(ctx)
		if derr != nil {
			return fmt.Errorf("session store migrate: checking for session data: %w", derr)
		}
		if hasData {
			return fmt.Errorf("session store migrate: no default user recorded -- run 'memstore admin tier3-init --default-user <name>' before starting memstored")
		}
		return nil
	}

	for _, tbl := range []string{
		"session_turns", "session_hooks", "context_hints",
		"context_injections", "context_feedback",
	} {
		if _, err := s.pool.Exec(ctx,
			`UPDATE `+tbl+` SET user_id = $1 WHERE user_id IS NULL`, *defUID,
		); err != nil {
			return fmt.Errorf("session store migrate: backfilling %s.user_id: %w", tbl, err)
		}
	}
	return nil
}

// hasSessionData reports whether any of the five session tables holds a row.
func (s *SessionStore) hasSessionData(ctx context.Context) (bool, error) {
	var hasData bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM session_turns      LIMIT 1 UNION ALL
			SELECT 1 FROM session_hooks      LIMIT 1 UNION ALL
			SELECT 1 FROM context_hints      LIMIT 1 UNION ALL
			SELECT 1 FROM context_injections LIMIT 1 UNION ALL
			SELECT 1 FROM context_feedback   LIMIT 1
		)`).Scan(&hasData)
	return hasData, err
}

// SaveTurns upserts session turns using a single batched round-trip.
// Stamped-write: stamps user_id = s.userID.
func (s *SessionStore) SaveTurns(ctx context.Context, sessionID string, turns []memstore.SessionTurn) error {
	if len(turns) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range turns {
		batch.Queue(`
			INSERT INTO session_turns(session_id, uuid, turn_index, role, content, cwd, created_at, user_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (user_id, session_id, uuid) DO NOTHING
		`, sessionID, t.UUID, t.TurnIndex, t.Role, t.Content, t.CWD, t.CreatedAt, s.userID)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for _, t := range turns {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("save turn %s: %w", t.UUID, err)
		}
	}
	return nil
}

// SaveHook appends a raw Stop hook payload.
// Stamped-write: stamps user_id = s.userID.
// session_id is extracted from the JSON payload (best-effort); user_id is
// stamped from the store's scope, not the payload, since hooks do not carry
// an owner identity.
func (s *SessionStore) SaveHook(ctx context.Context, payload []byte) error {
	var hook struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	json.Unmarshal(payload, &hook) // best-effort
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_hooks(session_id, cwd, payload, user_id)
		VALUES ($1, $2, $3, $4)
	`, hook.SessionID, hook.CWD, payload, s.userID)
	return err
}

// normalizeCWD cleans and normalizes a working directory path so that hints
// stored under one representation are found by equivalent representations.
// Returns empty string unchanged so SQL guards ($n != ”) work correctly.
func normalizeCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Clean(cwd)
}

// StoreHint stores a context hint produced by the Ollama pipeline.
// Stamped-write: stamps user_id = s.userID.
func (s *SessionStore) StoreHint(ctx context.Context, hint memstore.ContextHint) (int64, error) {
	refIDs, _ := json.Marshal(hint.RefIDs)
	retrievedIDs, _ := json.Marshal(hint.RetrievedIDs)
	candidateScores, _ := json.Marshal(hint.CandidateScores)
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO context_hints(
			session_id, cwd, turn_index, hint_text,
			ref_ids, retrieved_ids, candidate_scores,
			search_query, ranker_version,
			relevance, desirability, user_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id
	`, hint.SessionID, normalizeCWD(hint.CWD), hint.TurnIndex, hint.HintText,
		refIDs, retrievedIDs, candidateScores,
		hint.SearchQuery, hint.RankerVersion,
		hint.Relevance, hint.Desirability, s.userID).Scan(&id)
	return id, err
}

func scanHints(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]memstore.ContextHint, error) {
	defer rows.Close()
	var hints []memstore.ContextHint
	for rows.Next() {
		var h memstore.ContextHint
		var refIDsRaw, retrievedIDsRaw, candidateScoresRaw []byte
		if err := rows.Scan(
			&h.ID, &h.SessionID, &h.CWD, &h.TurnIndex, &h.HintText,
			&refIDsRaw, &retrievedIDsRaw, &candidateScoresRaw,
			&h.SearchQuery, &h.RankerVersion,
			&h.Relevance, &h.Desirability, &h.CreatedAt,
		); err != nil {
			return nil, err
		}
		json.Unmarshal(refIDsRaw, &h.RefIDs)
		json.Unmarshal(retrievedIDsRaw, &h.RetrievedIDs)
		json.Unmarshal(candidateScoresRaw, &h.CandidateScores)
		hints = append(hints, h)
	}
	return hints, rows.Err()
}

// GetPendingHints returns unconsumed hints matching sessionID or cwd (OR semantics),
// ordered by relevance*desirability desc. Either parameter may be empty.
// Scoped-read: filters AND user_id = s.userID when userID != 0.
func (s *SessionStore) GetPendingHints(ctx context.Context, sessionID, cwd string) ([]memstore.ContextHint, error) {
	args := []any{sessionID, normalizeCWD(cwd)}
	userWhere, args := s.userClause("AND", args)
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, cwd, turn_index, hint_text,
		       ref_ids, retrieved_ids, candidate_scores,
		       search_query, ranker_version,
		       relevance, desirability, created_at
		FROM context_hints
		WHERE consumed_at IS NULL
		  AND (($1 != '' AND session_id = $1) OR ($2 != '' AND cwd = $2))`+
		userWhere+`
		ORDER BY (relevance * desirability) DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	return scanHints(rows)
}

// MarkHintConsumed marks a hint as consumed.
// Scoped-read/mutation: filters AND user_id = s.userID so a user cannot
// consume another user's hint.
func (s *SessionStore) MarkHintConsumed(ctx context.Context, hintID int64) error {
	args := []any{hintID}
	userWhere, args := s.userClause("AND", args)
	_, err := s.pool.Exec(ctx,
		`UPDATE context_hints SET consumed_at = NOW() WHERE id = $1`+userWhere,
		args...,
	)
	return err
}

// RecordInjection records that a ref was injected into a session.
// rank is the 0-based position of the item in the candidate list; -1 if unknown.
// Stamped-write: stamps user_id = s.userID. Ignores conflicts (idempotent).
func (s *SessionStore) RecordInjection(ctx context.Context, sessionID, refID, refType string, rank int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO context_injections(session_id, ref_id, ref_type, rank, user_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, session_id, ref_id, ref_type) DO NOTHING
	`, sessionID, refID, refType, rank, s.userID)
	return err
}

// WasInjected returns true if refID+refType was already injected this session.
// Scoped-read: filters AND user_id = s.userID when userID != 0.
func (s *SessionStore) WasInjected(ctx context.Context, sessionID, refID, refType string) (bool, error) {
	args := []any{sessionID, refID, refType}
	userWhere, args := s.userClause("AND", args)
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM context_injections
			WHERE session_id=$1 AND ref_id=$2 AND ref_type=$3`+
			userWhere+`
		)`, args...).Scan(&exists)
	return exists, err
}

// RecordFeedback stores Claude's rating of an injected context item.
// Stamped-write: stamps user_id = s.userID. One rating per (user_id, ref_id,
// ref_type, session_id) -- silently ignores duplicates.
func (s *SessionStore) RecordFeedback(ctx context.Context, fb memstore.ContextFeedback) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO context_feedback(ref_id, ref_type, session_id, score, reason, user_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, ref_id, ref_type, session_id) DO NOTHING
	`, fb.RefID, fb.RefType, fb.SessionID, fb.Score, fb.Reason, s.userID)
	return err
}

// GetInjectedFactIDs returns the IDs of facts injected into the given session
// via recall. Used for auto-rating at session end.
// Scoped-read: filters AND user_id = s.userID when userID != 0.
func (s *SessionStore) GetInjectedFactIDs(ctx context.Context, sessionID string) ([]int64, error) {
	args := []any{sessionID}
	userWhere, args := s.userClause("AND", args)
	rows, err := s.pool.Query(ctx,
		`SELECT ref_id::bigint FROM context_injections
		WHERE session_id = $1 AND ref_type = 'fact'`+
			userWhere+`
		ORDER BY rank ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetInjectedHints returns hints injected into the given session via recall.
// Used for auto-rating at session end.
// Scoped-read: filters on ci.user_id when userID != 0.
func (s *SessionStore) GetInjectedHints(ctx context.Context, sessionID string) ([]memstore.ContextHint, error) {
	args := []any{sessionID}
	userWhere := ""
	if s.userID != 0 {
		args = append(args, s.userID)
		userWhere = fmt.Sprintf(" AND ci.user_id = $%d", len(args))
	}
	rows, err := s.pool.Query(ctx,
		`SELECT ch.id, ch.session_id, ch.cwd, ch.turn_index, ch.hint_text,
		       ch.ref_ids, ch.retrieved_ids, ch.candidate_scores,
		       ch.search_query, ch.ranker_version,
		       ch.relevance, ch.desirability, ch.created_at
		FROM context_hints ch
		JOIN context_injections ci
		  ON ci.ref_id = ch.id::text AND ci.ref_type = 'hint'
		WHERE ci.session_id = $1`+
			userWhere+`
		ORDER BY ci.injected_at ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	return scanHints(rows)
}

// FeedbackScores returns the average feedback score and rating count for each
// ref across all sessions. Only refs with feedback data are included.
// Scoped-read: filters AND user_id = s.userID when userID != 0.
// Service-conditional: at userID 0 (service scope) spans all users.
func (s *SessionStore) FeedbackScores(ctx context.Context, refIDs []string, refType string) (map[string]memstore.FeedbackStat, error) {
	if len(refIDs) == 0 {
		return nil, nil
	}
	args := []any{refIDs, refType}
	userWhere, args := s.userClause("AND", args)
	rows, err := s.pool.Query(ctx,
		`SELECT ref_id, AVG(score)::float8, COUNT(*)::int FROM context_feedback
		WHERE ref_id = ANY($1) AND ref_type = $2`+
			userWhere+`
		GROUP BY ref_id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]memstore.FeedbackStat)
	for rows.Next() {
		var refID string
		var stat memstore.FeedbackStat
		if err := rows.Scan(&refID, &stat.Avg, &stat.Count); err != nil {
			return nil, err
		}
		stats[refID] = stat
	}
	return stats, rows.Err()
}

// UnratedFactSessions returns session IDs that have fact injections with no
// corresponding feedback. Used by the backfill-feedback command.
// Scoped-read: filters AND ci.user_id = s.userID when userID != 0.
// Service-conditional: at userID 0 (service scope) spans all users.
func (s *SessionStore) UnratedFactSessions(ctx context.Context) ([]string, error) {
	args := []any{}
	userWhere := ""
	if s.userID != 0 {
		args = append(args, s.userID)
		userWhere = fmt.Sprintf(" AND ci.user_id = $%d", len(args))
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ci.session_id
		FROM context_injections ci
		WHERE ci.ref_type = 'fact'`+
			userWhere+`
		AND EXISTS (SELECT 1 FROM session_turns st WHERE st.session_id = ci.session_id)
		AND NOT EXISTS (
			SELECT 1 FROM context_feedback cf
			WHERE cf.ref_id = ci.ref_id AND cf.ref_type = ci.ref_type
			AND cf.session_id = ci.session_id
		)
		ORDER BY ci.session_id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetSessionTurns returns all turns for a given session, ordered by turn index.
// Scoped-read: filters AND user_id = s.userID when userID != 0.
func (s *SessionStore) GetSessionTurns(ctx context.Context, sessionID string) ([]memstore.SessionTurn, error) {
	args := []any{sessionID}
	userWhere, args := s.userClause("AND", args)
	rows, err := s.pool.Query(ctx,
		`SELECT session_id, uuid, turn_index, role, content, cwd, created_at
		FROM session_turns
		WHERE session_id = $1`+
			userWhere+`
		ORDER BY turn_index ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []memstore.SessionTurn
	for rows.Next() {
		var t memstore.SessionTurn
		if err := rows.Scan(&t.SessionID, &t.UUID, &t.TurnIndex, &t.Role, &t.Content, &t.CWD, &t.CreatedAt); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// FeedbackScore returns the average feedback score for a ref across all sessions.
// Returns 0 if no feedback exists.
// Scoped-read: filters AND user_id = s.userID when userID != 0.
func (s *SessionStore) FeedbackScore(ctx context.Context, refID, refType string) (float64, error) {
	args := []any{refID, refType}
	userWhere, args := s.userClause("AND", args)
	var score float64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(AVG(score), 0) FROM context_feedback
		WHERE ref_id=$1 AND ref_type=$2`+userWhere,
		args...,
	).Scan(&score)
	return score, err
}
