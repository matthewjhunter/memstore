package pgstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/testpg"
)

// The approximate path over fact chunk vectors. It has to return what the exact
// scan returns -- the index only makes it faster -- so most of these compare the
// two directly, which is why they sit inside the package: searchVector is called
// with a query vector rather than through Search and an embedder.

const annTestIndex = annIndexPrefix + "4"

// newANNStore opens a store with unconstrained vector columns on a fresh database,
// as production has: no deployment sets vec-dim, and with vector(4) columns the
// cast in the ANN query is a no-op the planner removes, so the query would no
// longer match the index expression. No embedder: these tests hand
// searchVector its query vector.
func newANNStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx := context.Background()
	pool := testpg.Pool(t)
	s, err := New(ctx, pool, nil, "test", 0, 0)
	if errors.Is(err, ErrNoDefaultUser) {
		if err := InitIdentity(ctx, pool, "test", "testuser"); err != nil {
			t.Fatalf("InitIdentity: %v", err)
		}
		s, err = New(ctx, pool, nil, "test", 0, 0)
	}
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedANNFacts stores n facts with random vectors. Every third fact has a second
// chunk and every tenth supersedes the one before it, so there are more chunks
// than facts and an inactive set for the filter to leave out.
func seedANNFacts(t *testing.T, s *PostgresStore, n int) {
	t.Helper()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(1, 2))
	var prev int64
	for i := range n {
		id, err := s.Insert(ctx, memstore.Fact{Content: fmt.Sprintf("ann fact %d", i), Subject: "ann", Category: "note"})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		chunks := []memstore.FactChunk{{Ordinal: 0, Vector: randVec(r), ByteEnd: 4}}
		if i%3 == 0 {
			chunks = append(chunks, memstore.FactChunk{Ordinal: 1, Vector: randVec(r), ByteStart: 2, ByteEnd: 8})
		}
		if err := s.SetFactVectors(ctx, id, memstore.FactVectors{Whole: chunks[0].Vector, Chunks: chunks}); err != nil {
			t.Fatalf("SetFactVectors: %v", err)
		}
		if i%10 == 9 {
			if err := s.Supersede(ctx, prev, id); err != nil {
				t.Fatalf("Supersede: %v", err)
			}
		}
		prev = id
	}
}

func randVec(r *rand.Rand) []float32 {
	v := make([]float32, 4)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// annIndexNames lists the ANN indexes on the fact chunk table.
func annIndexNames(t *testing.T, s *PostgresStore) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		  WHERE i.indrelid = 'memstore_fact_chunks'::regclass AND starts_with(c.relname, $1)
		  ORDER BY 1`, annIndexPrefix)
	if err != nil {
		t.Fatalf("listing indexes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scanning index name: %v", err)
		}
		names = append(names, n)
	}
	return names
}

func ensureANN(t *testing.T, s *PostgresStore) {
	t.Helper()
	s.SetANNThreshold(1)
	built, err := s.EnsureFactChunkIndex(context.Background())
	if err != nil {
		t.Fatalf("EnsureFactChunkIndex: %v", err)
	}
	if !built {
		t.Fatal("EnsureFactChunkIndex built nothing with the threshold at 1")
	}
}

func TestEnsureFactChunkIndex(t *testing.T) {
	s := newANNStore(t)
	ctx := context.Background()
	seedANNFacts(t, s, 30)

	// Below the default threshold nothing is built, and search stays exact.
	built, err := s.EnsureFactChunkIndex(ctx)
	if err != nil || built {
		t.Fatalf("below threshold: built=%v err=%v, want nothing built", built, err)
	}
	if got := annIndexNames(t, s); len(got) != 0 {
		t.Errorf("below threshold: indexes %v, want none", got)
	}
	if s.annDim() != 0 {
		t.Errorf("below threshold: annDim %d, want 0", s.annDim())
	}

	ensureANN(t, s)
	if got := annIndexNames(t, s); !slices.Equal(got, []string{annTestIndex}) {
		t.Errorf("indexes %v, want [%s]", got, annTestIndex)
	}
	if s.annDim() != 4 {
		t.Errorf("annDim %d, want 4", s.annDim())
	}

	// A second pass finds the index and leaves it alone.
	built, err = s.EnsureFactChunkIndex(ctx)
	if err != nil || built {
		t.Fatalf("second pass: built=%v err=%v, want the existing index kept", built, err)
	}
	if got := annIndexNames(t, s); !slices.Equal(got, []string{annTestIndex}) {
		t.Errorf("second pass: indexes %v, want [%s]", got, annTestIndex)
	}
}

// A store with no chunk vectors has no dimension to build for.
func TestEnsureFactChunkIndex_NoVectors(t *testing.T) {
	s := newANNStore(t)
	s.SetANNThreshold(0)
	built, err := s.EnsureFactChunkIndex(context.Background())
	if err != nil || built {
		t.Fatalf("built=%v err=%v, want nothing built", built, err)
	}
	if got := annIndexNames(t, s); len(got) != 0 {
		t.Errorf("indexes %v, want none", got)
	}
}

// The index must not change what a search returns: same facts, same order,
// same similarity.
func TestSearchVectorANNMatchesExact(t *testing.T) {
	s := newANNStore(t)
	ctx := context.Background()
	seedANNFacts(t, s, 300)

	r := rand.New(rand.NewPCG(3, 4))
	queries := make([][]float32, 12)
	for i := range queries {
		queries[i] = randVec(r)
	}
	opts := memstore.SearchOpts{MaxResults: 20, OnlyActive: true}

	exact := make([][]memstore.SearchResult, len(queries))
	for i, q := range queries {
		res, err := s.searchVector(ctx, q, opts)
		if err != nil {
			t.Fatalf("exact search: %v", err)
		}
		if len(res) == 0 {
			t.Fatalf("exact search %d returned nothing; the comparison would be vacuous", i)
		}
		exact[i] = res
	}

	ensureANN(t, s)
	if !s.useANN(opts) {
		t.Fatal("useANN is false with the index built; the comparison would run exact against exact")
	}
	for i, q := range queries {
		got, err := s.searchVector(ctx, q, opts)
		if err != nil {
			t.Fatalf("ANN search: %v", err)
		}
		assertSameResults(t, fmt.Sprintf("query %d", i), got, exact[i])
	}
	// A failed ANN query falls back to the exact scan and clears the state, so
	// matching results alone would not show the index answered them.
	if s.annDim() != 4 {
		t.Errorf("annDim %d after the searches, want 4: the ANN query failed and fell back", s.annDim())
	}
}

func assertSameResults(t *testing.T, label string, got, want []memstore.SearchResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d results, want %d", label, len(got), len(want))
		return
	}
	for j := range got {
		if got[j].Fact.ID != want[j].Fact.ID || math.Abs(got[j].VecScore-want[j].VecScore) > 1e-6 {
			t.Errorf("%s, rank %d: fact %d (%.6f), want fact %d (%.6f)",
				label, j, got[j].Fact.ID, got[j].VecScore, want[j].Fact.ID, want[j].VecScore)
		}
	}
}

// The approximate query has to be one the index can drive. Its expression is
// the cast, so a query that drifted from it would plan a scan and still pass
// every result test.
func TestANNQueryUsesIndex(t *testing.T) {
	s := newANNStore(t)
	ctx := context.Background()
	// Enough rows, with statistics, that the planner is choosing between real
	// alternatives rather than a one-row estimate.
	seedANNFacts(t, s, 2000)
	ensureANN(t, s)
	if _, err := s.pool.Exec(ctx, `ANALYZE memstore_facts; ANALYZE memstore_fact_chunks`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	b, err := s.annVectorQuery([]float32{1, 0, 0, 0}, memstore.SearchOpts{MaxResults: 5, OnlyActive: true}, 4)
	if err != nil {
		t.Fatalf("annVectorQuery: %v", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := applyANNSettings(ctx, tx); err != nil {
		t.Fatalf("applyANNSettings: %v", err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+b.q, b.args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scanning plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	if !strings.Contains(plan.String(), annTestIndex) {
		t.Errorf("plan does not use %s:\n%s", annTestIndex, plan.String())
	}
}

// A selective filter stays exact: the index would walk the whole graph looking
// for the few rows that pass, and could stop short of them.
func TestUseANN(t *testing.T) {
	s := newANNStore(t)
	now := time.Now()
	if s.useANN(memstore.SearchOpts{}) {
		t.Error("useANN with no index, want false")
	}
	s.setANNDim(4)
	tests := []struct {
		name string
		opts memstore.SearchOpts
		want bool
	}{
		{"no filters", memstore.SearchOpts{}, true},
		{"only active", memstore.SearchOpts{OnlyActive: true}, true},
		{"namespaces", memstore.SearchOpts{Namespaces: []string{"a", "b"}}, true},
		{"all namespaces", memstore.SearchOpts{AllNamespaces: true}, true},
		{"subject", memstore.SearchOpts{Subject: "x"}, false},
		{"category", memstore.SearchOpts{Category: "note"}, false},
		{"kind", memstore.SearchOpts{Kind: "task"}, false},
		{"subsystem", memstore.SearchOpts{Subsystem: "search"}, false},
		{"metadata", memstore.SearchOpts{MetadataFilters: []memstore.MetadataFilter{{Key: "k", Op: "=", Value: "v"}}}, false},
		{"created after", memstore.SearchOpts{CreatedAfter: &now}, false},
		{"created before", memstore.SearchOpts{CreatedBefore: &now}, false},
	}
	for _, tt := range tests {
		if got := s.useANN(tt.opts); got != tt.want {
			t.Errorf("%s: useANN = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// If the approximate query fails, the search still answers, exactly, and the
// store stops trying the index until the next check.
func TestSearchVectorFallsBackWhenANNFails(t *testing.T) {
	s := newANNStore(t)
	ctx := context.Background()
	seedANNFacts(t, s, 40)
	opts := memstore.SearchOpts{MaxResults: 10, OnlyActive: true}
	q := []float32{0.3, -0.2, 0.9, 0.1}

	want, err := s.searchVector(ctx, q, opts)
	if err != nil {
		t.Fatalf("exact search: %v", err)
	}
	// A dimension the vectors do not have makes the cast fail.
	s.setANNDim(8)
	got, err := s.searchVector(ctx, q, opts)
	if err != nil {
		t.Fatalf("search with a failing ANN query: %v, want the exact fallback", err)
	}
	assertSameResults(t, "fallback", got, want)
	if s.annDim() != 0 {
		t.Errorf("annDim %d after a failed ANN query, want 0", s.annDim())
	}
}

// Both paths that clear vectors drop the index: the next vectors may come from
// a model with another dimension, and an index cast to the old one would fail
// every insert.
func TestClearVectorsDropsANNIndex(t *testing.T) {
	s := newANNStore(t)
	seedANNFacts(t, s, 20)
	ensureANN(t, s)
	if err := s.clearVectors(context.Background()); err != nil {
		t.Fatalf("clearVectors: %v", err)
	}
	if got := annIndexNames(t, s); len(got) != 0 {
		t.Errorf("indexes after clearVectors: %v, want none", got)
	}
	if s.annDim() != 0 {
		t.Errorf("annDim %d after clearVectors, want 0", s.annDim())
	}
}

func TestResetEmbeddingsDropsANNIndex(t *testing.T) {
	s := newANNStore(t)
	seedANNFacts(t, s, 20)
	ensureANN(t, s)
	if _, err := ResetEmbeddings(context.Background(), s.pool); err != nil {
		t.Fatalf("ResetEmbeddings: %v", err)
	}
	if got := annIndexNames(t, s); len(got) != 0 {
		t.Errorf("indexes after ResetEmbeddings: %v, want none", got)
	}
}
