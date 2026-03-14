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
// It is independent of PostgresStore — it manages its own tables.
type SessionStore struct {
	pool *pgxpool.Pool
}

// NewSessionStore creates a SessionStore and runs its migrations.
func NewSessionStore(ctx context.Context, pool *pgxpool.Pool) (*SessionStore, error) {
	s := &SessionStore{pool: pool}
	return s, s.migrate(ctx)
}

func (s *SessionStore) migrate(ctx context.Context) error {
	stmts := []string{
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
		// but may also raise 42P07 (duplicate_table) in some versions — catch both.
		`DO $$ BEGIN
			ALTER TABLE context_feedback ADD CONSTRAINT uq_context_feedback_ref_session UNIQUE (ref_id, ref_type, session_id);
		EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_context_feedback_ref ON context_feedback(ref_id, ref_type)`,
		`CREATE INDEX IF NOT EXISTS idx_context_feedback_session ON context_feedback(session_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("session store migrate: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// SaveTurns upserts session turns using a single batched round-trip.
func (s *SessionStore) SaveTurns(ctx context.Context, sessionID string, turns []memstore.SessionTurn) error {
	if len(turns) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range turns {
		batch.Queue(`
			INSERT INTO session_turns(session_id, uuid, turn_index, role, content, cwd, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (session_id, uuid) DO NOTHING
		`, sessionID, t.UUID, t.TurnIndex, t.Role, t.Content, t.CWD, t.CreatedAt)
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
func (s *SessionStore) SaveHook(ctx context.Context, payload []byte) error {
	var hook struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	json.Unmarshal(payload, &hook) // best-effort
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_hooks(session_id, cwd, payload)
		VALUES ($1, $2, $3)
	`, hook.SessionID, hook.CWD, payload)
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
			relevance, desirability
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id
	`, hint.SessionID, normalizeCWD(hint.CWD), hint.TurnIndex, hint.HintText,
		refIDs, retrievedIDs, candidateScores,
		hint.SearchQuery, hint.RankerVersion,
		hint.Relevance, hint.Desirability).Scan(&id)
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
// ordered by relevance×desirability desc. Either parameter may be empty.
func (s *SessionStore) GetPendingHints(ctx context.Context, sessionID, cwd string) ([]memstore.ContextHint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, cwd, turn_index, hint_text,
		       ref_ids, retrieved_ids, candidate_scores,
		       search_query, ranker_version,
		       relevance, desirability, created_at
		FROM context_hints
		WHERE consumed_at IS NULL
		  AND (($1 != '' AND session_id = $1) OR ($2 != '' AND cwd = $2))
		ORDER BY (relevance * desirability) DESC
	`, sessionID, normalizeCWD(cwd))
	if err != nil {
		return nil, err
	}
	return scanHints(rows)
}

// MarkHintConsumed marks a hint as consumed.
func (s *SessionStore) MarkHintConsumed(ctx context.Context, hintID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE context_hints SET consumed_at = NOW() WHERE id = $1
	`, hintID)
	return err
}

// RecordInjection records that a ref was injected into a session.
// rank is the 0-based position of the item in the candidate list; -1 if unknown.
// Ignores conflicts (idempotent).
func (s *SessionStore) RecordInjection(ctx context.Context, sessionID, refID, refType string, rank int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO context_injections(session_id, ref_id, ref_type, rank)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (session_id, ref_id, ref_type) DO NOTHING
	`, sessionID, refID, refType, rank)
	return err
}

// WasInjected returns true if refID+refType was already injected this session.
func (s *SessionStore) WasInjected(ctx context.Context, sessionID, refID, refType string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM context_injections
			WHERE session_id=$1 AND ref_id=$2 AND ref_type=$3
		)
	`, sessionID, refID, refType).Scan(&exists)
	return exists, err
}

// RecordFeedback stores Claude's rating of an injected context item.
// One rating per (ref_id, ref_type, session_id) — silently ignores duplicates.
func (s *SessionStore) RecordFeedback(ctx context.Context, fb memstore.ContextFeedback) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO context_feedback(ref_id, ref_type, session_id, score, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (ref_id, ref_type, session_id) DO NOTHING
	`, fb.RefID, fb.RefType, fb.SessionID, fb.Score, fb.Reason)
	return err
}

// GetInjectedHints returns hints that were injected into the given session,
// identified via the context_injections dedup log. Used for auto-rating at session end.
func (s *SessionStore) GetInjectedHints(ctx context.Context, sessionID string) ([]memstore.ContextHint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ch.id, ch.session_id, ch.cwd, ch.turn_index, ch.hint_text,
		       ch.ref_ids, ch.retrieved_ids, ch.candidate_scores,
		       ch.search_query, ch.ranker_version,
		       ch.relevance, ch.desirability, ch.created_at
		FROM context_hints ch
		JOIN context_injections ci
		  ON ci.ref_id = ch.id::text AND ci.ref_type = 'hint'
		WHERE ci.session_id = $1
		ORDER BY ci.injected_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	return scanHints(rows)
}

// FeedbackScores returns the average feedback score for each ref across all sessions.
// Only refs with feedback data are included in the result map.
func (s *SessionStore) FeedbackScores(ctx context.Context, refIDs []string, refType string) (map[string]float64, error) {
	if len(refIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ref_id, AVG(score)::float8 FROM context_feedback
		WHERE ref_id = ANY($1) AND ref_type = $2
		GROUP BY ref_id
	`, refIDs, refType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scores := make(map[string]float64)
	for rows.Next() {
		var refID string
		var avg float64
		if err := rows.Scan(&refID, &avg); err != nil {
			return nil, err
		}
		scores[refID] = avg
	}
	return scores, rows.Err()
}

// FeedbackScore returns the average feedback score for a ref across all sessions.
// Returns 0 if no feedback exists.
func (s *SessionStore) FeedbackScore(ctx context.Context, refID, refType string) (float64, error) {
	var score float64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(AVG(score), 0) FROM context_feedback
		WHERE ref_id=$1 AND ref_type=$2
	`, refID, refType).Scan(&score)
	return score, err
}
