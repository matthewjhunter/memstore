package pgstore_test

import (
	"context"
	"testing"

	"github.com/matthewjhunter/memstore/pgstore"
)

// The valuable part is the merge: three spellings of one topic, each holding
// one fact, become one subject holding three. Slugifying alone only tidies
// names; folding them is what makes the subject a usable grouping key.
func TestNormalizeSubjects_MergesSpellings(t *testing.T) {
	const ns = "normmerge"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	legacySubject(t, mustInsert(t, store, "first spelling", "p1"), "BIORce Role")
	legacySubject(t, mustInsert(t, store, "second spelling", "p2"), "Biorce Role")
	legacySubject(t, mustInsert(t, store, "third spelling", "p3"), "Biorce role")
	mustInsert(t, store, "already fine", "memstore")

	rep, err := pgstore.NormalizeSubjects(ctx, lintPool(t), ns, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied {
		t.Error("a dry run reported itself as applied")
	}
	if rep.Facts != 3 || len(rep.Renames) != 3 {
		t.Fatalf("renames = %+v (%d facts), want all three spellings", rep.Renames, rep.Facts)
	}
	for _, r := range rep.Renames {
		if r.To != "biorce-role" {
			t.Errorf("%q -> %q, want biorce-role", r.From, r.To)
		}
	}
	// Nothing was written by a dry run.
	after, err := pgstore.NormalizeSubjects(ctx, lintPool(t), ns, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Renames) != len(rep.Renames) {
		t.Errorf("a dry run changed the corpus: %d renames then %d", len(rep.Renames), len(after.Renames))
	}
}

// Applying must clear the affected vectors. The subject is rendered into the
// embedded text, so a renamed fact whose vector survived would be retrievable
// under a name it no longer carries.
func TestNormalizeSubjects_ApplyClearsVectorsAndIsIdempotent(t *testing.T) {
	const ns = "normapply"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	id := mustInsert(t, store, "a fact about a castle", "placeholder")
	legacySubject(t, id, "Falkenstein Castle")
	pool := lintPool(t)
	if _, err := pool.Exec(ctx, `UPDATE memstore_facts SET embedding = $1 WHERE id = $2`,
		"[1,2,3,4]", id); err != nil {
		t.Fatal(err)
	}

	rep, err := pgstore.NormalizeSubjects(ctx, pool, ns, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.Cleared != 1 {
		t.Errorf("report = %+v, want applied with 1 vector cleared", rep)
	}

	var subject string
	var hasVec bool
	if err := pool.QueryRow(ctx,
		`SELECT subject, embedding IS NOT NULL FROM memstore_facts WHERE id = $1`, id).
		Scan(&subject, &hasVec); err != nil {
		t.Fatal(err)
	}
	if subject != "falkenstein-castle" {
		t.Errorf("subject = %q, want falkenstein-castle", subject)
	}
	if hasVec {
		t.Error("the vector survived the rename; the fact is indexed under its old subject")
	}

	// A second run has nothing to do -- otherwise every run would look like
	// it found work and re-embedding would never settle.
	again, err := pgstore.NormalizeSubjects(ctx, pool, ns, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Renames) != 0 || again.Cleared != 0 {
		t.Errorf("second run = %+v, want no work", again)
	}
}

// A subject with nothing usable left is reported and left alone. Storing ""
// would trade a malformed subject for a missing one.
func TestNormalizeSubjects_SkipsUnsalvageable(t *testing.T) {
	const ns = "normskip"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	id := mustInsert(t, store, "punctuation only", "placeholder")
	legacySubject(t, id, "!!!")

	rep, err := pgstore.NormalizeSubjects(ctx, lintPool(t), ns, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "!!!" {
		t.Errorf("skipped = %v, want the unsalvageable subject", rep.Skipped)
	}
	var subject string
	if err := lintPool(t).QueryRow(ctx, `SELECT subject FROM memstore_facts WHERE id = $1`, id).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "!!!" {
		t.Errorf("subject = %q, want it left untouched", subject)
	}
}
