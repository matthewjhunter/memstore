package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore/internal/screening"
)

// This file implements screening.PendingStore for SQLiteStore, plus the finding
// records that give an operator visibility into what screening decided.
//
// The state transitions live here rather than in the worker because they must be
// atomic with the finding they justify: a fact that flips to blocked without a
// recorded reason is indistinguishable from one that vanished.

// PendingFacts returns writes awaiting screening, oldest first.
//
// Abandoned facts are excluded by state, which is what keeps one unscreenable fact
// from heading the queue forever.
func (s *SQLiteStore) PendingFacts(ctx context.Context, limit int) ([]screening.PendingFact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 16
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, metadata, screen_attempts
		 FROM memstore_facts
		 WHERE namespace = ? AND screen_state = 'pending'
		 ORDER BY id LIMIT ?`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memstore: querying pending facts: %w", err)
	}
	defer rows.Close()

	var out []screening.PendingFact
	for rows.Next() {
		var f screening.PendingFact
		var content string
		var meta sql.NullString
		if err := rows.Scan(&f.ID, &content, &meta, &f.Attempts); err != nil {
			return nil, fmt.Errorf("memstore: scanning pending fact: %w", err)
		}
		f.Content = ScreenableText(content, meta.String)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ScreenableText composes what the screener actually judges: the fact's content plus
// every metadata string value.
//
// Metadata has to be in here. It is rendered to models alongside content, it is
// writable through a second path (UpdateMetadata), and it is where an attacker would
// go the moment content alone is screened.
//
// Every string value, with no length floor. An earlier version skipped values under 80
// runes on the theory that short values are enum-ish and cannot say much -- which is
// false, and the tests said so immediately: "ignore all previous instructions" is 32
// characters and a complete attack, so the floor was a documented hole exactly the
// size of the most common payload. The renderer has a length threshold because
// inline-versus-fenced is a layout question; screening has no equivalent excuse, and
// scanning a few extra short strings costs nothing.
//
// Values are walked recursively, so a payload nested inside an object or array is
// found rather than skipped for not being a top-level string.
func ScreenableText(content, metadata string) string {
	if metadata == "" || metadata == "null" {
		return content
	}
	var parsed any
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		// Unparseable metadata gets screened whole: its shape is unknown, so no part
		// of it can be dismissed as too short to matter.
		return content + "\n\n" + metadata
	}

	var values []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				values = append(values, t)
			}
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(parsed)

	if len(values) == 0 {
		return content
	}
	return content + "\n\n" + strings.Join(values, "\n\n")
}

// Resolve applies a terminal screening decision and records the finding in the same
// transaction.
func (s *SQLiteStore) Resolve(ctx context.Context, id int64, d screening.Decision) error {
	state := ScreenClean
	if d.Blocked() {
		state = ScreenBlocked
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memstore: beginning screening transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE memstore_facts SET screen_state = ?, screened_at = ?
		 WHERE id = ? AND namespace = ? AND screen_state = 'pending'`,
		string(state), time.Now().UTC().Unix(), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: resolving fact %d: %w", id, err)
	}
	// A fact that is no longer pending was resolved by someone else. Recording a
	// second finding for it would double-count the block in any tally built on this
	// table, so treat the race as a no-op.
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}

	if err := insertFinding(ctx, tx, s.namespace, &id, d); err != nil {
		return err
	}
	return tx.Commit()
}

// Defer records a failed screening attempt, leaving the fact pending for a later tick.
func (s *SQLiteStore) Defer(ctx context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET screen_attempts = screen_attempts + 1
		 WHERE id = ? AND namespace = ? AND screen_state = 'pending'`,
		id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: deferring fact %d: %w", id, err)
	}
	return nil
}

// Abandon stops the worker retrying a fact.
//
// The row is kept and stays unreadable. Abandoning is a decision about the screening
// process, not about the content, so the finding records the outcome without claiming
// a threat was found.
func (s *SQLiteStore) Abandon(ctx context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memstore: beginning abandon transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE memstore_facts SET screen_state = 'abandoned', screened_at = ?
		 WHERE id = ? AND namespace = ? AND screen_state = 'pending'`,
		time.Now().UTC().Unix(), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: abandoning fact %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}

	d := screening.Decision{Outcome: screening.OutcomeAbandoned, Reason: reason}
	if err := insertFinding(ctx, tx, s.namespace, &id, d); err != nil {
		return err
	}
	return tx.Commit()
}

// insertFinding writes the payload-free record of a screening decision.
//
// Every column here is either a number, a fixed-vocabulary string, or a rule ID from
// airlock's corpus. None of it is attacker-authored: the quoted evidence and the
// model's prose are deliberately absent, because this table is read by dashboards,
// log scrapers, and eventually a UI, none of which fence what they render.
func insertFinding(ctx context.Context, tx *sql.Tx, namespace string, factID *int64, d screening.Decision) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memstore_screen_findings
			(namespace, fact_id, outcome, threat, category, verified,
			 detect_score, detect_rules, obfuscated, model_screened, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		namespace, factID, string(d.Outcome), d.Threat, d.Category, boolInt(d.Verified),
		d.DetectScore, strings.Join(d.DetectRules, ","), boolInt(d.Obfuscated),
		boolInt(d.ModelScreened), d.Reason, time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("memstore: recording screening finding: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ScreenCounts returns how many facts sit in each screening state.
//
// This is the visibility surface until there is a UI: it answers "is the worker
// keeping up", "how much of the corpus predates screening", and "has anything been
// blocked".
func (s *SQLiteStore) ScreenCounts(ctx context.Context) (map[ScreenState]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT screen_state, COUNT(*) FROM memstore_facts WHERE namespace = ? GROUP BY screen_state`,
		s.namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("memstore: counting screen states: %w", err)
	}
	defer rows.Close()

	out := map[ScreenState]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, fmt.Errorf("memstore: scanning screen state count: %w", err)
		}
		out[ScreenState(st)] = n
	}
	return out, rows.Err()
}

// BlockedFact is a rejected write, surfaced for review.
//
// Content is included: an operator deciding whether a block was a false positive has
// to see what was blocked, and this content exists nowhere else -- a blocked write
// never becomes a readable fact. Callers rendering it to a model must fence it, the
// same as any stored content.
type BlockedFact struct {
	ID        int64
	Subject   string
	Content   string
	Threat    int
	Category  string
	Reason    string
	CreatedAt time.Time
}

// BlockedFacts returns writes screening rejected, newest first.
func (s *SQLiteStore) BlockedFacts(ctx context.Context, limit int) ([]BlockedFact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		// The finding is selected as a correlated subquery rather than joined: a fact
		// accumulates findings over its lifetime (blocked, released, re-screened,
		// blocked again), and a join would return the same fact once per finding. The
		// newest one is the current verdict.
		`SELECT f.id, f.subject, f.content,
		        COALESCE((SELECT threat   FROM memstore_screen_findings
		                   WHERE fact_id = f.id AND namespace = f.namespace
		                   ORDER BY id DESC LIMIT 1), 0),
		        COALESCE((SELECT category FROM memstore_screen_findings
		                   WHERE fact_id = f.id AND namespace = f.namespace
		                   ORDER BY id DESC LIMIT 1), ''),
		        COALESCE((SELECT reason   FROM memstore_screen_findings
		                   WHERE fact_id = f.id AND namespace = f.namespace
		                   ORDER BY id DESC LIMIT 1), ''),
		        f.created_at
		 FROM memstore_facts f
		 WHERE f.namespace = ? AND f.screen_state = 'blocked'
		 ORDER BY f.id DESC LIMIT ?`,
		s.namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memstore: querying blocked facts: %w", err)
	}
	defer rows.Close()

	var out []BlockedFact
	for rows.Next() {
		var b BlockedFact
		var created string
		if err := rows.Scan(&b.ID, &b.Subject, &b.Content, &b.Threat, &b.Category, &b.Reason, &created); err != nil {
			return nil, fmt.Errorf("memstore: scanning blocked fact: %w", err)
		}
		b.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ReleaseFact overrides a block, making the fact readable.
//
// This is the false-positive escape hatch, and the reason blocked rows are retained
// rather than deleted. It is a human decision: nothing in the screening pipeline calls
// it. The fact becomes grandfathered rather than clean, because it was admitted by
// override and not by passing a screen.
func (s *SQLiteStore) ReleaseFact(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx,
		`UPDATE memstore_facts SET screen_state = 'grandfathered', screened_at = ?
		 WHERE id = ? AND namespace = ? AND screen_state IN ('blocked','abandoned')`,
		time.Now().UTC().Unix(), id, s.namespace,
	)
	if err != nil {
		return fmt.Errorf("memstore: releasing fact %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memstore: fact %d is not blocked or abandoned", id)
	}
	return nil
}
