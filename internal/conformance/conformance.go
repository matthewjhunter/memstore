// Package conformance provides a shared Store contract test suite that can be
// wired against any memstore.Store implementation. Each subtest receives a
// fresh store via the factory functions in Options, so backends are exercised
// identically without sharing state.
//
// Usage:
//
//	func TestConformance(t *testing.T) {
//	    conformance.Run(t, conformance.Options{
//	        NewStore:        func(t *testing.T) memstore.Store { ... },
//	        NewStoreNS:      func(t *testing.T, ns string) memstore.Store { ... }, // optional
//	        SetSupersededBy: func(t *testing.T, supersededByID, targetID int64) { ... }, // optional
//	    })
//	}
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// Options configures a conformance run. NewStore is required; the others are
// optional -- subtests that need them are skipped when they are nil.
type Options struct {
	// NewStore returns a fresh, empty store scoped to a test namespace. Called
	// once per subtest; the store need not be closed by the caller (use
	// t.Cleanup inside the factory).
	NewStore func(t *testing.T) memstore.Store

	// NewStoreNS is like NewStore but accepts an explicit namespace. Required
	// for the NamespaceIsolation subtest; if nil that subtest is skipped.
	NewStoreNS func(t *testing.T, namespace string) memstore.Store

	// SetSupersededBy forcibly sets superseded_by on the row with id=targetID
	// to supersededByID via a raw backend write, bypassing Store validation.
	// The wiring closure captures its own db handle and uses backend-native SQL
	// ($N placeholders for Postgres). Required for
	// HistoryCycleTerminates; if nil that subtest is skipped.
	SetSupersededBy func(t *testing.T, supersededByID, targetID int64)

	// NewTwoUserStores returns two stores over the SAME underlying database
	// and namespace, scoped to two DIFFERENT users. nil skips the
	// UserIsolation family (backends without per-user scoping).
	NewTwoUserStores func(t *testing.T) (memstore.Store, memstore.Store)

	// NewTwoUserSessionStores returns two SessionStores over the SAME
	// database, scoped to two DIFFERENT users. nil skips the
	// SessionIsolation family.
	NewTwoUserSessionStores func(t *testing.T) (memstore.SessionStore, memstore.SessionStore)
}

// Run executes the shared Store contract tests using the factories in opts.
func Run(t *testing.T, opts Options) {
	t.Helper()
	if opts.NewStore == nil {
		t.Fatal("conformance.Options.NewStore must not be nil")
	}

	t.Run("InsertGetRoundTrip", func(t *testing.T) {
		testInsertGetRoundTrip(t, opts.NewStore(t))
	})
	t.Run("ExistsAndDedup", func(t *testing.T) {
		testExistsAndDedup(t, opts.NewStore(t))
	})
	t.Run("BreakdownAggregates", func(t *testing.T) {
		testBreakdownAggregates(t, opts.NewStore(t))
	})
	t.Run("NotFoundSentinel", func(t *testing.T) {
		testNotFoundSentinel(t, opts.NewStore(t))
	})
	t.Run("CapabilityScoping", func(t *testing.T) {
		testCapabilityScoping(t, opts.NewStore(t))
	})
	t.Run("SupersedeAndHistory", func(t *testing.T) {
		testSupersedeAndHistory(t, opts.NewStore(t))
	})
	t.Run("HistoryCycleTerminates", func(t *testing.T) {
		if opts.SetSupersededBy == nil {
			t.Skip("SetSupersededBy not provided; skipping cycle test")
		}
		s := opts.NewStore(t)
		testHistoryCycleTerminates(t, s, opts.SetSupersededBy)
	})
	t.Run("InvalidMetadataFilterErrors", func(t *testing.T) {
		testInvalidMetadataFilterErrors(t, opts.NewStore(t))
	})
	t.Run("MetadataFilterMatches", func(t *testing.T) {
		testMetadataFilterMatches(t, opts.NewStore(t))
	})
	t.Run("NumericMetadataComparisonDivergence", func(t *testing.T) {
		testNumericMetadataComparisonDivergence(t, opts.NewStore(t))
	})
	t.Run("UpdateMetadataMergeSemantics", func(t *testing.T) {
		testUpdateMetadataMergeSemantics(t, opts.NewStore(t))
	})
	t.Run("EmbedQuarantine", func(t *testing.T) {
		testEmbedQuarantine(t, opts.NewStore(t))
	})
	t.Run("FactChunkRoundTrip", func(t *testing.T) {
		testFactChunkRoundTrip(t, opts.NewStore(t))
	})
	t.Run("FactChunksReplaceRatherThanMerge", func(t *testing.T) {
		testFactChunksReplaceRatherThanMerge(t, opts.NewStore(t))
	})
	t.Run("SearchRanksFactsByBestChunk", func(t *testing.T) {
		testSearchRanksFactsByBestChunk(t, opts.NewStore(t))
	})
	t.Run("FactMarkerIsTheWholeFactVector", func(t *testing.T) {
		testFactMarkerIsTheWholeFactVector(t, opts.NewStore(t))
	})
	t.Run("EmbedFactsProducesChunks", func(t *testing.T) {
		testEmbedFactsProducesChunks(t, opts.NewStore(t))
	})
	t.Run("NamespaceIsolation", func(t *testing.T) {
		if opts.NewStoreNS == nil {
			t.Skip("NewStoreNS not provided; skipping namespace isolation test")
		}
		testNamespaceIsolation(t, opts.NewStoreNS)
	})
	t.Run("UserIsolation", func(t *testing.T) {
		// Each subtest skips individually (rather than skipping the parent)
		// so backends without per-user scoping show the family as SKIPPED,
		// not absent.
		newTwo := func(t *testing.T) (memstore.Store, memstore.Store) {
			t.Helper()
			if opts.NewTwoUserStores == nil {
				t.Skip("NewTwoUserStores not provided; skipping user isolation test")
			}
			return opts.NewTwoUserStores(t)
		}
		t.Run("GetCrossUserNotFound", func(t *testing.T) {
			a, b := newTwo(t)
			testGetCrossUserNotFound(t, a, b)
		})
		t.Run("ListAndSearchIsolated", func(t *testing.T) {
			a, b := newTwo(t)
			testListAndSearchIsolated(t, a, b)
		})
		t.Run("MutationsIsolated", func(t *testing.T) {
			a, b := newTwo(t)
			testMutationsIsolated(t, a, b)
		})
		t.Run("LinksIsolated", func(t *testing.T) {
			a, b := newTwo(t)
			testLinksIsolated(t, a, b)
		})
		t.Run("HistoryCannotCrossUsers", func(t *testing.T) {
			a, b := newTwo(t)
			if opts.SetSupersededBy == nil {
				t.Skip("SetSupersededBy not provided; skipping cross-user history test")
			}
			testHistoryCannotCrossUsers(t, a, b, opts.SetSupersededBy)
		})
		t.Run("EmbedPipelineIsolated", func(t *testing.T) {
			a, b := newTwo(t)
			testEmbedPipelineIsolated(t, a, b)
		})
	})
	t.Run("SessionIsolation", func(t *testing.T) {
		newTwoSession := func(t *testing.T) (memstore.SessionStore, memstore.SessionStore) {
			t.Helper()
			if opts.NewTwoUserSessionStores == nil {
				t.Skip("NewTwoUserSessionStores not provided; skipping session isolation test")
			}
			return opts.NewTwoUserSessionStores(t)
		}
		t.Run("TurnsIsolated", func(t *testing.T) {
			a, b := newTwoSession(t)
			testSessionTurnsIsolated(t, a, b)
		})
		t.Run("HintsIsolated", func(t *testing.T) {
			a, b := newTwoSession(t)
			testSessionHintsIsolated(t, a, b)
		})
		t.Run("InjectionsIsolated", func(t *testing.T) {
			a, b := newTwoSession(t)
			testSessionInjectionsIsolated(t, a, b)
		})
		t.Run("FeedbackIsolated", func(t *testing.T) {
			a, b := newTwoSession(t)
			testSessionFeedbackIsolated(t, a, b)
		})
	})
}

// --- subtest implementations ---

func testInsertGetRoundTrip(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	meta := json.RawMessage(`{"source":"conformance","priority":7}`)
	id, err := s.Insert(ctx, memstore.Fact{
		Content:   "conformance test fact",
		Subject:   "conformance",
		Category:  "test",
		Kind:      "invariant",
		Subsystem: "core",
		Metadata:  meta,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == 0 {
		t.Fatal("Insert returned zero ID")
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		// The explicit return after Fatal is for staticcheck's SA5011, which
		// (as of the CI linter's current version) does not treat Fatal as
		// terminating here and flags the dereferences below.
		t.Fatal("Get returned nil")
		return
	}
	if got.Content != "conformance test fact" {
		t.Errorf("Content = %q, want %q", got.Content, "conformance test fact")
	}
	if got.Subject != "conformance" {
		t.Errorf("Subject = %q, want %q", got.Subject, "conformance")
	}
	if got.Category != "test" {
		t.Errorf("Category = %q, want %q", got.Category, "test")
	}
	if got.Kind != "invariant" {
		t.Errorf("Kind = %q, want %q", got.Kind, "invariant")
	}
	if got.Subsystem != "core" {
		t.Errorf("Subsystem = %q, want %q", got.Subsystem, "core")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if got.Metadata == nil {
		t.Fatal("Metadata is nil")
	}

	// UserID must be non-zero: the store resolves an identity at construction.
	if got.UserID == 0 {
		t.Error("UserID is 0; store must assign a non-zero user identity on insert")
	}

	// A second fact inserted via the same store must share the same UserID.
	id2, err := s.Insert(ctx, memstore.Fact{
		Content:  "second conformance fact",
		Subject:  "conformance",
		Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert second fact: %v", err)
	}
	got2, err := s.Get(ctx, id2)
	if err != nil {
		t.Fatalf("Get second fact: %v", err)
	}
	if got2 == nil {
		t.Fatal("Get second fact returned nil")
		return // SA5011; see testInsertGetRoundTrip
	}
	if got2.UserID != got.UserID {
		t.Errorf("second fact UserID = %d, want same as first (%d)", got2.UserID, got.UserID)
	}

	// Compare metadata by value, not raw bytes -- Postgres
	// JSONB, so key order may differ.
	var gotMeta, wantMeta map[string]any
	if err := json.Unmarshal(got.Metadata, &gotMeta); err != nil {
		t.Fatalf("unmarshal got.Metadata: %v", err)
	}
	if err := json.Unmarshal(meta, &wantMeta); err != nil {
		t.Fatalf("unmarshal want meta: %v", err)
	}
	for k, wv := range wantMeta {
		gv, ok := gotMeta[k]
		if !ok {
			t.Errorf("metadata missing key %q", k)
			continue
		}
		// JSON numbers unmarshal as float64 in both backends.
		if wf, ok := wv.(float64); ok {
			if gf, ok := gv.(float64); !ok || gf != wf {
				t.Errorf("metadata[%q] = %v, want %v", k, gv, wv)
			}
		} else if gv != wv {
			t.Errorf("metadata[%q] = %v, want %v", k, gv, wv)
		}
	}
}

// testBreakdownAggregates covers the aggregate that replaced counting rows in
// Go. It must agree with ActiveCount, exclude superseded facts, and omit the
// empty kind the way the caller's own loop did.
func testBreakdownAggregates(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	seed := []memstore.Fact{
		{Content: "alpha one", Subject: "alpha", Category: "note", Kind: "convention"},
		{Content: "alpha two", Subject: "alpha", Category: "note", Kind: "convention"},
		{Content: "alpha three", Subject: "alpha", Category: "project", Kind: ""},
		{Content: "beta one", Subject: "beta", Category: "project", Kind: "decision"},
	}
	for _, f := range seed {
		if _, err := s.Insert(ctx, f); err != nil {
			t.Fatalf("Insert(%q): %v", f.Content, err)
		}
	}

	// A superseded fact must not appear in any histogram.
	oldID, err := s.Insert(ctx, memstore.Fact{Content: "gamma old", Subject: "gamma", Category: "note"})
	if err != nil {
		t.Fatalf("Insert(gamma old): %v", err)
	}
	newID, err := s.Insert(ctx, memstore.Fact{Content: "gamma new", Subject: "gamma", Category: "note"})
	if err != nil {
		t.Fatalf("Insert(gamma new): %v", err)
	}
	if err := s.Supersede(ctx, oldID, newID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	bd, err := s.Breakdown(ctx)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}

	wantSubjects := map[string]int{"alpha": 3, "beta": 1, "gamma": 1}
	if !maps.Equal(bd.Subjects, wantSubjects) {
		t.Errorf("Subjects = %v, want %v", bd.Subjects, wantSubjects)
	}

	wantCategories := map[string]int{"note": 3, "project": 2}
	if !maps.Equal(bd.Categories, wantCategories) {
		t.Errorf("Categories = %v, want %v", bd.Categories, wantCategories)
	}

	// The empty kind is omitted, matching what the pre-aggregate loop did.
	wantKinds := map[string]int{"convention": 2, "decision": 1}
	if !maps.Equal(bd.Kinds, wantKinds) {
		t.Errorf("Kinds = %v, want %v", bd.Kinds, wantKinds)
	}

	// The aggregate and the counter must not disagree about what is active.
	active, err := s.ActiveCount(ctx)
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	var summed int
	for _, n := range bd.Subjects {
		summed += n
	}
	if int64(summed) != active {
		t.Errorf("Subjects sum to %d, ActiveCount says %d", summed, active)
	}
}

// testNotFoundSentinel pins the contract that an id-targeted operation naming a
// row that does not exist reports memstore.ErrNotFound rather than an opaque
// error. History is here as well as the mutations: it is a read, but it is
// id-targeted, so a typo in the id reaches the caller the same way (#172). The MCP layer needs to tell that case apart from a real store failure:
// the two report on different channels, and without a sentinel every miss looks
// like an outage. Both backends must agree, including LinkFacts, where the miss
// surfaces as a foreign-key violation rather than a zero-row update.
func testNotFoundSentinel(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	const missing = int64(999999)

	realID, err := s.Insert(ctx, memstore.Fact{Content: "anchor fact", Subject: "anchor", Category: "note"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	linkID, err := s.LinkFacts(ctx, realID, realID, "reference", false, "", nil)
	if err != nil {
		t.Fatalf("LinkFacts: %v", err)
	}

	cases := []struct {
		name string
		op   func() error
	}{
		{"Delete", func() error { return s.Delete(ctx, missing) }},
		{"Confirm", func() error { return s.Confirm(ctx, missing) }},
		{"UpdateMetadata", func() error { return s.UpdateMetadata(ctx, missing, map[string]any{"k": "v"}) }},
		{"DeleteLink", func() error { return s.DeleteLink(ctx, missing) }},
		{"UpdateLink", func() error { return s.UpdateLink(ctx, missing, "label", nil) }},
		{"LinkFactsMissingSource", func() error {
			_, err := s.LinkFacts(ctx, missing, realID, "reference", false, "", nil)
			return err
		}},
		{"LinkFactsMissingTarget", func() error {
			_, err := s.LinkFacts(ctx, realID, missing, "reference", false, "", nil)
			return err
		}},
		{"Supersede", func() error { return s.Supersede(ctx, missing, realID) }},
		{"History", func() error {
			_, err := s.History(ctx, missing, "")
			return err
		}},
	}

	for _, tc := range cases {
		err := tc.op()
		if err == nil {
			t.Errorf("%s(%d): expected an error, got nil", tc.name, missing)
			continue
		}
		if !errors.Is(err, memstore.ErrNotFound) {
			t.Errorf("%s(%d): errors.Is(err, ErrNotFound) = false; err = %v", tc.name, missing, err)
		}
	}

	// The sentinel must not leak onto operations that succeed.
	if err := s.Confirm(ctx, realID); err != nil {
		t.Errorf("Confirm(existing): %v", err)
	}
	if err := s.UpdateLink(ctx, linkID, "new label", nil); err != nil {
		t.Errorf("UpdateLink(existing): %v", err)
	}
	if err := s.Delete(ctx, realID); err != nil {
		t.Errorf("Delete(existing): %v", err)
	}
}

func testExistsAndDedup(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	_, err := s.Insert(ctx, memstore.Fact{
		Content:  "unique content string",
		Subject:  "dedup-subject",
		Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	exists, err := s.Exists(ctx, "unique content string", "dedup-subject")
	if err != nil {
		t.Fatalf("Exists (should be true): %v", err)
	}
	if !exists {
		t.Error("Exists returned false for known content+subject")
	}

	exists, err = s.Exists(ctx, "unique content string", "other-subject")
	if err != nil {
		t.Fatalf("Exists (wrong subject): %v", err)
	}
	if exists {
		t.Error("Exists returned true for wrong subject")
	}

	exists, err = s.Exists(ctx, "different content", "dedup-subject")
	if err != nil {
		t.Fatalf("Exists (wrong content): %v", err)
	}
	if exists {
		t.Error("Exists returned true for wrong content")
	}
}

func testSupersedeAndHistory(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	idA, err := s.Insert(ctx, memstore.Fact{Content: "v1", Subject: "chain", Category: "test"})
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	idB, err := s.Insert(ctx, memstore.Fact{Content: "v2", Subject: "chain", Category: "test"})
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}
	idC, err := s.Insert(ctx, memstore.Fact{Content: "v3", Subject: "chain", Category: "test"})
	if err != nil {
		t.Fatalf("insert C: %v", err)
	}

	if err := s.Supersede(ctx, idA, idB); err != nil {
		t.Fatalf("Supersede A->B: %v", err)
	}
	if err := s.Supersede(ctx, idB, idC); err != nil {
		t.Fatalf("Supersede B->C: %v", err)
	}

	// History from any node should return the full 3-entry chain, oldest first.
	for _, queryID := range []int64{idA, idB, idC} {
		entries, err := s.History(ctx, queryID, "")
		if err != nil {
			t.Fatalf("History(id=%d): %v", queryID, err)
		}
		if len(entries) != 3 {
			t.Fatalf("History(id=%d): got %d entries, want 3", queryID, len(entries))
		}
		if entries[0].Fact.Content != "v1" {
			t.Errorf("History(id=%d): entries[0].Content = %q, want v1", queryID, entries[0].Fact.Content)
		}
		if entries[2].Fact.Content != "v3" {
			t.Errorf("History(id=%d): entries[2].Content = %q, want v3", queryID, entries[2].Fact.Content)
		}
		for i, e := range entries {
			if e.Position != i {
				t.Errorf("History(id=%d): entries[%d].Position = %d, want %d", queryID, i, e.Position, i)
			}
			if e.ChainLength != 3 {
				t.Errorf("History(id=%d): entries[%d].ChainLength = %d, want 3", queryID, i, e.ChainLength)
			}
		}
	}

	// OnlyActive listings should exclude the superseded facts.
	facts, err := s.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if err != nil {
		t.Fatalf("List OnlyActive: %v", err)
	}
	for _, f := range facts {
		if f.ID == idA || f.ID == idB {
			t.Errorf("List OnlyActive returned superseded fact id=%d", f.ID)
		}
	}
	found := false
	for _, f := range facts {
		if f.ID == idC {
			found = true
		}
	}
	if !found {
		t.Error("List OnlyActive did not return the active fact")
	}
}

func testHistoryCycleTerminates(t *testing.T, s memstore.Store, setSupersededBy func(*testing.T, int64, int64)) {
	t.Helper()
	ctx := context.Background()

	idA, err := s.Insert(ctx, memstore.Fact{Content: "cycle A", Subject: "cycle", Category: "test"})
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	idB, err := s.Insert(ctx, memstore.Fact{Content: "cycle B", Subject: "cycle", Category: "test"})
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}

	// Force A -> B -> A cycle via caller-provided raw write (bypasses Store
	// validation; the closure uses backend-native SQL with its own placeholders).
	setSupersededBy(t, idB, idA) // A.superseded_by = B
	setSupersededBy(t, idA, idB) // B.superseded_by = A

	entries, err := s.History(ctx, idA, "")
	if err != nil {
		t.Fatalf("History returned error on cyclic data: %v", err)
	}
	if len(entries) > 3 {
		t.Errorf("expected at most 3 entries for a 2-node cycle, got %d", len(entries))
	}
	if len(entries) == 0 {
		t.Error("expected at least 1 entry (the anchor itself)")
	}
}

func testInvalidMetadataFilterErrors(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "seed fact", Subject: "X", Category: "test",
		Metadata: json.RawMessage(`{"tier":"gold"}`),
	})

	cases := []struct {
		name    string
		filters []memstore.MetadataFilter
	}{
		{"bad key in List", []memstore.MetadataFilter{{Key: "bad-key!", Op: "=", Value: "x"}}},
		{"bad op in List", []memstore.MetadataFilter{{Key: "tier", Op: "LIKE", Value: "x"}}},
	}
	for _, tc := range cases {
		_, err := s.List(ctx, memstore.QueryOpts{MetadataFilters: tc.filters})
		if err == nil {
			t.Errorf("List %s: expected error, got nil", tc.name)
		}
	}

	// SearchFTS must reject the same invalid filters.
	_, err := s.SearchFTS(ctx, "seed", memstore.SearchOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "bad-key!", Op: "=", Value: "x"}},
	})
	if err == nil {
		t.Error("SearchFTS bad key: expected error, got nil")
	}
	_, err = s.SearchFTS(ctx, "seed", memstore.SearchOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "tier", Op: "LIKE", Value: "x"}},
	})
	if err == nil {
		t.Error("SearchFTS bad op: expected error, got nil")
	}
}

func testMetadataFilterMatches(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	// String-valued equality is the filter form both backends support today.
	// Numeric comparison is covered by NumericMetadataComparisonDivergence.
	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "gold fact", Subject: "filter", Category: "test",
		Metadata: json.RawMessage(`{"tier":"gold"}`),
	})
	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "silver fact", Subject: "filter", Category: "test",
		Metadata: json.RawMessage(`{"tier":"silver"}`),
	})

	facts, err := s.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "tier", Op: "=", Value: "gold"},
		},
	})
	if err != nil {
		t.Fatalf("List with valid filter: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].Content != "gold fact" {
		t.Errorf("Content = %q, want %q", facts[0].Content, "gold fact")
	}
}

// testNumericMetadataComparisonDivergence pins the numeric metadata-filter
// contract: when the filter value is a Go numeric type, comparison is numeric
// (not lexicographic). The skip path remains for backends that have not yet
// implemented numeric comparison -- on those backends the first List call
// returns an error and the subtest skips rather than fails.
//
// Contract (must hold on every backend that passes):
//   - chapter <= 3: low-chapter (1) included, high-chapter (9) excluded.
//   - missing-key fact: excluded without IncludeNull, included with it.
//   - non-numeric fact (chapter:"not-a-number"): excluded in both modes.
//   - IncludeNull variant: returns low-chapter and missing-key; not high or
//     non-numeric.
func testNumericMetadataComparisonDivergence(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "low chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{"chapter":1}`),
	})
	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "high chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{"chapter":9}`),
	})
	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "missing chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{}`),
	})
	s.Insert(ctx, memstore.Fact{ //nolint:errcheck // fixture insert; a failure surfaces via the assertions below
		Content: "non-numeric chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{"chapter":"not-a-number"}`),
	})

	// Plain filter: skip if backend does not support numeric comparison.
	facts, err := s.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 3},
		},
	})
	if err != nil {
		t.Skipf("known divergence: numeric metadata comparison unsupported on this backend: %v", err)
	}

	// Only low chapter (1) should be returned; missing and non-numeric are excluded.
	if len(facts) != 1 {
		t.Fatalf("chapter <= 3: got %d facts, want 1", len(facts))
	}
	if facts[0].Content != "low chapter" {
		t.Errorf("chapter <= 3: Content = %q, want %q", facts[0].Content, "low chapter")
	}

	// Verify missing-key and non-numeric facts are not present.
	for _, f := range facts {
		if f.Content == "missing chapter" {
			t.Error("chapter <= 3: missing-key fact should not match")
		}
		if f.Content == "non-numeric chapter" {
			t.Error("chapter <= 3: non-numeric chapter fact should not match")
		}
	}

	// IncludeNull variant: missing-key is included; non-numeric is still excluded.
	factsInc, err := s.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 3, IncludeNull: true},
		},
	})
	if err != nil {
		t.Fatalf("chapter <= 3 IncludeNull: %v", err)
	}
	var gotLow, gotMissing, gotHigh, gotNonNumeric bool
	for _, f := range factsInc {
		switch f.Content {
		case "low chapter":
			gotLow = true
		case "missing chapter":
			gotMissing = true
		case "high chapter":
			gotHigh = true
		case "non-numeric chapter":
			gotNonNumeric = true
		}
	}
	if !gotLow {
		t.Error("chapter <= 3 IncludeNull: expected low chapter fact, not found")
	}
	if !gotMissing {
		t.Error("chapter <= 3 IncludeNull: expected missing-key fact (IncludeNull), not found")
	}
	if gotHigh {
		t.Error("chapter <= 3 IncludeNull: high chapter should not match <= 3")
	}
	if gotNonNumeric {
		t.Error("chapter <= 3 IncludeNull: non-numeric chapter should not match even with IncludeNull")
	}
}

func testUpdateMetadataMergeSemantics(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	id, err := s.Insert(ctx, memstore.Fact{
		Content: "merge test", Subject: "M", Category: "test",
		Metadata: json.RawMessage(`{"keep":"yes","overwrite":"old","remove":"me"}`),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Set a new key, overwrite an existing key, delete one with nil value.
	if err := s.UpdateMetadata(ctx, id, map[string]any{
		"new_key":   "added",
		"overwrite": "new",
		"remove":    nil,
	}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after UpdateMetadata: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Metadata, &m); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}

	if m["keep"] != "yes" {
		t.Errorf("untouched key 'keep' = %v, want yes", m["keep"])
	}
	if m["new_key"] != "added" {
		t.Errorf("new key 'new_key' = %v, want added", m["new_key"])
	}
	if m["overwrite"] != "new" {
		t.Errorf("overwritten key 'overwrite' = %v, want new", m["overwrite"])
	}
	if _, exists := m["remove"]; exists {
		t.Errorf("nil-patched key 'remove' should have been deleted, got %v", m["remove"])
	}
}

func testEmbedQuarantine(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	// A fact without an embedding needs embedding.
	id, err := s.Insert(ctx, memstore.Fact{
		Content: "needs embedding", Subject: "Q", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	before, err := s.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("NeedingEmbedding before quarantine: %v", err)
	}
	foundBefore := false
	for _, f := range before {
		if f.ID == id {
			foundBefore = true
		}
	}
	if !foundBefore {
		t.Fatalf("fact id=%d not in NeedingEmbedding before quarantine", id)
	}

	// Quarantine the fact.
	if err := s.MarkEmbedFailed(ctx, id, "conformance test reason"); err != nil {
		t.Fatalf("MarkEmbedFailed: %v", err)
	}

	// After quarantine it must not appear in NeedingEmbedding.
	after, err := s.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("NeedingEmbedding after quarantine: %v", err)
	}
	for _, f := range after {
		if f.ID == id {
			t.Errorf("quarantined fact id=%d still appears in NeedingEmbedding", id)
		}
	}
}

func testNamespaceIsolation(t *testing.T, newStoreNS func(*testing.T, string) memstore.Store) {
	t.Helper()
	ctx := context.Background()

	sA := newStoreNS(t, "ns-alpha")
	sB := newStoreNS(t, "ns-beta")

	idA, err := sA.Insert(ctx, memstore.Fact{
		Content: "alpha-only fact", Subject: "shared-subject", Category: "test",
	})
	if err != nil {
		t.Fatalf("insert into alpha: %v", err)
	}
	_, err = sB.Insert(ctx, memstore.Fact{
		Content: "beta-only fact", Subject: "shared-subject", Category: "test",
	})
	if err != nil {
		t.Fatalf("insert into beta: %v", err)
	}

	// sB must not see sA's fact by ID.
	gotCross, err := sB.Get(ctx, idA)
	if err != nil {
		t.Fatalf("Get cross-namespace: %v", err)
	}
	if gotCross != nil {
		t.Errorf("sB.Get(idA) should return nil, got %+v", gotCross)
	}

	// Each namespace sees only its own facts by subject.
	factsA, err := sA.BySubject(ctx, "shared-subject", false)
	if err != nil {
		t.Fatalf("BySubject alpha: %v", err)
	}
	if len(factsA) != 1 || factsA[0].Content != "alpha-only fact" {
		t.Errorf("BySubject alpha: got %v, want 1 alpha fact", factsA)
	}

	factsB, err := sB.BySubject(ctx, "shared-subject", false)
	if err != nil {
		t.Fatalf("BySubject beta: %v", err)
	}
	if len(factsB) != 1 || factsB[0].Content != "beta-only fact" {
		t.Errorf("BySubject beta: got %v, want 1 beta fact", factsB)
	}

	// Exists must be scoped.
	exists, err := sB.Exists(ctx, "alpha-only fact", "shared-subject")
	if err != nil {
		t.Fatalf("Exists cross-namespace: %v", err)
	}
	if exists {
		t.Error("sB.Exists should not find alpha's fact")
	}
}

// --- UserIsolation subtests ---
//
// Each subtest receives two stores over the same database and namespace,
// scoped to different users (A seeds, B probes). The contract under test is
// plan 009 D2: owner-only access on every read and write path, with
// cross-user access indistinguishable from nonexistent data.

// missingFactID is an id far beyond anything a fresh test database assigns,
// used to compare cross-user outcomes against the nonexistent-id outcome.
const missingFactID = int64(1) << 60

func testGetCrossUserNotFound(t *testing.T, a, b memstore.Store) {
	t.Helper()
	ctx := context.Background()

	id, err := a.Insert(ctx, memstore.Fact{Content: "user A private fact", Subject: "iso", Category: "test"})
	if err != nil {
		t.Fatalf("Insert A: %v", err)
	}

	crossFact, crossErr := b.Get(ctx, id)
	missingFact, missingErr := b.Get(ctx, missingFactID)

	if crossFact != nil {
		t.Fatalf("B.Get(A's id) returned a fact: %+v", crossFact)
	}
	if missingFact != nil {
		t.Fatalf("B.Get(nonexistent id) returned a fact: %+v", missingFact)
	}
	// Existence must not leak: the cross-user outcome must be identical to
	// the nonexistent-id outcome.
	if fmt.Sprint(crossErr) != fmt.Sprint(missingErr) {
		t.Errorf("B.Get(A's id) err = %v; B.Get(nonexistent) err = %v; outcomes must be identical", crossErr, missingErr)
	}

	// A still sees its own fact.
	own, err := a.Get(ctx, id)
	if err != nil || own == nil {
		t.Fatalf("A.Get(own id) = (%v, %v), want fact", own, err)
	}
}

func testListAndSearchIsolated(t *testing.T, a, b memstore.Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := a.Insert(ctx, memstore.Fact{
		Content: "zebra quantum heliotrope fact", Subject: "iso-subject",
		Category: "test", Subsystem: "isosub",
	}); err != nil {
		t.Fatalf("Insert A: %v", err)
	}

	facts, err := b.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatalf("B.List: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("B.List sees %d of A's facts, want 0", len(facts))
	}

	bySub, err := b.BySubject(ctx, "iso-subject", false)
	if err != nil {
		t.Fatalf("B.BySubject: %v", err)
	}
	if len(bySub) != 0 {
		t.Errorf("B.BySubject sees %d of A's facts, want 0", len(bySub))
	}

	exists, err := b.Exists(ctx, "zebra quantum heliotrope fact", "iso-subject")
	if err != nil {
		t.Fatalf("B.Exists: %v", err)
	}
	if exists {
		t.Error("B.Exists found A's fact")
	}

	count, err := b.ActiveCount(ctx)
	if err != nil {
		t.Fatalf("B.ActiveCount: %v", err)
	}
	if count != 0 {
		t.Errorf("B.ActiveCount = %d, want 0", count)
	}

	fts, err := b.SearchFTS(ctx, "zebra quantum", memstore.SearchOpts{})
	if err != nil {
		t.Fatalf("B.SearchFTS: %v", err)
	}
	if len(fts) != 0 {
		t.Errorf("B.SearchFTS returned %d of A's facts, want 0", len(fts))
	}

	hybrid, err := b.Search(ctx, "zebra quantum", memstore.SearchOpts{})
	if err != nil {
		t.Fatalf("B.Search: %v", err)
	}
	if len(hybrid) != 0 {
		t.Errorf("B.Search returned %d of A's facts, want 0", len(hybrid))
	}

	batch, err := b.SearchBatch(ctx, []string{"zebra quantum"}, memstore.SearchOpts{})
	if err != nil {
		t.Fatalf("B.SearchBatch: %v", err)
	}
	for i, res := range batch {
		if len(res) != 0 {
			t.Errorf("B.SearchBatch[%d] returned %d of A's facts, want 0", i, len(res))
		}
	}

	subs, err := b.ListSubsystems(ctx, "")
	if err != nil {
		t.Fatalf("B.ListSubsystems: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("B.ListSubsystems sees A's subsystems: %v", subs)
	}

	// Swap roles: B seeds, A probes.
	if _, err := b.Insert(ctx, memstore.Fact{
		Content: "yonder gizmo flotsam fact", Subject: "iso-b", Category: "test",
	}); err != nil {
		t.Fatalf("Insert B: %v", err)
	}
	aFacts, err := a.List(ctx, memstore.QueryOpts{Subject: "iso-b"})
	if err != nil {
		t.Fatalf("A.List: %v", err)
	}
	if len(aFacts) != 0 {
		t.Errorf("A.List sees %d of B's facts, want 0", len(aFacts))
	}
	aFTS, err := a.SearchFTS(ctx, "yonder gizmo", memstore.SearchOpts{})
	if err != nil {
		t.Fatalf("A.SearchFTS: %v", err)
	}
	if len(aFTS) != 0 {
		t.Errorf("A.SearchFTS returned %d of B's facts, want 0", len(aFTS))
	}
}

func testMutationsIsolated(t *testing.T, a, b memstore.Store) {
	t.Helper()
	ctx := context.Background()

	id, err := a.Insert(ctx, memstore.Fact{
		Content: "mutation target", Subject: "iso-mut", Category: "test",
		Metadata: json.RawMessage(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("Insert A: %v", err)
	}
	orig, err := a.Get(ctx, id)
	if err != nil || orig == nil {
		t.Fatalf("A.Get(own id) = (%v, %v), want fact", orig, err)
	}

	// Each mutation must produce the same outcome for A's id as for a
	// nonexistent id (no existence leak), and leave A's data unchanged.
	check := func(name string, fn func(id int64) error) {
		t.Helper()
		crossErr := fn(id)
		missErr := fn(missingFactID)
		if (crossErr == nil) != (missErr == nil) {
			t.Errorf("B.%s: cross-user err = %v, nonexistent err = %v; outcomes must match", name, crossErr, missErr)
		}
	}

	check("Confirm", func(x int64) error { return b.Confirm(ctx, x) })
	check("Touch", func(x int64) error { return b.Touch(ctx, []int64{x}) })
	// RecordInjection is a counter bump like Touch and has to respect the same
	// boundary. Optional on the interface, so probe it only where implemented.
	if rec, ok := b.(memstore.InjectionRecorder); ok {
		check("RecordInjection", func(x int64) error { return rec.RecordInjection(ctx, []int64{x}) })
	}
	check("UpdateMetadata", func(x int64) error { return b.UpdateMetadata(ctx, x, map[string]any{"k": "hacked"}) })
	check("Supersede", func(x int64) error { return b.Supersede(ctx, x, x) })
	check("SetEmbedding", func(x int64) error { return b.SetEmbedding(ctx, x, []float32{0.1, 0.2, 0.3, 0.4}) })
	check("MarkEmbedFailed", func(x int64) error { return b.MarkEmbedFailed(ctx, x, "isolation probe") })
	check("Delete", func(x int64) error { return b.Delete(ctx, x) })

	after, err := a.Get(ctx, id)
	if err != nil {
		t.Fatalf("A.Get after probes: %v", err)
	}
	if after == nil {
		t.Fatal("A's fact was deleted by B")
		return // SA5011; see testInsertGetRoundTrip
	}
	if after.Content != orig.Content {
		t.Errorf("Content changed: %q -> %q", orig.Content, after.Content)
	}
	if after.ConfirmedCount != orig.ConfirmedCount {
		t.Errorf("ConfirmedCount changed: %d -> %d", orig.ConfirmedCount, after.ConfirmedCount)
	}
	if after.UseCount != orig.UseCount {
		t.Errorf("UseCount changed: %d -> %d", orig.UseCount, after.UseCount)
	}
	if after.SupersededBy != nil {
		t.Errorf("SupersededBy set by B: %v", *after.SupersededBy)
	}
	if string(after.Metadata) != string(orig.Metadata) {
		t.Errorf("Metadata changed: %s -> %s", orig.Metadata, after.Metadata)
	}

	// The fact has no embedding, so it must still be in A's embed queue:
	// B's SetEmbedding must not have landed, and B's MarkEmbedFailed must
	// not have quarantined it.
	pending, err := a.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("A.NeedingEmbedding: %v", err)
	}
	found := false
	for _, f := range pending {
		if f.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Error("A's fact left the embed queue: B's SetEmbedding or MarkEmbedFailed landed")
	}
}

func testLinksIsolated(t *testing.T, a, b memstore.Store) {
	t.Helper()
	ctx := context.Background()

	src, err := a.Insert(ctx, memstore.Fact{Content: "link source", Subject: "iso-link", Category: "test"})
	if err != nil {
		t.Fatalf("Insert A src: %v", err)
	}
	tgt, err := a.Insert(ctx, memstore.Fact{Content: "link target", Subject: "iso-link", Category: "test"})
	if err != nil {
		t.Fatalf("Insert A tgt: %v", err)
	}
	linkID, err := a.LinkFacts(ctx, src, tgt, "ref", false, "a-link", nil)
	if err != nil {
		t.Fatalf("A.LinkFacts: %v", err)
	}

	// B cannot link A's facts, in any combination with its own.
	bFact, err := b.Insert(ctx, memstore.Fact{Content: "b fact", Subject: "iso-link", Category: "test"})
	if err != nil {
		t.Fatalf("Insert B: %v", err)
	}
	if _, err := b.LinkFacts(ctx, src, tgt, "ref", false, "b-link", nil); err == nil {
		t.Error("B.LinkFacts(A's src, A's tgt) succeeded")
	}
	if _, err := b.LinkFacts(ctx, bFact, tgt, "ref", false, "", nil); err == nil {
		t.Error("B.LinkFacts(B's fact, A's tgt) succeeded")
	}
	if _, err := b.LinkFacts(ctx, src, bFact, "ref", false, "", nil); err == nil {
		t.Error("B.LinkFacts(A's src, B's fact) succeeded")
	}

	// B cannot see or affect A's link. GetLink follows Get's not-found
	// contract: a foreign (or absent) link is (nil, nil), not an error. A
	// non-nil link here -- regardless of err -- would be the leak.
	if l, err := b.GetLink(ctx, linkID); err != nil {
		t.Errorf("B.GetLink(A's link) returned error, want (nil, nil): %v", err)
	} else if l != nil {
		t.Errorf("B.GetLink sees A's link: %+v", l)
	}
	links, err := b.GetLinks(ctx, src, memstore.LinkBoth)
	if err != nil {
		t.Fatalf("B.GetLinks: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("B.GetLinks sees %d of A's links, want 0", len(links))
	}
	if err := b.UpdateLink(ctx, linkID, "hacked", nil); err == nil {
		t.Error("B.UpdateLink on A's link succeeded")
	}
	if err := b.DeleteLink(ctx, linkID); err == nil {
		t.Error("B.DeleteLink on A's link succeeded")
	}

	// A's link is intact and unchanged.
	l, err := a.GetLink(ctx, linkID)
	if err != nil {
		t.Fatalf("A.GetLink after probes: %v", err)
	}
	if l.Label != "a-link" {
		t.Errorf("A's link label = %q, want %q", l.Label, "a-link")
	}
}

func testHistoryCannotCrossUsers(t *testing.T, a, b memstore.Store, setSupersededBy func(t *testing.T, supersededByID, targetID int64)) {
	t.Helper()
	ctx := context.Background()

	aID, err := a.Insert(ctx, memstore.Fact{Content: "user A chain head", Subject: "iso-hist", Category: "test"})
	if err != nil {
		t.Fatalf("Insert A: %v", err)
	}
	bID, err := b.Insert(ctx, memstore.Fact{Content: "user B secret", Subject: "iso-hist", Category: "test"})
	if err != nil {
		t.Fatalf("Insert B: %v", err)
	}

	// Forge A's fact's superseded_by to point at B's fact via a raw write.
	setSupersededBy(t, bID, aID)

	// A's history must terminate without ever returning B's fact, exactly
	// as if superseded_by were dangling.
	entries, err := a.History(ctx, aID, "")
	if err != nil {
		t.Fatalf("A.History: %v", err)
	}
	for _, e := range entries {
		if e.Fact.ID == bID {
			t.Errorf("A.History crossed into B's fact %d", bID)
		}
	}

	// B's history of its own fact must not include A's.
	bEntries, err := b.History(ctx, bID, "")
	if err != nil {
		t.Fatalf("B.History: %v", err)
	}
	for _, e := range bEntries {
		if e.Fact.ID == aID {
			t.Errorf("B.History crossed into A's fact %d", aID)
		}
	}

	// Subject-based history is scoped too.
	bSubj, err := b.History(ctx, 0, "iso-hist")
	if err != nil {
		t.Fatalf("B.History(subject): %v", err)
	}
	for _, e := range bSubj {
		if e.Fact.ID == aID {
			t.Errorf("B.History(subject) returned A's fact %d", aID)
		}
	}
}

func testEmbedPipelineIsolated(t *testing.T, a, b memstore.Store) {
	t.Helper()
	ctx := context.Background()

	aID, err := a.Insert(ctx, memstore.Fact{Content: "unembedded fact of A", Subject: "iso-embed", Category: "test"})
	if err != nil {
		t.Fatalf("Insert A: %v", err)
	}

	// Sanity: the factory must produce an un-embedded fact for this test to
	// mean anything. If the backend embeds at insert time, skip.
	aPending, err := a.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("A.NeedingEmbedding: %v", err)
	}
	found := false
	for _, f := range aPending {
		if f.ID == aID {
			found = true
			break
		}
	}
	if !found {
		t.Skip("factory embeds facts at insert; cannot produce an un-embedded fact to isolate")
	}

	bPending, err := b.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("B.NeedingEmbedding: %v", err)
	}
	for _, f := range bPending {
		if f.ID == aID {
			t.Errorf("B.NeedingEmbedding returned A's fact %d", aID)
		}
	}
}

// --- session isolation subtests ---

// sessionTurnReader is the extended capability for reading back session turns.
// Not part of the core SessionStore interface; capability-asserted in the test.
type sessionTurnReader interface {
	GetSessionTurns(ctx context.Context, sessionID string) ([]memstore.SessionTurn, error)
}

// sessionInjectionReader is the extended capability for reading injected fact IDs.
type sessionInjectionReader interface {
	GetInjectedFactIDs(ctx context.Context, sessionID string) ([]int64, error)
}

// testSessionTurnsIsolated verifies that two users' turns for the same
// session_id are stored and retrieved independently.
func testSessionTurnsIsolated(t *testing.T, a, b memstore.SessionStore) {
	t.Helper()
	ctx := context.Background()
	sid := "session-turns-isolation"

	ar, ok := a.(sessionTurnReader)
	if !ok {
		t.Skip("store does not implement GetSessionTurns; skipping turns isolation check")
	}
	br, ok := b.(sessionTurnReader)
	if !ok {
		t.Skip("store does not implement GetSessionTurns; skipping turns isolation check")
	}

	turnsA := []memstore.SessionTurn{
		{SessionID: sid, UUID: "uuid-a-1", TurnIndex: 0, Role: "user", Content: "hello from A"},
	}
	turnsB := []memstore.SessionTurn{
		{SessionID: sid, UUID: "uuid-b-1", TurnIndex: 0, Role: "user", Content: "hello from B"},
	}

	if err := a.SaveTurns(ctx, sid, turnsA); err != nil {
		t.Fatalf("A.SaveTurns: %v", err)
	}
	if err := b.SaveTurns(ctx, sid, turnsB); err != nil {
		t.Fatalf("B.SaveTurns: %v", err)
	}

	gotA, err := ar.GetSessionTurns(ctx, sid)
	if err != nil {
		t.Fatalf("A.GetSessionTurns: %v", err)
	}
	if len(gotA) != 1 || gotA[0].Content != "hello from A" {
		t.Errorf("A sees wrong turns: %v", gotA)
	}
	for _, tr := range gotA {
		if tr.Content == "hello from B" {
			t.Errorf("A.GetSessionTurns leaked B's turn")
		}
	}

	gotB, err := br.GetSessionTurns(ctx, sid)
	if err != nil {
		t.Fatalf("B.GetSessionTurns: %v", err)
	}
	if len(gotB) != 1 || gotB[0].Content != "hello from B" {
		t.Errorf("B sees wrong turns: %v", gotB)
	}
	for _, tr := range gotB {
		if tr.Content == "hello from A" {
			t.Errorf("B.GetSessionTurns leaked A's turn")
		}
	}
}

// testSessionHintsIsolated verifies that hints stored by A are not visible to
// B, and that B cannot consume A's hint.
func testSessionHintsIsolated(t *testing.T, a, b memstore.SessionStore) {
	t.Helper()
	ctx := context.Background()
	sid := "session-hints-isolation"
	cwd := "/proj/hints-isolation"

	hint := memstore.ContextHint{
		SessionID:    sid,
		CWD:          cwd,
		TurnIndex:    1,
		HintText:     "hint from A",
		Relevance:    0.9,
		Desirability: 0.8,
	}
	hintID, err := a.StoreHint(ctx, hint)
	if err != nil {
		t.Fatalf("A.StoreHint: %v", err)
	}

	// B queries same session/cwd -- should see nothing.
	bHints, err := b.GetPendingHints(ctx, sid, cwd)
	if err != nil {
		t.Fatalf("B.GetPendingHints: %v", err)
	}
	if len(bHints) != 0 {
		t.Errorf("B.GetPendingHints returned %d hints from A (expected 0)", len(bHints))
	}

	// B attempts to consume A's hint by ID -- should be a no-op (A still sees it).
	if err := b.MarkHintConsumed(ctx, hintID); err != nil {
		t.Fatalf("B.MarkHintConsumed: %v", err)
	}
	aHints, err := a.GetPendingHints(ctx, sid, cwd)
	if err != nil {
		t.Fatalf("A.GetPendingHints after B.MarkHintConsumed: %v", err)
	}
	if len(aHints) != 1 {
		t.Errorf("A's hint was consumed by B: GetPendingHints returned %d (expected 1)", len(aHints))
	}
}

// testSessionInjectionsIsolated verifies that A's injection is invisible to B.
func testSessionInjectionsIsolated(t *testing.T, a, b memstore.SessionStore) {
	t.Helper()
	ctx := context.Background()
	sid := "session-inj-isolation"
	refID := "42"
	refType := memstore.RefTypeFact

	if err := a.RecordInjection(ctx, sid, refID, refType, 0); err != nil {
		t.Fatalf("A.RecordInjection: %v", err)
	}

	// B checks same session -- should not see A's injection via WasInjected.
	injected, err := b.WasInjected(ctx, sid, refID, refType)
	if err != nil {
		t.Fatalf("B.WasInjected: %v", err)
	}
	if injected {
		t.Error("B.WasInjected returned true for A's injection")
	}

	// B checks GetInjectedFactIDs if the capability is present.
	if br, ok := b.(sessionInjectionReader); ok {
		ids, err := br.GetInjectedFactIDs(ctx, sid)
		if err != nil {
			t.Fatalf("B.GetInjectedFactIDs: %v", err)
		}
		for _, id := range ids {
			if fmt.Sprintf("%d", id) == refID {
				t.Errorf("B.GetInjectedFactIDs returned A's refID")
			}
		}
	}

	// A should still see its own injection.
	injectedA, err := a.WasInjected(ctx, sid, refID, refType)
	if err != nil {
		t.Fatalf("A.WasInjected: %v", err)
	}
	if !injectedA {
		t.Error("A.WasInjected returned false for its own injection")
	}
}

// testSessionFeedbackIsolated verifies that A's feedback is not visible to B.
func testSessionFeedbackIsolated(t *testing.T, a, b memstore.SessionStore) {
	t.Helper()
	ctx := context.Background()
	sid := "session-fb-isolation"
	refID := "99"
	refType := memstore.RefTypeFact

	fb := memstore.ContextFeedback{
		RefID:     refID,
		RefType:   refType,
		SessionID: sid,
		Score:     1,
		Reason:    "useful",
	}
	if err := a.RecordFeedback(ctx, fb); err != nil {
		t.Fatalf("A.RecordFeedback: %v", err)
	}

	// B queries FeedbackScores for the same refID -- should see nothing.
	scorer, ok := b.(memstore.FeedbackScorer)
	if !ok {
		t.Skip("B does not implement FeedbackScorer; skipping feedback isolation check")
	}
	stats, err := scorer.FeedbackScores(ctx, []string{refID}, refType)
	if err != nil {
		t.Fatalf("B.FeedbackScores: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("B.FeedbackScores returned %d entries from A (expected 0): %v", len(stats), stats)
	}
}

// --- Chunked fact embeddings ---
//
// A fact is a set of vectors rather than a point, so every fact-level
// comparison has to say what it means over that set. These pin the semantics
// for every backend.
//
// The vectors below are chosen against the mock embedder both backends use,
// which maps a single query text to [0.1, 0.2, 0.3, 0.4]: {1,2,3,4} is parallel
// to it and scores 1.0, {4,-3,2,-1} is orthogonal and scores 0, and {1,1,1,1}
// lands between them at about 0.91.

// vectorsOf bundles chunks with a whole-fact vector. Where a test does not
// care what the whole-fact vector is, chunk 0's stands in -- which is what a
// single-chunk fact produces anyway.
func vectorsOf(whole []float32, chunks ...memstore.FactChunk) memstore.FactVectors {
	if whole == nil && len(chunks) > 0 {
		whole = chunks[0].Vector
	}
	return memstore.FactVectors{Whole: whole, Chunks: chunks}
}

func chunkAt(ordinal int, vec []float32) memstore.FactChunk {
	return memstore.FactChunk{
		Ordinal:   ordinal,
		Vector:    vec,
		ByteStart: ordinal * 100,
		ByteEnd:   (ordinal + 1) * 100,
	}
}

func testFactChunkRoundTrip(t *testing.T, s memstore.Store) {
	ctx := context.Background()

	id, err := s.Insert(ctx, memstore.Fact{Content: "multi chunk fact", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.SetFactVectors(ctx, id, vectorsOf(nil,
		chunkAt(0, []float32{1, 0, 0, 0}),
		chunkAt(1, []float32{0, 1, 0, 0}),
		chunkAt(2, []float32{0, 0, 1, 0}),
	)); err != nil {
		t.Fatalf("SetFactVectors: %v", err)
	}

	got, err := s.FactChunks(ctx, id)
	if err != nil {
		t.Fatalf("FactChunks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3", len(got))
	}
	for i, c := range got {
		if c.Ordinal != i {
			t.Errorf("row %d has ordinal %d -- chunks must come back in ordinal order", i, c.Ordinal)
		}
		if c.ByteStart != i*100 || c.ByteEnd != (i+1)*100 {
			t.Errorf("chunk %d span [%d,%d) did not round-trip", i, c.ByteStart, c.ByteEnd)
		}
	}
}

// A re-embed producing fewer chunks must not leave the old high-ordinal
// vectors behind, or they keep answering searches for text the fact no longer
// has.
func testFactChunksReplaceRatherThanMerge(t *testing.T, s memstore.Store) {
	ctx := context.Background()

	id, err := s.Insert(ctx, memstore.Fact{Content: "shrinking fact", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.SetFactVectors(ctx, id, vectorsOf(nil,
		chunkAt(0, []float32{1, 0, 0, 0}),
		chunkAt(1, []float32{0, 1, 0, 0}),
		chunkAt(2, []float32{0, 0, 1, 0}),
	)); err != nil {
		t.Fatalf("SetFactVectors: %v", err)
	}
	if err := s.SetFactVectors(ctx, id, vectorsOf(nil, chunkAt(0, []float32{1, 0, 0, 0}))); err != nil {
		t.Fatalf("SetFactVectors shrink: %v", err)
	}

	got, err := s.FactChunks(ctx, id)
	if err != nil {
		t.Fatalf("FactChunks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d chunks after re-embedding to one, want 1", len(got))
	}
}

// A fact whose *second* chunk matches the query must be found, and found once.
// Ranking on chunk 0 alone misses it entirely; returning it per matching chunk
// lets one long fact fill the result set and double-counts it in the fusion.
func testSearchRanksFactsByBestChunk(t *testing.T, s memstore.Store) {
	ctx := context.Background()

	longID, err := s.Insert(ctx, memstore.Fact{Content: "zzz unrelated wording", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatalf("Insert long: %v", err)
	}
	shortID, err := s.Insert(ctx, memstore.Fact{Content: "yyy other wording", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatalf("Insert short: %v", err)
	}

	if err := s.SetFactVectors(ctx, longID, vectorsOf(nil,
		chunkAt(0, []float32{4, -3, 2, -1}), // orthogonal to the query
		chunkAt(1, []float32{1, 2, 3, 4}),   // exactly on it
	)); err != nil {
		t.Fatalf("SetFactVectors long: %v", err)
	}
	if err := s.SetFactVectors(ctx, shortID, vectorsOf(nil,
		chunkAt(0, []float32{1, 1, 1, 1}),
	)); err != nil {
		t.Fatalf("SetFactVectors short: %v", err)
	}

	results, err := s.Search(ctx, "qqq", memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
		FTSWeight:  0.0001,
		VecWeight:  1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	seen := map[int64]int{}
	var order []int64
	for _, r := range results {
		seen[r.Fact.ID]++
		order = append(order, r.Fact.ID)
	}
	if seen[longID] == 0 {
		t.Fatalf("the long fact was not found; its matching chunk is ordinal 1, "+
			"so ranking on chunk 0 alone misses it (results: %v)", order)
	}
	if seen[longID] > 1 {
		t.Errorf("the long fact appeared %d times; chunks must collapse to one hit per fact", seen[longID])
	}
	if len(order) > 0 && order[0] != longID {
		t.Errorf("ranked %v; the long fact best chunk (1.0) beats the short fact (0.91)", order)
	}
}

// Deduplication compares a new fact against an existing fact's stored marker
// and supersedes destructively at 0.85. The marker therefore has to be the
// whole-fact vector: chunk 0 is the opening passage of anything that splits,
// which scores systematically low against a whole-fact vector and would
// silently stop deduplicating long facts.
func testFactMarkerIsTheWholeFactVector(t *testing.T, s memstore.Store) {
	ctx := context.Background()

	id, err := s.Insert(ctx, memstore.Fact{Content: "a fact that splits", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	whole := []float32{9, 9, 9, 9}
	if err := s.SetFactVectors(ctx, id, memstore.FactVectors{
		Whole: whole,
		Chunks: []memstore.FactChunk{
			chunkAt(0, []float32{1, 0, 0, 0}),
			chunkAt(1, []float32{0, 1, 0, 0}),
		},
	}); err != nil {
		t.Fatalf("SetFactVectors: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Embedding) != len(whole) {
		t.Fatalf("marker has %d dims, want %d", len(got.Embedding), len(whole))
	}
	for i := range whole {
		if got.Embedding[i] != whole[i] {
			t.Fatalf("marker = %v, want the whole-fact vector %v -- storing chunk 0 here "+
				"compares an opening passage against a whole fact during supersession",
				got.Embedding, whole)
		}
	}
}

// EmbedFacts is the documented re-embed path after an import (see transfer.go),
// and it has to produce the same shape as every other embed path. Writing only
// the fact row's marker leaves the fact invisible to vector search, which reads
// chunks -- embedded as far as the queue is concerned, and unfindable in
// practice.
func testEmbedFactsProducesChunks(t *testing.T, s memstore.Store) {
	ctx := context.Background()

	id, err := s.Insert(ctx, memstore.Fact{
		Content: "a fact imported without its vectors", Subject: "S", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	n, err := s.EmbedFacts(ctx, 10)
	if err != nil {
		t.Fatalf("EmbedFacts: %v", err)
	}
	if n == 0 {
		t.Fatal("EmbedFacts embedded nothing; the fact was inserted without a vector")
	}

	chunks, err := s.FactChunks(ctx, id)
	if err != nil {
		t.Fatalf("FactChunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Error("EmbedFacts left no chunk rows; the fact is unfindable by vector search")
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Embedding) == 0 {
		t.Error("EmbedFacts left no marker; NeedingEmbedding would re-queue the fact forever")
	}
}

// testCapabilityScoping pins the contract of the capability-typed handles: a
// principal without write rights is refused a writable handle outright, and the
// readable handle it does get cannot be widened back into one.
//
// The type-assertion case is the one worth having. Every backend satisfies
// WritableStore, so a ReadableStore carrying a bare backend could be asserted
// straight back up to the full interface -- the narrowing would be a naming
// convention rather than a boundary. The scopers return a wrapper promoting
// only the read method set, and this is what says so.
func testCapabilityScoping(t *testing.T, s memstore.Store) {
	t.Helper()
	ctx := context.Background()

	sc, ok := s.(memstore.StoreScoper)
	if !ok {
		t.Skip("backend is not a StoreScoper")
	}

	reader := memstore.Principal{UserID: 1}
	writer := memstore.Principal{UserID: 1, Write: true}

	if _, err := sc.WritableFor(reader); !errors.Is(err, memstore.ErrNotPermitted) {
		t.Errorf("WritableFor(read principal): errors.Is(err, ErrNotPermitted) = false; err = %v", err)
	}

	w, err := sc.WritableFor(writer)
	if err != nil {
		t.Fatalf("WritableFor(write principal): %v", err)
	}
	id, err := w.Insert(ctx, memstore.Fact{Content: "written through a writable handle", Subject: "cap", Category: "note"})
	if err != nil {
		t.Fatalf("Insert through writable handle: %v", err)
	}

	r, err := sc.ReadableFor(reader)
	if err != nil {
		t.Fatalf("ReadableFor: %v", err)
	}
	if _, ok := r.(memstore.WritableStore); ok {
		t.Error("a readable handle asserts to WritableStore; the narrowing is advisory, not enforced")
	}
	if _, ok := r.(memstore.EmbedStore); ok {
		t.Error("a readable handle asserts to EmbedStore; the embedding pipeline's writes are reachable from a read handle")
	}

	// The read handle still has to work, including its telemetry: a
	// retrieval-only caller that cannot record a use makes its own reads
	// invisible to every metric built on use_count.
	if got, err := r.Get(ctx, id); err != nil || got == nil {
		t.Errorf("Get through readable handle: fact=%v err=%v", got, err)
	}
	if err := r.Touch(ctx, []int64{id}); err != nil {
		t.Errorf("Touch through readable handle: %v", err)
	}
}
