package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/matthewjhunter/memstore"
)

// SessionStore keeps citations and serves history to the citation backfill.
var (
	_ memstore.CitationRecorder = (*SessionStore)(nil)
	_ memstore.CitationSource   = (*SessionStore)(nil)
)

// RecordCitations stores citations for this store's user, skipping any already
// recorded for the same session and fact, and returns how many were new.
func (s *SessionStore) RecordCitations(ctx context.Context, cites []memstore.FactCitation) (int, error) {
	if len(cites) == 0 {
		return 0, nil
	}
	if s.userID == 0 {
		// The service scope reads across users and has no user to stamp.
		return 0, errors.New("pgstore: RecordCitations needs a user-scoped session store")
	}
	batch := &pgx.Batch{}
	for _, c := range cites {
		at := c.CitedAt
		if at.IsZero() {
			at = time.Now()
		}
		batch.Queue(`
			INSERT INTO fact_citations(user_id, session_id, fact_id, turn_uuid, cited_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, session_id, fact_id) DO NOTHING
		`, s.userID, c.SessionID, c.FactID, c.TurnUUID, at)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	n := 0
	for range cites {
		tag, err := br.Exec()
		if err != nil {
			return n, fmt.Errorf("pgstore: recording citation: %w", err)
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// CitingSessions returns the sessions, for this store's user, whose assistant
// turns contain the citation form. It is a coarse filter for the backfill:
// the handler still strips code spans and checks each id.
func (s *SessionStore) CitingSessions(ctx context.Context) ([]string, error) {
	args := []any{}
	userWhere, args := s.userClause("AND", args)
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT session_id FROM session_turns
		WHERE role = 'assistant' AND content ~ '\[fact [0-9]+\]'`+userWhere+`
		ORDER BY session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: listing citing sessions: %w", err)
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
