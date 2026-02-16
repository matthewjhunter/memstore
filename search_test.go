package memstore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/matthewjhunter/memstore"
)

func TestSearch_FTSBasicMatch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "Matthew prefers dark mode", Subject: "Matthew", Category: "preference"},
		{Content: "The server runs on port 8080", Subject: "Server", Category: "system"},
		{Content: "Matthew uses neovim for editing", Subject: "Matthew", Category: "preference"},
	}
	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatal(err)
	}

	results, err := store.Search(ctx, "Matthew dark mode", memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Fact.Subject != "Matthew" {
		t.Errorf("top result subject = %q, want Matthew", results[0].Fact.Subject)
	}
	if results[0].FTSScore <= 0 {
		t.Errorf("expected positive FTS score, got %f", results[0].FTSScore)
	}
}

func TestSearch_CategoryFilter(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "Matthew likes coffee", Subject: "Matthew", Category: "preference"},
		{Content: "The server likes coffee too", Subject: "Server", Category: "system"},
	}
	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatal(err)
	}

	results, err := store.Search(ctx, "coffee", memstore.SearchOpts{
		MaxResults: 10,
		Category:   "system",
		OnlyActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range results {
		if r.Fact.Category != "system" {
			t.Errorf("result category = %q, want system", r.Fact.Category)
		}
	}
}

func TestSearch_ExcludeSuperseded(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	oldID, _ := store.Insert(ctx, memstore.Fact{
		Content: "Matthew uses vim keybindings", Subject: "Matthew", Category: "preference",
	})
	newID, _ := store.Insert(ctx, memstore.Fact{
		Content: "Matthew switched to standard keybindings", Subject: "Matthew", Category: "preference",
	})
	store.Supersede(ctx, oldID, newID)

	results, err := store.Search(ctx, "Matthew keybindings", memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range results {
		if r.Fact.ID == oldID {
			t.Errorf("superseded fact %d should not appear", oldID)
		}
	}
}

func TestSearch_HybridMerge(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert a fact with an embedding so the store's embedder produces
	// a vector for the query and the vector path fires too.
	store.Insert(ctx, memstore.Fact{
		Content:   "The cat sat on the mat",
		Subject:   "Cat",
		Category:  "event",
		Embedding: []float32{0.1, 0.2, 0.3, 0.4},
	})

	results, err := store.Search(ctx, "cat sat mat", memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].FTSScore == 0 {
		t.Error("expected non-zero FTS score")
	}
	// VecScore may or may not be >0 depending on mock embedding similarity,
	// but the search should not error.
}

func TestSearch_MaxResults(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for i := range 20 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("fact number %d about testing", i),
			Subject:  "Test",
			Category: "test",
		})
	}

	results, err := store.Search(ctx, "testing", memstore.SearchOpts{
		MaxResults: 5,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) > 5 {
		t.Errorf("expected at most 5 results, got %d", len(results))
	}
}

func TestSearch_FTSOnlyWithoutEmbedder(t *testing.T) {
	store := openTestStoreWith(t, nil)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "The weather is sunny", Subject: "Weather", Category: "event",
	})

	results, err := store.Search(ctx, "sunny weather", memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one FTS result")
	}
	if results[0].VecScore != 0 {
		t.Errorf("expected zero vec score without embedder, got %f", results[0].VecScore)
	}
}
