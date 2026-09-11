package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// Recording is idempotent per user, session and fact, so a transcript that is
// uploaded again -- a resumed session reuses its id -- adds nothing twice.
func TestRecordCitations(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cites := []memstore.FactCitation{
		{SessionID: "s1", FactID: 5, TurnUUID: "a1", CitedAt: now},
		{SessionID: "s1", FactID: 6, TurnUUID: "a1", CitedAt: now},
	}

	n, err := ss.RecordCitations(ctx, cites)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("first record: %d new, want 2", n)
	}
	n, err = ss.RecordCitations(ctx, cites[:1])
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("repeat record: %d new, want 0", n)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fact_citations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("fact_citations holds %d rows, want 2", rows)
	}

	if n, err := ss.RecordCitations(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty record: %d, %v", n, err)
	}
}

// The backfill lists only sessions whose assistant turns contain the form.
func TestCitingSessions(t *testing.T) {
	ss, _ := newTestSessionStore(t)
	ctx := context.Background()
	for sid, turn := range map[string]memstore.SessionTurn{
		"cites":     {UUID: "a", Role: "assistant", Content: "per [fact 5], yes"},
		"quiet":     {UUID: "b", Role: "assistant", Content: "no citations here"},
		"user-only": {UUID: "c", Role: "user", Content: "what about [fact 5]?"},
	} {
		turn.SessionID = sid
		if err := ss.SaveTurns(ctx, sid, []memstore.SessionTurn{turn}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ss.CitingSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "cites" {
		t.Errorf("CitingSessions = %v, want [cites]", got)
	}
}
