package pgstore_test

import (
	"context"
	"math"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// TestFeedbackScores_DecayWithAge pins #161's fix: a rating's weight halves
// every FeedbackHalfLife, so ratings from a window that ended months ago fade
// toward neutral instead of holding a fact down forever.
func TestFeedbackScores_DecayWithAge(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()

	for _, fb := range []memstore.ContextFeedback{
		{RefID: "42", RefType: memstore.RefTypeFact, SessionID: "fresh", Score: 1},
		{RefID: "42", RefType: memstore.RefTypeFact, SessionID: "old", Score: -1},
	} {
		if err := ss.RecordFeedback(ctx, fb); err != nil {
			t.Fatal(err)
		}
	}
	// Two half-lives old: the rating carries a quarter of a fresh one's weight.
	age := 2 * memstore.FeedbackHalfLife
	if _, err := pool.Exec(ctx,
		`UPDATE context_feedback SET created_at = now() - make_interval(secs => $1) WHERE session_id = 'old'`,
		age.Seconds()); err != nil {
		t.Fatal(err)
	}

	stats, err := ss.FeedbackScores(ctx, []string{"42"}, memstore.RefTypeFact)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := stats["42"]
	if !ok {
		t.Fatalf("no stats for ref 42: %v", stats)
	}
	if st.Count != 2 {
		t.Errorf("Count = %d, want 2: the raw count is not decayed", st.Count)
	}
	if math.Abs(st.Weight-1.25) > 0.01 {
		t.Errorf("Weight = %.3f, want 1.25 (1 fresh + 0.25 at two half-lives)", st.Weight)
	}
	// (1*1 + -1*0.25) / 1.25
	if math.Abs(st.Avg-0.6) > 0.01 {
		t.Errorf("Avg = %.3f, want 0.6, the age-weighted mean", st.Avg)
	}
}
