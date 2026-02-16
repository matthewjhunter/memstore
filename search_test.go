package memstore_test

import (
	"context"
	"encoding/json"
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

func TestSearch_MetadataFilterEquality(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content:  "Marcus has brown eyes",
		Subject:  "Marcus",
		Category: "character",
		Metadata: json.RawMessage(`{"source_stage":"bible","chapter":1}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "The forest is dark and deep",
		Subject:  "Forest",
		Category: "setting",
		Metadata: json.RawMessage(`{"source_stage":"writer","chapter":3}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "The village has a market",
		Subject:  "Village",
		Category: "setting",
		Metadata: json.RawMessage(`{"source_stage":"bible","chapter":2}`),
	})

	// Filter by source_stage = "bible".
	results, err := store.Search(ctx, "dark forest village market", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "source_stage", Op: "=", Value: "bible"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range results {
		var m map[string]any
		json.Unmarshal(r.Fact.Metadata, &m)
		if m["source_stage"] != "bible" {
			t.Errorf("expected source_stage=bible, got %v for %q", m["source_stage"], r.Fact.Content)
		}
	}
}

func TestSearch_MetadataFilterComparison(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Event in chapter %d about the quest", i),
			Subject:  "Quest",
			Category: "event",
			Metadata: json.RawMessage(fmt.Sprintf(`{"chapter":%d}`, i)),
		})
	}

	// Filter chapter <= 3.
	results, err := store.Search(ctx, "quest chapter event", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range results {
		var m map[string]any
		json.Unmarshal(r.Fact.Metadata, &m)
		ch := m["chapter"].(float64)
		if ch > 3 {
			t.Errorf("got chapter %v, want <= 3", ch)
		}
	}
	if len(results) == 0 {
		t.Error("expected at least one result")
	}
}

func TestSearch_MetadataFilterExcludesNullMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Fact with metadata.
	store.Insert(ctx, memstore.Fact{
		Content:  "The dragon breathes fire",
		Subject:  "Dragon",
		Category: "character",
		Metadata: json.RawMessage(`{"is_draft":false}`),
	})
	// Fact without metadata — should be excluded by any metadata filter.
	store.Insert(ctx, memstore.Fact{
		Content:  "The dragon has scales",
		Subject:  "Dragon",
		Category: "character",
	})

	results, err := store.Search(ctx, "dragon", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "is_draft", Op: "=", Value: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Fact.Content != "The dragon breathes fire" {
		t.Errorf("wrong result: %q", results[0].Fact.Content)
	}
}

func TestSearch_MetadataFilterInvalidOperator(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	_, err := store.Search(ctx, "test", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "LIKE", Value: "%test%"},
		},
	})
	if err == nil {
		t.Error("expected error for invalid operator")
	}
}

func TestSearch_MetadataFilterInvalidKey(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	_, err := store.Search(ctx, "test", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "'; DROP TABLE memstore_facts; --", Op: "=", Value: 1},
		},
	})
	if err == nil {
		t.Error("expected error for invalid key")
	}
}
