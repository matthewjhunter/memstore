package pgstore_test

import (
	"context"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// setVec gives a fact a known vector so similarity is arithmetic rather than
// whatever a mock embedder happened to produce. teststore's schema is
// vector(4); cosine similarity is 1 - (a <=> b).
func setVec(t *testing.T, id int64, vec string) {
	t.Helper()
	if _, err := lintPool(t).Exec(context.Background(),
		`UPDATE memstore_facts SET embedding = $1 WHERE id = $2`, vec, id); err != nil {
		t.Fatal(err)
	}
}

// The gate decides, and it decides on the current policy rather than on
// whatever was configured when a fact was stored -- that gap is the whole
// reason this pass exists.
func TestBackfillLinks_GateSelectsPairs(t *testing.T) {
	const ns = "bflgate"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	a := mustInsert(t, store, "anchor", "topic")
	near := mustInsert(t, store, "near neighbour", "topic")
	far := mustInsert(t, store, "unrelated", "topic")
	setVec(t, a, "[1,0,0,0]")
	setVec(t, near, "[0.8,0.6,0,0]") // cosine 0.8
	setVec(t, far, "[0,0,1,0]")      // orthogonal to both of the others

	rep, err := pgstore.BackfillLinks(ctx, lintPool(t), ns, pgstore.BackfillLinksOpts{MinSim: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Facts != 3 || rep.NoVector != 0 {
		t.Errorf("facts=%d noVector=%d, want 3 and 0", rep.Facts, rep.NoVector)
	}
	if rep.Candidates != 1 {
		t.Fatalf("candidates = %d (%+v), want only the 0.8 pair", rep.Candidates, rep.Sample)
	}
	// Candidates are normalized to (lower id, higher id), so the pair has
	// one spelling and the assertion can say so.
	got := rep.Sample[0]
	if got.SourceID != min(a, near) || got.TargetID != max(a, near) {
		t.Errorf("pair = %d-%d, want %d-%d; %d is unrelated", got.SourceID, got.TargetID, a, near, far)
	}
	if rep.Applied {
		t.Error("a dry run reported itself applied")
	}
}

// Links are written bidirectional, so a->b and b->a are one edge. Writing
// both would double the graph and make every rerun look like it had work.
func TestBackfillLinks_ApplyIsSymmetricAndIdempotent(t *testing.T) {
	const ns = "bflapply"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	a := mustInsert(t, store, "one side", "topic")
	b := mustInsert(t, store, "other side", "topic")
	setVec(t, a, "[1,0,0,0]")
	setVec(t, b, "[0.9,0.435889894,0,0]") // cosine ~0.9

	pool := lintPool(t)
	rep, err := pgstore.BackfillLinks(ctx, pool, ns, pgstore.BackfillLinksOpts{MinSim: 0.5, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 1 {
		t.Fatalf("added = %d, want exactly one edge for the pair", rep.Added)
	}

	var links int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memstore_links WHERE namespace = $1`, ns).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Errorf("stored %d links, want 1 -- a->b and b->a are the same edge", links)
	}
	var linkType string
	var bidi bool
	if err := pool.QueryRow(ctx,
		`SELECT link_type, bidirectional FROM memstore_links WHERE namespace = $1`, ns).Scan(&linkType, &bidi); err != nil {
		t.Fatal(err)
	}
	if linkType != "related" || !bidi {
		t.Errorf("link is %q bidirectional=%v, want the shape extraction writes", linkType, bidi)
	}

	// Rerunning finds nothing: the pair is already linked.
	again, err := pgstore.BackfillLinks(ctx, pool, ns, pgstore.BackfillLinksOpts{MinSim: 0.5, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Candidates != 0 || again.Added != 0 {
		t.Errorf("second run = %+v, want no work", again)
	}
}

// MaxPerFact mirrors extraction's cap of three per fact. Without it one
// dense cluster would absorb the whole budget of the run.
func TestBackfillLinks_RespectsPerFactCap(t *testing.T) {
	const ns = "bflcap"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	hub := mustInsert(t, store, "hub", "topic")
	setVec(t, hub, "[1,0,0,0]")
	for range 5 {
		id := mustInsert(t, store, "spoke "+ns, "topic")
		setVec(t, id, "[0.99,0.141067,0,0]") // ~0.99 to the hub and each other
	}

	rep, err := pgstore.BackfillLinks(ctx, lintPool(t), ns, pgstore.BackfillLinksOpts{
		MinSim: 0.5, MaxPerFact: 2, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added == 0 {
		t.Fatal("no links added")
	}
	var maxDeg int
	if err := lintPool(t).QueryRow(ctx, `
		SELECT coalesce(max(c), 0) FROM (
			SELECT id, count(*) AS c FROM (
				SELECT source_id AS id FROM memstore_links WHERE namespace = $1
				UNION ALL
				SELECT target_id FROM memstore_links WHERE namespace = $1
			) t GROUP BY id) u
	`, ns).Scan(&maxDeg); err != nil {
		t.Fatal(err)
	}
	if maxDeg > 2 {
		t.Errorf("a fact gained %d links, want at most MaxPerFact=2", maxDeg)
	}
}

// A fact with no vector cannot be compared; it is reported, not silently
// dropped, so an operator can tell "nothing similar" from "never embedded".
func TestBackfillLinks_ReportsFactsWithoutVectors(t *testing.T) {
	const ns = "bflnovec"
	store := newTestStoreNS(t, ns)
	id := mustInsert(t, store, "never embedded", "topic")
	_ = id

	rep, err := pgstore.BackfillLinks(context.Background(), lintPool(t), ns, pgstore.BackfillLinksOpts{MinSim: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if rep.NoVector != 1 || rep.Facts != 0 {
		t.Errorf("report = %+v, want 1 without a vector and 0 comparable", rep)
	}
}

// A fact stored through memory_store used to be born with no links and never
// acquire any: auto-linking lived only in the session extraction path. The
// embed queue calls this once a fact has a vector, which is the first moment
// there is anything to compare it against.
func TestLinkNeighbors_LinksJustEmbeddedFacts(t *testing.T) {
	const ns = "linkneighbors"
	store := newTestStoreNS(t, ns)
	ctx := context.Background()
	existing := mustInsert(t, store, "an established fact", "topic")
	fresh := mustInsert(t, store, "a fact just stored by hand", "topic")
	unrelated := mustInsert(t, store, "nothing to do with it", "topic")
	setVec(t, existing, "[1,0,0,0]")
	setVec(t, fresh, "[0.95,0.312249,0,0]") // ~0.95 to existing
	setVec(t, unrelated, "[0,0,1,0]")

	pol := memstore.SimilarityPolicy{LinkMinSim: 0.5}
	n, err := store.LinkNeighbors(ctx, []int64{fresh}, pol, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("linked %d pairs, want 1", n)
	}

	var src, dst int64
	if err := lintPool(t).QueryRow(ctx,
		`SELECT source_id, target_id FROM memstore_links WHERE namespace = $1`, ns).Scan(&src, &dst); err != nil {
		t.Fatal(err)
	}
	if src != min(existing, fresh) || dst != max(existing, fresh) {
		t.Errorf("linked %d-%d, want %d-%d; %d is unrelated", src, dst, existing, fresh, unrelated)
	}

	// Re-embedding a fact must not accumulate duplicates of edges it has.
	again, err := store.LinkNeighbors(ctx, []int64{fresh}, pol, 3)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("relinking added %d pairs, want 0 -- the edge already exists", again)
	}
}

// An empty batch is the common case on a quiet queue and must not query.
func TestLinkNeighbors_EmptyBatch(t *testing.T) {
	store := newTestStoreNS(t, "linkempty")
	n, err := store.LinkNeighbors(context.Background(), nil, memstore.SimilarityPolicy{LinkMinSim: 0.5}, 3)
	if err != nil || n != 0 {
		t.Errorf("n=%d err=%v, want 0 and no error", n, err)
	}
}
