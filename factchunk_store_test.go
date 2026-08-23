package memstore_test

import (
	"context"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// The multi-chunk semantics of chunked fact embeddings: a fact is a set of
// vectors, and every fact-level comparison has to say what it means over that
// set.
//
// mockEmbedder maps a single query text to [0.1, 0.2, 0.3, 0.4], so chunk
// vectors here are chosen relative to that: a multiple of it scores 1.0, and
// [4,-3,2,-1] is orthogonal to it and scores 0.

func chunkAt(ordinal int, vec []float32) memstore.FactChunk {
	return memstore.FactChunk{
		Ordinal:   ordinal,
		Vector:    vec,
		ByteStart: ordinal * 100,
		ByteEnd:   (ordinal + 1) * 100,
	}
}

func TestSetFactChunks_RoundTripsEveryChunk(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{Content: "multi", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatal(err)
	}
	want := []memstore.FactChunk{
		chunkAt(0, []float32{1, 0, 0, 0}),
		chunkAt(1, []float32{0, 1, 0, 0}),
		chunkAt(2, []float32{0, 0, 1, 0}),
	}
	if err := store.SetFactChunks(ctx, id, want); err != nil {
		t.Fatal(err)
	}

	got, err := store.FactChunks(ctx, id)
	if err != nil {
		t.Fatal(err)
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
// vectors behind, or they keep answering searches for text the fact no
// longer has.
func TestSetFactChunks_ReplacesRatherThanMerges(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{Content: "shrinks", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetFactChunks(ctx, id, []memstore.FactChunk{
		chunkAt(0, []float32{1, 0, 0, 0}),
		chunkAt(1, []float32{0, 1, 0, 0}),
		chunkAt(2, []float32{0, 0, 1, 0}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFactChunks(ctx, id, []memstore.FactChunk{
		chunkAt(0, []float32{1, 0, 0, 0}),
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := store.FactChunks(ctx, id)
	if len(got) != 1 {
		t.Fatalf("got %d chunks after re-embedding to one, want 1", len(got))
	}
}

// The payoff. A fact whose *second* chunk matches the query must be found, and
// found once. Ranking on chunk 0 alone would miss it entirely; returning it per
// matching chunk would let one long fact fill the result set.
func TestSearch_RanksFactsByTheirBestChunk(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	longID, err := store.Insert(ctx, memstore.Fact{
		Content: "zzz unrelated wording", Subject: "S", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	shortID, err := store.Insert(ctx, memstore.Fact{
		Content: "yyy other wording", Subject: "S", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Chunk 0 is orthogonal to the query; chunk 1 is exactly on it.
	if err := store.SetFactChunks(ctx, longID, []memstore.FactChunk{
		chunkAt(0, []float32{4, -3, 2, -1}),
		chunkAt(1, []float32{1, 2, 3, 4}),
	}); err != nil {
		t.Fatal(err)
	}
	// A single chunk that matches, but less well than the long fact's chunk 1.
	if err := store.SetFactChunks(ctx, shortID, []memstore.FactChunk{
		chunkAt(0, []float32{1, 1, 1, 1}),
	}); err != nil {
		t.Fatal(err)
	}

	results, err := store.Search(ctx, "qqq", memstore.SearchOpts{
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
	if order[0] != longID {
		t.Errorf("ranked %v; the long fact's best chunk (1.0) beats the short fact's (0.91)", order)
	}
}

// A fact inserted with a precomputed whole-fact vector must be findable: the
// marker on the fact row is not what vector search reads.
func TestInsert_PrecomputedEmbeddingIsSearchable(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content: "zzz unrelated wording", Subject: "S", Category: "test",
		Embedding: []float32{1, 2, 3, 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	chunks, err := store.FactChunks(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunk rows for a precomputed vector, want 1 -- "+
			"vector search reads chunks, so the fact would be invisible", len(chunks))
	}
}
