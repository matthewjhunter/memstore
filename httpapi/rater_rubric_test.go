package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// TestRateFactPromptTreatsUnreferencedAsNeutral pins decision D3 of
// docs/citation-feedback.md: "never referenced" is neutral, and -1 is reserved
// for a fact that was wrong or misleading.
func TestRateFactPromptTreatsUnreferencedAsNeutral(t *testing.T) {
	gen := &schemaGenerator{resp: `{"score": 0, "reason": "no evidence either way"}`}
	q := &ExtractQueue{generator: gen}
	if _, _, err := q.rateFact(context.Background(), "Prefer small, separate commits.", "[user]: fix the dice parser"); err != nil {
		t.Fatal(err)
	}
	p := gen.gotPrompt

	if strings.Contains(p, "or never referenced)") {
		t.Error("prompt still lists \"never referenced\" as a reason for -1")
	}
	for _, want := range []string{
		"0 =",                   // a neutral score exists
		"never referenced",      // and unreferenced facts are sent there
		"without being quoted",  // with the reason: conventions shape work unquoted
		"wrong or misleading",   // -1 is reserved for harm
		"When in doubt, rate 0", // doubt no longer pads the positives
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
	if !strings.Contains(p, "<untrusted-") {
		t.Error("prompt no longer fences the fact and the session excerpt")
	}
}

// A neutral answer comes back as neutral, and so does anything out of range:
// recording nothing is the safe default, and the old default of +1 on an
// unexpected value inflated the positives the same way "when in doubt, +1" did.
func TestRateFactScores(t *testing.T) {
	for resp, want := range map[string]int{
		`{"score": 1, "reason": "used"}`:      1,
		`{"score": -1, "reason": "misled"}`:   -1,
		`{"score": 0, "reason": "no signal"}`: 0,
		`{"score": 5, "reason": "odd"}`:       0,
	} {
		q := &ExtractQueue{generator: &scoringGenerator{resp: resp}}
		got, _, err := q.rateFact(context.Background(), "a fact", "[user]: some work")
		if err != nil {
			t.Fatalf("%s: %v", resp, err)
		}
		if got != want {
			t.Errorf("%s: score %d, want %d", resp, got, want)
		}
	}
}

// A neutral score must not be recorded: a stored 0 would still count toward the
// fact's weight in recall's feedback multiplier and dilute real signal.
func TestAutoRateFacts_NeutralIsNotRecorded(t *testing.T) {
	rater := &fakeHintRater{factIDs: []int64{42}}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		42: {ID: 42, Content: "Prefer small, separate commits."},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `{"score": 0, "reason": "no evidence either way"}`},
		rater:     rater,
		store:     store,
	}
	q.autoRateFacts(context.Background(), extractJob{
		SessionID: "sess-neutral",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "fix the dice parser"}},
	})
	if len(rater.feedback) != 0 {
		t.Errorf("recorded %d rating(s) for a neutral score: %+v", len(rater.feedback), rater.feedback)
	}
}

// The backfill path rates through rateFact too, and must skip a neutral score
// the same way -- or a backfill run would write the zeros the live path avoids.
func TestBackfillFeedback_NeutralIsNotRecorded(t *testing.T) {
	rater := &fakeBackfillRater{
		fakeHintRater: fakeHintRater{factIDs: []int64{42}},
		sessions: map[string][]memstore.SessionTurn{
			"sess-neutral": {{Role: "user", Content: "working on the osg dice parser"}},
		},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		42: {ID: 42, Content: "Prefer small, separate commits."},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `{"score": 0, "reason": "no evidence either way"}`},
		rater:     rater,
		store:     store,
	}
	result, err := q.BackfillFeedback(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", result.Sessions)
	}
	if result.Rated != 0 || len(rater.feedback) != 0 {
		t.Errorf("backfill recorded %d rating(s) (Rated=%d) for a neutral score", len(rater.feedback), result.Rated)
	}
}
