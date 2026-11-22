package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/transcript"
)

// SaveArchive records one upload of a session's raw transcript: the index row
// for the stored file, and the segments and tool calls derived from it.
//
// A resumed session uploads again, longer. The index row is updated in place
// and keeps the time of its first upload; the derived rows are deleted and
// written again from the new parse, since a later compaction changes where
// every segment after it falls. It is one transaction, so a reader sees the
// old derivation or the new one.
//
// Stamped-write: every row carries user_id = s.userID.
func (s *SessionStore) SaveArchive(ctx context.Context, a memstore.SessionArchive, parsed *transcript.Session) (int64, error) {
	if s.userID == 0 {
		return 0, fmt.Errorf("pgstore: SaveArchive needs a user-scoped session store")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("save archive: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO session_archives(user_id, session_id, path, stored_bytes, raw_bytes, sha256,
			lines, bad_lines, title, cwd, started_at, ended_at, compactions, parser_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (user_id, session_id) DO UPDATE SET
			path = EXCLUDED.path, stored_bytes = EXCLUDED.stored_bytes, raw_bytes = EXCLUDED.raw_bytes,
			sha256 = EXCLUDED.sha256, lines = EXCLUDED.lines, bad_lines = EXCLUDED.bad_lines,
			title = EXCLUDED.title, cwd = EXCLUDED.cwd, started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at, compactions = EXCLUDED.compactions,
			parser_version = EXCLUDED.parser_version, last_uploaded_at = NOW(), removed_at = NULL
		RETURNING id`,
		s.userID, a.SessionID, a.Path, a.StoredBytes, a.RawBytes, a.SHA256,
		parsed.Lines, parsed.BadLines, stripNUL(parsed.Title), stripNUL(parsed.CWD),
		nullTime(parsed.StartedAt), nullTime(parsed.EndedAt), len(parsed.Compactions), transcript.Version,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("save archive: %w", err)
	}

	for _, table := range []string{"session_segments", "session_tool_calls"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE archive_id = $1`, id); err != nil {
			return 0, fmt.Errorf("save archive: clearing %s: %w", table, err)
		}
	}

	segs := make([][]any, len(parsed.Segments))
	for i, g := range parsed.Segments {
		segs[i] = []any{
			id, s.userID, g.Ordinal, g.FirstLine, g.LastLine, g.StartOffset, g.EndOffset,
			nullTime(g.StartedAt), nullTime(g.EndedAt), nullOrdinal(g.After), nullOrdinal(g.Before),
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"session_segments"},
		[]string{"archive_id", "user_id", "ordinal", "first_line", "last_line", "start_offset", "end_offset",
			"started_at", "ended_at", "after_compaction", "before_compaction"},
		pgx.CopyFromRows(segs)); err != nil {
		return 0, fmt.Errorf("save archive: segments: %w", err)
	}

	tools := make([][]any, len(parsed.Tools))
	for i, c := range parsed.Tools {
		// The input goes in as text for Postgres to parse as JSONB: a NUL
		// escape has to come out of the encoded form first (see nulbyte.go),
		// and an input that is not there at all is a SQL NULL.
		var input any
		if len(c.Input) > 0 {
			input = string(stripNULJSON(c.Input))
		}
		tools[i] = []any{
			id, s.userID, stripNUL(c.ID), stripNUL(c.Name), input, c.Line, nullTime(c.At),
			nullOrdinal(c.Segment), c.Sidechain, c.HasResult, c.IsError,
			c.ResultLine, c.ResultOffset, c.ResultBytes,
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"session_tool_calls"},
		[]string{"archive_id", "user_id", "call_id", "name", "input", "line", "called_at",
			"segment", "sidechain", "has_result", "is_error", "result_line", "result_offset", "result_bytes"},
		pgx.CopyFromRows(tools)); err != nil {
		return 0, fmt.Errorf("save archive: tool calls: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("save archive: %w", err)
	}
	return id, nil
}

// nullTime stores a zero time as NULL: a transcript entry with no timestamp
// has no time, and year 1 is not one.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullOrdinal stores transcript.None as NULL.
func nullOrdinal(n int) any {
	if n == transcript.None {
		return nil
	}
	return n
}
