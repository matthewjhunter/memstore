package pgstore_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// SampleSimilarity reads what the auto-link and auto-supersede gates would
// see: the cosine between each linked pair, the cosine at a fixed rank from
// the link's source (the background floor), and the cosine between each
// superseded fact and its successor. Vectors are set directly so the numbers
// are known.
func TestSampleSimilarity_ReportsPairsAndFloor(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, tbl := range []string{"api_tokens", "memstore_links", "memstore_facts", "memstore_meta", "memstore_version", "memstore_document_chunks", "memstore_documents", "memstore_users"} {
		pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}
	emb := &mockEmbedder{dim: 4}
	if _, err := pgstore.New(ctx, pool, emb, "test", 4, 16); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatal(err)
	}
	if err := pgstore.InitIdentity(ctx, pool, "test", "testuser"); err != nil {
		t.Fatal(err)
	}
	store, err := pgstore.New(ctx, pool, emb, "test", 4, 16)
	if err != nil {
		t.Fatal(err)
	}

	insert := func(content string, vec []float32) int64 {
		id, err := store.Insert(ctx, memstore.Fact{Content: content, Subject: "s", Category: "c"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetEmbedding(ctx, id, vec); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := insert("a", []float32{1, 0, 0, 0})
	b := insert("b", []float32{0.8, 0.6, 0, 0}) // cos(a,b) = 0.8
	insert("c", []float32{0, 1, 0, 0})          // cos(a,c) = 0
	old := insert("old", []float32{1, 0, 0, 0})
	successor := insert("new", []float32{0.6, 0.8, 0, 0}) // cos(old,new) = 0.6; also cos(a,new) = 0.6

	if _, err := store.LinkFacts(ctx, a, b, "related", true, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Supersede(ctx, old, successor); err != nil {
		t.Fatal(err)
	}

	// Floor at rank 2 from a, over active facts other than a: b (0.8), new (0.6), c (0).
	sample, err := pgstore.SampleSimilarity(ctx, pool, "test", 10, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Linked) != 1 || len(sample.Superseded) != 1 {
		t.Fatalf("got %d linked, %d superseded pairs; want 1 and 1", len(sample.Linked), len(sample.Superseded))
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-3 }
	if l := sample.Linked[0]; l.SourceID != a || l.TargetID != b || !near(l.Cosine, 0.8) || !near(l.Floor, 0.6) {
		t.Errorf("linked pair: %+v, want a->b cosine 0.8 floor 0.6", l)
	}
	if s := sample.Superseded[0]; s.SourceID != old || s.TargetID != successor || !near(s.Cosine, 0.6) {
		t.Errorf("superseded pair: %+v, want old->new cosine 0.6", s)
	}
	if sample.Model != "mock" {
		t.Errorf("model = %q, want mock", sample.Model)
	}

	// With a query embedder the link side is scored from the query-side vector
	// of the source content, which is what the auto-link gate compares. Embed
	// "a" as [0,1,0,0]: against b 0.6, and the rank-2 floor over active facts
	// (c 1.0, new 0.8, b 0.6) is 0.8.
	asQuery := func(_ context.Context, content string) ([]float32, error) {
		if content != "a" {
			t.Fatalf("queryEmbed called with %q, want the source content", content)
		}
		return []float32{0, 1, 0, 0}, nil
	}
	sample, err = pgstore.SampleSimilarity(ctx, pool, "test", 10, 2, asQuery)
	if err != nil {
		t.Fatal(err)
	}
	if l := sample.Linked[0]; !near(l.Cosine, 0.6) || !near(l.Floor, 0.8) {
		t.Errorf("query-side linked pair: %+v, want cosine 0.6 floor 0.8", l)
	}
	if s := sample.Superseded[0]; !near(s.Cosine, 0.6) {
		t.Errorf("supersession must stay stored-to-stored: %+v", s)
	}
}
