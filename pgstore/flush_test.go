package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// Flush deletes user data, so these pin what it may and may not touch: the
// age and use rule, every exclusion, the backup, and a delete that re-checks
// the rule rather than trusting a plan made earlier.

var flushNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// flushOpts is the rule the issue proposes: untouched for 90 days and never
// used.
func flushOpts() FlushOpts {
	return FlushOpts{OlderThan: 90 * 24 * time.Hour, MaxUses: 1, Now: flushNow}
}

// newFlushStore is a store with the session store's tables beside it, since
// exposures recorded there count as uses.
func newFlushStore(t *testing.T) *PostgresStore {
	t.Helper()
	s := newANNStore(t)
	if _, err := NewSessionStore(context.Background(), s.pool); err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return s
}

type agedFact struct {
	content, subject, category, kind, subsystem string
	metadata                                    string
	daysAgo                                     int
}

func insertAged(t *testing.T, s *PostgresStore, f agedFact) int64 {
	t.Helper()
	fact := memstore.Fact{
		Content:   f.content,
		Subject:   f.subject,
		Category:  "note",
		Kind:      f.kind,
		Subsystem: f.subsystem,
		CreatedAt: flushNow.AddDate(0, 0, -f.daysAgo),
	}
	if fact.Subject == "" {
		fact.Subject = "flush"
	}
	if f.category != "" {
		fact.Category = f.category
	}
	if f.metadata != "" {
		fact.Metadata = json.RawMessage(f.metadata)
	}
	id, err := s.Insert(context.Background(), fact)
	if err != nil {
		t.Fatalf("Insert %q: %v", f.content, err)
	}
	return id
}

func stale(t *testing.T, s *PostgresStore, content string) int64 {
	t.Helper()
	return insertAged(t, s, agedFact{content: content, daysAgo: 200})
}

func execSQL(t *testing.T, s *PostgresStore, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// claimFileTrigger records ids as shown by a file trigger, the way
// eval-triggers does: the trigger's own id and the ids of what it loaded.
func claimFileTrigger(t *testing.T, s *PostgresStore, ids ...int64) {
	t.Helper()
	ctx := context.Background()
	sess, err := NewSessionStore(ctx, s.pool)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := sess.ForUser(s.userID)
	if err != nil {
		t.Fatal(err)
	}
	claimer, ok := scoped.(memstore.InjectionClaimer)
	if !ok {
		t.Fatal("the scoped session store cannot claim injections")
	}
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = strconv.FormatInt(id, 10)
	}
	if _, err := claimer.ClaimInjections(ctx, "sess-1", memstore.ChannelFileTrigger, memstore.RefTypeFact, refs); err != nil {
		t.Fatalf("ClaimInjections: %v", err)
	}
}

func plan(t *testing.T, s *PostgresStore, opts FlushOpts) FlushPlan {
	t.Helper()
	p, err := PlanFlush(context.Background(), s.pool, s.namespace, opts)
	if err != nil {
		t.Fatalf("PlanFlush: %v", err)
	}
	return p
}

func assertIDs(t *testing.T, label string, got []int64, want ...int64) {
	t.Helper()
	got = slices.Sorted(slices.Values(got))
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("%s: ids %v, want %v", label, got, want)
	}
}

func TestPlanFlush_AgeAndUses(t *testing.T) {
	s := newFlushStore(t)
	ctx := context.Background()

	candidate := stale(t, s, "stale and never used")

	searched := stale(t, s, "old, returned by one search")
	execSQL(t, s, `UPDATE memstore_facts SET use_count = 1, last_used_at = $2 WHERE id = $1`, searched, flushNow.AddDate(0, 0, -150))

	injected := stale(t, s, "old, injected by recall once")
	execSQL(t, s, `UPDATE memstore_facts SET inject_count = 1, last_injected_at = $2 WHERE id = $1`, injected, flushNow.AddDate(0, 0, -150))

	insertAged(t, s, agedFact{content: "young and never used", daysAgo: 10})

	// Shown by a file trigger: that channel moves no counter on the fact, only
	// a context_injections row, and it is still a use.
	triggered := stale(t, s, "old, shown by a file trigger")
	claimFileTrigger(t, s, triggered)

	// A superseded fact is history, not a candidate, however old.
	old := stale(t, s, "the superseded version")
	successor := stale(t, s, "the version that replaced it")
	if err := s.Supersede(ctx, old, successor); err != nil {
		t.Fatal(err)
	}

	p := plan(t, s, flushOpts())
	assertIDs(t, "candidates", p.IDs, candidate)
	if p.Excluded[FlushSupersedesAnother] != 1 {
		t.Errorf("supersedes_another excluded %d, want 1 (the successor)", p.Excluded[FlushSupersedesAnother])
	}
	if p.Matched != len(p.IDs)+sumExcluded(p) {
		t.Errorf("matched %d, want candidates plus exclusions (%d)", p.Matched, len(p.IDs)+sumExcluded(p))
	}
}

// Age is measured from the last time anything touched the fact, not from its
// creation: a fact written long ago and used last week is live.
func TestPlanFlush_AgeRunsFromLastTouch(t *testing.T) {
	s := newFlushStore(t)
	longIdle := stale(t, s, "used once, long ago")
	execSQL(t, s, `UPDATE memstore_facts SET use_count = 1, last_used_at = $2 WHERE id = $1`, longIdle, flushNow.AddDate(0, 0, -150))
	recent := stale(t, s, "used once, last week")
	execSQL(t, s, `UPDATE memstore_facts SET use_count = 1, last_used_at = $2 WHERE id = $1`, recent, flushNow.AddDate(0, 0, -7))

	opts := flushOpts()
	opts.MaxUses = 2
	assertIDs(t, "candidates", plan(t, s, opts).IDs, longIdle)
}

// Nothing is exempt for what it is -- not a kind, not a category. What is kept
// is kept for something recorded about it: a persistent mark, an open task,
// a supersession, an explicit link, or a trigger that loads it.
func TestPlanFlush_Exclusions(t *testing.T) {
	s := newFlushStore(t)
	ctx := context.Background()

	invariant := insertAged(t, s, agedFact{content: "an invariant nobody used", kind: "invariant", daysAgo: 200})
	identity := insertAged(t, s, agedFact{content: "an identity fact nobody used", category: "identity", daysAgo: 200})
	done := insertAged(t, s, agedFact{content: "a finished task", kind: "task", daysAgo: 200, metadata: `{"status":"completed"}`})

	insertAged(t, s, agedFact{content: "marked persistent", daysAgo: 200, metadata: `{"persistent":true}`})
	insertAged(t, s, agedFact{content: "a pending task", kind: "task", daysAgo: 200, metadata: `{"status":"pending"}`})
	insertAged(t, s, agedFact{content: "a task with no status", kind: "task", daysAgo: 200})

	// An explicit link is someone's statement about two facts; both ends stay.
	src := stale(t, s, "explicitly linked, source")
	dst := stale(t, s, "explicitly linked, target")
	if _, err := s.LinkFacts(ctx, src, dst, "reference", false, "", nil); err != nil {
		t.Fatal(err)
	}
	// A related link is the similarity linker's, rebuilt on demand; it
	// protects nothing, or it would protect nearly every fact.
	relA := stale(t, s, "auto-linked, one end")
	relB := stale(t, s, "auto-linked, other end")
	if _, err := s.LinkFacts(ctx, relA, relB, "related", true, "", nil); err != nil {
		t.Fatal(err)
	}

	// Facts an active trigger loads are live whether or not anything counted
	// the loads; trigger exposures were not recorded before #230. The
	// triggers themselves have never fired, so they are candidates.
	trigSub := insertAged(t, s, agedFact{content: "trigger by subsystem", kind: "trigger", daysAgo: 200,
		metadata: `{"file_pattern":"*.go","load_subsystem":"search"}`})
	trigSubj := insertAged(t, s, agedFact{content: "trigger by subject", kind: "trigger", daysAgo: 200,
		metadata: `{"file_pattern":"*.yml","load_subject":"homelab"}`})
	insertAged(t, s, agedFact{content: "loaded by subsystem", subsystem: "search", daysAgo: 200})
	insertAged(t, s, agedFact{content: "loaded by subject", subject: "homelab", daysAgo: 200})

	p := plan(t, s, flushOpts())
	assertIDs(t, "candidates", p.IDs, invariant, identity, done, relA, relB, trigSub, trigSubj)
	want := map[FlushExclusion]int{
		FlushPersistent:    1,
		FlushOpenTask:      2,
		FlushExplicitLink:  2,
		FlushTriggerLoaded: 2,
	}
	for reason, n := range want {
		if p.Excluded[reason] != n {
			t.Errorf("%s excluded %d, want %d", reason, p.Excluded[reason], n)
		}
	}
}

// A trigger that fires is in use: eval-triggers claims the trigger's own id on
// the file-trigger channel, and that counts. One that never fires ages out.
func TestPlanFlush_TriggerFiringCountsAsUse(t *testing.T) {
	s := newFlushStore(t)
	fired := insertAged(t, s, agedFact{content: "a trigger that fired", kind: "trigger", daysAgo: 200,
		metadata: `{"file_pattern":"*.go","load_subsystem":"storage"}`})
	idle := insertAged(t, s, agedFact{content: "a trigger that never fires", kind: "trigger", daysAgo: 200,
		metadata: `{"file_pattern":"*.cobol","load_subsystem":"mainframe"}`})
	claimFileTrigger(t, s, fired)

	assertIDs(t, "candidates", plan(t, s, flushOpts()).IDs, idle)
}

func TestPlanFlush_Filters(t *testing.T) {
	s := newFlushStore(t)
	a := insertAged(t, s, agedFact{content: "breadcrumb one", subject: "breadcrumbs", daysAgo: 200})
	insertAged(t, s, agedFact{content: "something else", subject: "other", daysAgo: 200})
	summary := insertAged(t, s, agedFact{content: "a summary", subject: "other", kind: "summary", daysAgo: 200})

	opts := flushOpts()
	opts.Subject = "breadcrumbs"
	assertIDs(t, "subject filter", plan(t, s, opts).IDs, a)

	opts = flushOpts()
	opts.Kind = "summary"
	assertIDs(t, "kind filter", plan(t, s, opts).IDs, summary)
}

func TestPlanFlush_StaysInItsNamespace(t *testing.T) {
	s := newFlushStore(t)
	mine := stale(t, s, "stale in test")
	other, err := New(context.Background(), s.pool, nil, "other", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	stale(t, other, "stale in other")
	assertIDs(t, "test namespace", plan(t, s, flushOpts()).IDs, mine)
}

// The backup holds every row the delete removes, cascades included, as the
// rows themselves: chunks and links go with the fact, so a restore from the
// facts alone would lose them.
func TestWriteFlushBackup(t *testing.T) {
	s := newFlushStore(t)
	ctx := context.Background()
	doomed := stale(t, s, "stale with a chunk and a link")
	kept := insertAged(t, s, agedFact{content: "young neighbour", daysAgo: 1})
	if err := s.SetFactVectors(ctx, doomed, memstore.FactVectors{Whole: []float32{1, 0, 0, 0},
		Chunks: []memstore.FactChunk{{Vector: []float32{1, 0, 0, 0}, ByteEnd: 5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkFacts(ctx, doomed, kept, "related", true, "", nil); err != nil {
		t.Fatal(err)
	}

	p := plan(t, s, flushOpts())
	assertIDs(t, "candidates", p.IDs, doomed)
	var buf bytes.Buffer
	if err := WriteFlushBackup(ctx, s.pool, p.IDs, &buf); err != nil {
		t.Fatalf("WriteFlushBackup: %v", err)
	}
	var backup struct {
		Facts  []map[string]any `json:"facts"`
		Chunks []map[string]any `json:"chunks"`
		Links  []map[string]any `json:"links"`
	}
	if err := json.Unmarshal(buf.Bytes(), &backup); err != nil {
		t.Fatalf("backup is not JSON: %v\n%s", err, buf.String())
	}
	if len(backup.Facts) != 1 || backup.Facts[0]["content"] != "stale with a chunk and a link" {
		t.Errorf("backup facts = %v, want the one fact with its content", backup.Facts)
	}
	if len(backup.Chunks) != 1 {
		t.Errorf("backup chunks = %d, want 1", len(backup.Chunks))
	}
	if len(backup.Links) != 1 {
		t.Errorf("backup links = %d, want the link to the kept fact", len(backup.Links))
	}
}

// The delete re-checks the rule: a fact used or marked persistent between the
// plan and the delete is kept. The rest go, and their chunks and links with
// them.
func TestExecuteFlush(t *testing.T) {
	s := newFlushStore(t)
	ctx := context.Background()
	doomed := stale(t, s, "stale, deleted")
	rescued := stale(t, s, "stale, then searched before the delete")
	marked := stale(t, s, "stale, then marked persistent before the delete")
	kept := insertAged(t, s, agedFact{content: "young, kept", daysAgo: 1})
	if err := s.SetFactVectors(ctx, doomed, memstore.FactVectors{Whole: []float32{1, 0, 0, 0},
		Chunks: []memstore.FactChunk{{Vector: []float32{1, 0, 0, 0}, ByteEnd: 5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkFacts(ctx, doomed, kept, "related", true, "", nil); err != nil {
		t.Fatal(err)
	}

	opts := flushOpts()
	p := plan(t, s, opts)
	assertIDs(t, "candidates", p.IDs, doomed, rescued, marked)
	execSQL(t, s, `UPDATE memstore_facts SET use_count = 1, last_used_at = $2 WHERE id = $1`, rescued, flushNow)
	if err := s.UpdateMetadata(ctx, marked, map[string]any{memstore.MetaPersistent: true}); err != nil {
		t.Fatal(err)
	}

	deleted, err := ExecuteFlush(ctx, s.pool, s.namespace, opts, p.IDs)
	if err != nil {
		t.Fatalf("ExecuteFlush: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d, want 1", deleted)
	}
	for _, c := range []struct {
		id   int64
		gone bool
	}{{doomed, true}, {rescued, false}, {marked, false}, {kept, false}} {
		f, err := s.Get(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if (f == nil) != c.gone {
			t.Errorf("fact %d: gone=%v, want %v", c.id, f == nil, c.gone)
		}
	}
	var rows int
	if err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM memstore_fact_chunks WHERE fact_id = $1)
		      + (SELECT count(*) FROM memstore_links WHERE source_id = $1 OR target_id = $1)`, doomed).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d chunk and link rows left for the deleted fact, want 0", rows)
	}
}

func sumExcluded(p FlushPlan) int {
	n := 0
	for _, v := range p.Excluded {
		n += v
	}
	return n
}
