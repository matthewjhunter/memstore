package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
)

// loadFactsFromPG reads facts straight out of Postgres for the scan.
//
// It deliberately does not go through pgstore. Two reasons, both of which matter for a
// command whose whole job is to tell you the truth about a corpus before you enforce
// anything on it:
//
//  1. pgstore.New runs migrations. A read-only reporting command must not quietly
//     alter production schema -- and on a store that predates screening it would add
//     the screening columns and grandfather the corpus as a side effect of asking a
//     question about it.
//
//  2. Store reads apply the screening visibility filter, so List would skip pending,
//     blocked, and abandoned facts. Those are precisely the rows a calibration scan
//     exists to look at; a scan that cannot see what enforcement did is useless for
//     deciding whether enforcement is right.
//
// So this is a plain SELECT over every fact in the namespace, whatever state it is in.
func loadFactsFromPG(ctx context.Context, dsn, namespace string, limit int, subject string) ([]memstore.Fact, map[int64]string, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("scan: connect to postgres: %w", err)
	}
	defer pool.Close()

	// screen_state only exists after the screening migration. A scan is most useful
	// on a store that has not run it yet -- that is the deployment deciding whether to
	// turn screening on -- so its absence is normal, not an error.
	var hasState bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name = 'memstore_facts' AND column_name = 'screen_state')`,
	).Scan(&hasState); err != nil {
		return nil, nil, fmt.Errorf("scan: probing schema: %w", err)
	}

	stateCol := `'' AS screen_state`
	if hasState {
		stateCol = `screen_state`
	}
	q := `SELECT id, subject, category, kind, subsystem, content,
	             COALESCE(metadata::text, ''), ` + stateCol + `
	      FROM memstore_facts
	      WHERE namespace = $1 AND superseded_by IS NULL`
	args := []any{namespace}
	if subject != "" {
		q += ` AND subject = $2`
		args = append(args, subject)
	}
	q += ` ORDER BY id`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("scan: querying facts: %w", err)
	}
	defer rows.Close()

	var facts []memstore.Fact
	states := map[int64]string{}
	for rows.Next() {
		var f memstore.Fact
		var meta, state string
		if err := rows.Scan(&f.ID, &f.Subject, &f.Category, &f.Kind, &f.Subsystem,
			&f.Content, &meta, &state); err != nil {
			return nil, nil, fmt.Errorf("scan: scanning fact: %w", err)
		}
		if meta != "" {
			f.Metadata = []byte(meta)
		}
		facts = append(facts, f)
		if state != "" {
			states[f.ID] = state
		}
	}
	if err := rows.Err(); err != nil && err != pgx.ErrNoRows {
		return nil, nil, fmt.Errorf("scan: reading facts: %w", err)
	}
	return facts, states, nil
}
