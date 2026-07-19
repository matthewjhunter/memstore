package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/screening"
)

// This file implements screening.PendingStore for PostgresStore. It mirrors the
// SQLite implementation in the root package; the two must agree, since screening
// semantics cannot change with the backend.
//
// Postgres is the backend the daemon runs, which is the only deployment with a
// screening worker -- so in practice this is the file that carries the model-screening
// path.

// PendingFacts returns writes awaiting screening, oldest first.
//
// The claim is not locked. Ticks within one worker do not overlap, and two daemons
// against one database would at worst screen the same fact twice -- which is wasteful
// but harmless, because Resolve only applies to a row still pending and so the second
// verdict lands as a no-op rather than a second finding.
func (s *PostgresStore) PendingFacts(ctx context.Context, limit int) ([]screening.PendingFact, error) {
	if limit <= 0 {
		limit = 16
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, content, COALESCE(metadata::text, ''), screen_attempts
		 FROM memstore_facts
		 WHERE namespace = $1 AND screen_state IN ('pending','screening')
		 ORDER BY id LIMIT $2`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("pgstore: querying pending facts: %w", err)
	}
	defer rows.Close()

	var out []screening.PendingFact
	for rows.Next() {
		var f screening.PendingFact
		var content, meta string
		if err := rows.Scan(&f.ID, &content, &meta, &f.Attempts); err != nil {
			return nil, fmt.Errorf("pgstore: scanning pending fact: %w", err)
		}
		f.Content = memstore.ScreenableText(content, meta)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Resolve applies a terminal screening decision and records the finding atomically.
func (s *PostgresStore) Resolve(ctx context.Context, id int64, d screening.Decision) error {
	state := memstore.ScreenClean
	if d.Blocked() {
		state = memstore.ScreenBlocked
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: beginning screening transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx,
		`UPDATE memstore_facts SET screen_state = $1, screened_at = now()
		 WHERE id = $2 AND namespace = $3 AND screen_state IN ('pending','screening')`,
		string(state), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: resolving fact %d: %w", id, err)
	}
	// Already resolved by someone else: recording a second finding would double-count
	// the block in any tally built on this table.
	if ct.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}

	if err := insertFinding(ctx, tx, s.namespace, &id, d); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Defer records a failed screening attempt, leaving the fact pending.
func (s *PostgresStore) Defer(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET screen_attempts = screen_attempts + 1
		 WHERE id = $1 AND namespace = $2 AND screen_state IN ('pending','screening')`,
		id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: deferring fact %d: %w", id, err)
	}
	return nil
}

// Abandon stops the worker retrying a fact.
//
// Abandoning never changes whether a fact is readable: a gate-mode fact was being held,
// so it stays unreadable as 'abandoned'; an observe-mode fact was already readable, so
// it settles at 'regex-clean', which is exactly what it passed on the way in. See the
// SQLiteStore method for why.
func (s *PostgresStore) Abandon(ctx context.Context, id int64, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: beginning abandon transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx,
		`UPDATE memstore_facts
		 SET screen_state = CASE screen_state
		         WHEN 'screening' THEN 'regex-clean'
		         ELSE 'abandoned' END,
		     screened_at = now()
		 WHERE id = $1 AND namespace = $2 AND screen_state IN ('pending','screening')`,
		id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: abandoning fact %d: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}

	d := screening.Decision{Outcome: screening.OutcomeAbandoned, Reason: reason}
	if err := insertFinding(ctx, tx, s.namespace, &id, d); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertFinding writes the payload-free record of a screening decision. No column here
// holds attacker-authored text -- see screen.Finding for why the quoted evidence is
// deliberately absent.
func insertFinding(ctx context.Context, tx pgx.Tx, namespace string, factID *int64, d screening.Decision) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO memstore_screen_findings
			(namespace, fact_id, outcome, threat, category, verified,
			 detect_score, detect_rules, obfuscated, model_screened, reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		namespace, factID, string(d.Outcome), d.Threat, d.Category, d.Verified,
		d.DetectScore, strings.Join(d.DetectRules, ","), d.Obfuscated,
		d.ModelScreened, d.Reason,
	)
	if err != nil {
		return fmt.Errorf("pgstore: recording screening finding: %w", err)
	}
	return nil
}

// ScreenCounts returns how many facts sit in each screening state.
func (s *PostgresStore) ScreenCounts(ctx context.Context) (map[memstore.ScreenState]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT screen_state, COUNT(*) FROM memstore_facts WHERE namespace = $1 GROUP BY screen_state`,
		s.namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("pgstore: counting screen states: %w", err)
	}
	defer rows.Close()

	out := map[memstore.ScreenState]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, fmt.Errorf("pgstore: scanning screen state count: %w", err)
		}
		out[memstore.ScreenState(st)] = n
	}
	return out, rows.Err()
}

// BlockedFacts returns writes screening rejected, newest first.
//
// Content is included deliberately: an operator judging whether a block was a false
// positive has to see what was blocked, and a blocked write exists nowhere else.
// Callers rendering it to a model must fence it like any stored content.
func (s *PostgresStore) BlockedFacts(ctx context.Context, limit int) ([]memstore.BlockedFact, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		// LATERAL, not a plain join: a fact accumulates findings over its lifetime
		// (blocked, released, re-screened, blocked again), and joining them all would
		// return the same fact once per finding. The newest one is the current verdict.
		`SELECT f.id, f.subject, f.content,
		        COALESCE(n.threat, 0), COALESCE(n.category, ''), COALESCE(n.reason, ''), f.created_at
		 FROM memstore_facts f
		 LEFT JOIN LATERAL (
		     SELECT threat, category, reason
		     FROM memstore_screen_findings
		     WHERE fact_id = f.id AND namespace = f.namespace
		     ORDER BY id DESC LIMIT 1
		 ) n ON true
		 WHERE f.namespace = $1 AND f.screen_state = 'blocked'
		 ORDER BY f.id DESC LIMIT $2`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("pgstore: querying blocked facts: %w", err)
	}
	defer rows.Close()

	var out []memstore.BlockedFact
	for rows.Next() {
		var b memstore.BlockedFact
		var created time.Time
		if err := rows.Scan(&b.ID, &b.Subject, &b.Content, &b.Threat, &b.Category, &b.Reason, &created); err != nil {
			return nil, fmt.Errorf("pgstore: scanning blocked fact: %w", err)
		}
		b.CreatedAt = created
		out = append(out, b)
	}
	return out, rows.Err()
}

// ReleaseFact overrides a block, making the fact readable.
//
// The false-positive escape hatch, and why blocked rows are retained rather than
// deleted. It is a human decision: nothing in the pipeline calls it. The fact becomes
// grandfathered, not clean -- it was admitted by override, not by passing a screen.
func (s *PostgresStore) ReleaseFact(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE memstore_facts SET screen_state = 'grandfathered', screened_at = now()
		 WHERE id = $1 AND namespace = $2 AND screen_state IN ('blocked','abandoned')`,
		id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("pgstore: releasing fact %d: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return errors.New("pgstore: fact is not blocked or abandoned")
	}
	return nil
}
