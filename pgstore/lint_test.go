package pgstore_test

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// Lint has to count the whole corpus and show only a sample. A check that
// matches three thousand facts must say three thousand and print ten;
// reporting ten as the total is how a backlog stays invisible.
func TestLint_CountsAllButSamples(t *testing.T) {
	const ns = "lintcount"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	for range 7 {
		mustInsert(t, store, "orphan "+time.Now().String(), "lint")
	}

	rep, err := pgstore.Lint(ctx, lintPool(t), ns, memstore.LintOpts{
		Kinds: []memstore.LintKind{memstore.LintOrphan}, SampleLimit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Count(memstore.LintOrphan); got != 7 {
		t.Errorf("orphan count = %d, want all 7", got)
	}
	if len(rep.Findings) != 3 {
		t.Errorf("listed %d findings, want the 3-item sample", len(rep.Findings))
	}
}

// A linked fact is not an orphan, in either direction: being pointed at
// counts as much as pointing.
func TestLint_OrphansAreUnlinkedInBothDirections(t *testing.T) {
	const ns = "lintorphan"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	src := mustInsert(t, store, "the source", "lint")
	dst := mustInsert(t, store, "the target", "lint")
	lone := mustInsert(t, store, "nobody links to me", "lint")
	if _, err := store.LinkFacts(ctx, src, dst, "reference", false, "", nil); err != nil {
		t.Fatal(err)
	}

	rep, err := pgstore.Lint(ctx, lintPool(t), ns, memstore.LintOpts{Kinds: []memstore.LintKind{memstore.LintOrphan}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(memstore.LintOrphan) != 1 {
		t.Fatalf("orphans = %d, want only the unlinked one: %+v", rep.Count(memstore.LintOrphan), rep.Findings)
	}
	if rep.Findings[0].FactID != lone {
		t.Errorf("orphan = %d, want %d (%d and %d are linked)", rep.Findings[0].FactID, lone, src, dst)
	}
}

// The subject convention is a lowercase entity name. Real subjects carry
// dots, slashes and colons, so the check must pass those and catch what the
// extraction artifacts actually had: capitals and spaces.
func TestLint_OddSubject(t *testing.T) {
	const ns = "lintsubject"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	ok := []string{"memstore", "jane-austen", "speculativefiction.org", "infodancer/oidclient", "gemma3:12b"}
	bad := []string{"Version control action", "PR branch", "Candidate", "Falkenstein Castle"}
	for _, s := range ok {
		mustInsert(t, store, "fine "+s, s)
	}
	for _, s := range bad {
		legacySubject(t, mustInsert(t, store, "flagged "+s, "placeholder"), s)
	}

	rep, err := pgstore.Lint(ctx, lintPool(t), ns, memstore.LintOpts{Kinds: []memstore.LintKind{memstore.LintOddSubject}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Count(memstore.LintOddSubject); got != len(bad) {
		t.Errorf("odd subjects = %d, want %d; findings %+v", got, len(bad), rep.Findings)
	}
	for _, f := range rep.Findings {
		for _, good := range ok {
			if f.Subject == good {
				t.Errorf("flagged %q, which follows the convention", good)
			}
		}
	}
}

// Duplicates flag the later copy and leave the original alone -- the report
// is a list of things to remove, and removing both is not the intent.
func TestLint_DuplicateKeepsTheFirst(t *testing.T) {
	const ns = "lintdupe"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	const text = "The base directory for the skill is /home/matthew/.claude/skills/osg-session-notes."
	first := mustInsert(t, store, text, "skill-a")
	second := mustInsert(t, store, text, "skill-b")
	mustInsert(t, store, "something else entirely", "skill-c")

	rep, err := pgstore.Lint(ctx, lintPool(t), ns, memstore.LintOpts{Kinds: []memstore.LintKind{memstore.LintDuplicate}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(memstore.LintDuplicate) != 1 {
		t.Fatalf("duplicates = %d, want 1: %+v", rep.Count(memstore.LintDuplicate), rep.Findings)
	}
	if rep.Findings[0].FactID != second {
		t.Errorf("flagged %d, want the later copy %d (original %d stays)", rep.Findings[0].FactID, second, first)
	}
}

// A fact stored an hour ago has not had its chance to be retrieved yet.
func TestLint_NeverSurfacedRespectsMinAge(t *testing.T) {
	const ns = "lintfresh"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	mustInsert(t, store, "stored just now", "lint")

	rep, err := pgstore.Lint(ctx, lintPool(t), ns, memstore.LintOpts{
		Kinds: []memstore.LintKind{memstore.LintNeverSurfaced}, MinAge: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Count(memstore.LintNeverSurfaced); got != 0 {
		t.Errorf("never-surfaced = %d, want 0 -- the fact is minutes old", got)
	}

	// With no age floor the same fact is in scope.
	rep, err = pgstore.Lint(ctx, lintPool(t), ns, memstore.LintOpts{Kinds: []memstore.LintKind{memstore.LintNeverSurfaced}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Count(memstore.LintNeverSurfaced); got != 1 {
		t.Errorf("never-surfaced with no floor = %d, want 1", got)
	}
}

// lintPool opens a pool on the same database the test store uses; Lint takes
// a pool and namespace, matching the other admin-facing aggregates.
func lintPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// An empty subject is a different defect from a badly-shaped one: the V4
// identity migration blanked subjects that only carried ownership, so these
// are facts waiting for a topic, not facts with a wrong one. Reporting them
// together would put the migration's own output in the same bucket as
// extraction artifacts.
func TestLint_MissingSubjectIsNotOddSubject(t *testing.T) {
	const ns = "lintnosubject"
	store := newTestStoreNS(t, ns)
	blank := mustInsert(t, store, "a fact the migration blanked", "")
	odd := mustInsert(t, store, "an extraction artifact", "placeholder")
	legacySubject(t, odd, "Version control action")
	mustInsert(t, store, "a well-formed fact", "memstore")

	rep, err := pgstore.Lint(context.Background(), lintPool(t), ns, memstore.LintOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(memstore.LintMissingSubject) != 1 || rep.Count(memstore.LintOddSubject) != 1 {
		t.Fatalf("missing=%d odd=%d, want 1 each (%d blank, %d odd)",
			rep.Count(memstore.LintMissingSubject), rep.Count(memstore.LintOddSubject), blank, odd)
	}
	for _, f := range rep.Findings {
		if f.Kind == memstore.LintOddSubject && f.FactID == blank {
			t.Error("the blank-subject fact was reported as odd-subject")
		}
	}
}

// legacySubject plants a subject the store would now refuse to write,
// simulating a fact stored before NormalizeStoredSubject became an
// invariant. That is exactly the population the lint and normalize commands
// exist to clean up, and with the invariant in place it is the only way to
// produce one.
func legacySubject(t *testing.T, id int64, subject string) {
	t.Helper()
	if _, err := lintPool(t).Exec(context.Background(),
		`UPDATE memstore_facts SET subject = $1 WHERE id = $2`, subject, id); err != nil {
		t.Fatal(err)
	}
}
