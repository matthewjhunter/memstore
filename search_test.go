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

	results, err := store.Search(ctx, "Matthew dark mode", nil, memstore.SearchOpts{
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

	results, err := store.Search(ctx, "coffee", nil, memstore.SearchOpts{
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

	results, err := store.Search(ctx, "Matthew keybindings", nil, memstore.SearchOpts{
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

func TestSearch_MergeDeduplication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert a fact with an embedding.
	emb := []float32{1, 0, 0}
	store.Insert(ctx, memstore.Fact{
		Content:   "The cat sat on the mat",
		Subject:   "Cat",
		Category:  "event",
		Embedding: emb,
	})

	// Search with both text and vector -- should deduplicate.
	results, err := store.Search(ctx, "cat sat mat", emb, memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 deduplicated result, got %d", len(results))
	}
	if results[0].FTSScore == 0 {
		t.Error("expected non-zero FTS score")
	}
	if results[0].VecScore == 0 {
		t.Error("expected non-zero Vec score")
	}
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

	results, err := store.Search(ctx, "testing", nil, memstore.SearchOpts{
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

func TestSearch_ConfigurableWeights(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	emb := []float32{1, 0, 0}
	store.Insert(ctx, memstore.Fact{
		Content:   "The weather is sunny",
		Subject:   "Weather",
		Category:  "event",
		Embedding: emb,
	})

	// Search with custom weights.
	results, err := store.Search(ctx, "sunny weather", emb, memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
		FTSWeight:  0.3,
		VecWeight:  0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Combined <= 0 {
		t.Errorf("expected positive combined score, got %f", results[0].Combined)
	}
}
