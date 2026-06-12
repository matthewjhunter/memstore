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
	// (? placeholders for SQLite, $N for Postgres). Required for
	// HistoryCycleTerminates; if nil that subtest is skipped.
	SetSupersededBy func(t *testing.T, supersededByID, targetID int64)
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
	t.Run("UpdateMetadataMergeSemantics", func(t *testing.T) {
		testUpdateMetadataMergeSemantics(t, opts.NewStore(t))
	})
	t.Run("EmbedQuarantine", func(t *testing.T) {
		testEmbedQuarantine(t, opts.NewStore(t))
	})
	t.Run("NamespaceIsolation", func(t *testing.T) {
		if opts.NewStoreNS == nil {
			t.Skip("NewStoreNS not provided; skipping namespace isolation test")
		}
		testNamespaceIsolation(t, opts.NewStoreNS)
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
		t.Fatal("Get returned nil")
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

	// Compare metadata by value, not raw bytes -- SQLite stores TEXT, Postgres
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

	s.Insert(ctx, memstore.Fact{ //nolint:errcheck
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

	s.Insert(ctx, memstore.Fact{ //nolint:errcheck
		Content: "low", Subject: "filter", Category: "test",
		Metadata: json.RawMessage(`{"chapter":1}`),
	})
	s.Insert(ctx, memstore.Fact{ //nolint:errcheck
		Content: "high", Subject: "filter", Category: "test",
		Metadata: json.RawMessage(`{"chapter":9}`),
	})

	facts, err := s.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 3},
		},
	})
	if err != nil {
		t.Fatalf("List with valid filter: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].Content != "low" {
		t.Errorf("Content = %q, want low", facts[0].Content)
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
