package pgstore_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/transcript"
)

const archiveSession = "0a219f89-c532-4ee6-b324-54e6dd7b6a97"

// archiveTranscript is a session with one compaction and one tool call. With
// more set, it has been resumed: a second compaction and a later turn.
func archiveTranscript(t *testing.T, more bool) *transcript.Session {
	t.Helper()
	enc := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	lines := []string{
		enc(map[string]any{"type": "ai-title", "aiTitle": "Review the hook logs"}),
		enc(map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-11-01T10:00:00Z", "sessionId": archiveSession, "cwd": "/work/repo",
			"message": map[string]any{"role": "user", "content": "check the tree"}}),
		enc(map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-11-01T10:00:05Z",
			"message": map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "printf 'a\x00b'"}}}}}),
		enc(map[string]any{"type": "user", "uuid": "u2", "timestamp": "2026-11-01T10:00:07Z",
			"message": map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ab"}}}}),
		enc(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "b1", "timestamp": "2026-11-01T11:00:00Z",
			"compactMetadata": map[string]any{"trigger": "auto", "preTokens": 1000}}),
		enc(map[string]any{"type": "user", "uuid": "c1", "parentUuid": "b1", "timestamp": "2026-11-01T11:00:00Z", "isCompactSummary": true,
			"message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation. Summary one."}}),
		enc(map[string]any{"type": "user", "uuid": "u3", "timestamp": "2026-11-01T11:01:00Z",
			"message": map[string]any{"role": "user", "content": "carry on"}}),
	}
	if more {
		lines = append(lines,
			enc(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "b2", "timestamp": "2026-11-01T12:00:00Z",
				"compactMetadata": map[string]any{"trigger": "manual", "preTokens": 2000}}),
			enc(map[string]any{"type": "user", "uuid": "c2", "parentUuid": "b2", "timestamp": "2026-11-01T12:00:00Z", "isCompactSummary": true,
				"message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation. Summary two."}}),
			enc(map[string]any{"type": "user", "uuid": "u4", "timestamp": "2026-11-01T12:01:00Z",
				"message": map[string]any{"role": "user", "content": "and finish"}}),
		)
	}
	s, err := transcript.Parse(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSaveArchive_RecordsTheFileAndWhatWasDerived(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()
	parsed := archiveTranscript(t, false)

	id, err := ss.SaveArchive(ctx, memstore.SessionArchive{
		SessionID: archiveSession, Path: "1/" + archiveSession + ".jsonl.gz",
		StoredBytes: 100, RawBytes: parsed.Bytes, SHA256: "abc",
	}, parsed)
	if err != nil {
		t.Fatalf("SaveArchive: %v", err)
	}

	var title, cwd, sha string
	var lines, compactions, version int
	var userID int64
	if err := pool.QueryRow(ctx, `SELECT title, cwd, sha256, lines, compactions, parser_version, user_id FROM session_archives WHERE id = $1`, id).
		Scan(&title, &cwd, &sha, &lines, &compactions, &version, &userID); err != nil {
		t.Fatal(err)
	}
	if title != "Review the hook logs" || cwd != "/work/repo" || sha != "abc" || lines != 7 || compactions != 1 || version != transcript.Version {
		t.Errorf("archive row = %q %q %q lines %d compactions %d version %d", title, cwd, sha, lines, compactions, version)
	}
	if userID != ss.UserID() {
		t.Errorf("archive user = %d, want %d", userID, ss.UserID())
	}

	// Two segments either side of the compaction. The first is followed by
	// it and starts from nothing; the second is the other way round.
	rows, err := pool.Query(ctx, `SELECT ordinal, first_line, last_line, after_compaction, before_compaction FROM session_segments WHERE archive_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		t.Fatal(err)
	}
	type seg struct {
		ordinal, first, last int
		after, before        *int
	}
	var segs []seg
	for rows.Next() {
		var g seg
		if err := rows.Scan(&g.ordinal, &g.first, &g.last, &g.after, &g.before); err != nil {
			t.Fatal(err)
		}
		segs = append(segs, g)
	}
	rows.Close()
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if segs[0].after != nil || segs[0].before == nil || *segs[0].before != 0 {
		t.Errorf("segment 0 links = %v / %v, want none / 0", segs[0].after, segs[0].before)
	}
	if segs[1].after == nil || *segs[1].after != 0 || segs[1].before != nil {
		t.Errorf("segment 1 links = %v / %v, want 0 / none", segs[1].after, segs[1].before)
	}

	// The tool call, with an input that carried a NUL: Postgres cannot hold
	// one in JSONB, and one such input must not lose the whole upload.
	var name, command string
	var hasResult bool
	var segment *int
	if err := pool.QueryRow(ctx, `SELECT name, input->>'command', has_result, segment FROM session_tool_calls WHERE archive_id = $1`, id).
		Scan(&name, &command, &hasResult, &segment); err != nil {
		t.Fatal(err)
	}
	if name != "Bash" || command != "printf 'ab'" || !hasResult || segment == nil || *segment != 0 {
		t.Errorf("tool call = %q %q result %v segment %v", name, command, hasResult, segment)
	}
}

// A resumed session uploads again. There is still one archive row, with its
// first upload time kept, and the derived rows are those of the new parse
// alone -- not the old ones with the new ones added.
func TestSaveArchive_AResumedSessionReplacesItsDerivedRows(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()
	a := memstore.SessionArchive{SessionID: archiveSession, Path: "p", SHA256: "one"}

	first, err := ss.SaveArchive(ctx, a, archiveTranscript(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE session_archives SET first_uploaded_at = '2026-01-01', last_uploaded_at = '2026-01-01', removed_at = '2026-02-01' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	a.SHA256 = "two"
	second, err := ss.SaveArchive(ctx, a, archiveTranscript(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("resumed session got a new archive row (%d, was %d)", second, first)
	}

	var archives, segments, tools, compactions int
	var sha string
	var keptFirst, movedLast, restored bool
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_archives`).Scan(&archives); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT sha256, compactions, first_uploaded_at = '2026-01-01', last_uploaded_at > '2026-01-01', removed_at IS NULL FROM session_archives WHERE id = $1`, first).
		Scan(&sha, &compactions, &keptFirst, &movedLast, &restored); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM session_segments WHERE archive_id = $1`, first).Scan(&segments)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM session_tool_calls WHERE archive_id = $1`, first).Scan(&tools)
	if archives != 1 || sha != "two" || compactions != 2 {
		t.Errorf("archives = %d sha = %q compactions = %d", archives, sha, compactions)
	}
	if !keptFirst || !movedLast {
		t.Errorf("upload times: first kept %v, last moved %v", keptFirst, movedLast)
	}
	if !restored {
		t.Error("an upload after the file was removed left the row marked removed")
	}
	if segments != 3 || tools != 1 {
		t.Errorf("segments = %d tools = %d, want 3 and 1", segments, tools)
	}
}

// The archive is per user: a store with no user cannot say whose file it is.
func TestSaveArchive_NeedsAUser(t *testing.T) {
	ss, _ := newTestSessionStore(t)
	if _, err := ss.ServiceScope().SaveArchive(context.Background(), memstore.SessionArchive{SessionID: archiveSession}, archiveTranscript(t, false)); err == nil {
		t.Error("SaveArchive at service scope succeeded")
	}
}
