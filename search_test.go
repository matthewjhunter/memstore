package memstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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

func TestSearch_NoEmbedder(t *testing.T) {
	store := openTestStoreWith(t, nil)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "The weather is sunny", Subject: "Weather", Category: "event",
	})

	_, err := store.Search(ctx, "sunny weather", memstore.SearchOpts{
		MaxResults: 10,
	})
	if err == nil {
		t.Fatal("expected error when no embedder configured")
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

func TestSearch_MetadataFilterIncludeNull(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Fact with chapter metadata.
	store.Insert(ctx, memstore.Fact{
		Content:  "The knight enters the castle in chapter two",
		Subject:  "Knight",
		Category: "event",
		Metadata: json.RawMessage(`{"chapter":2}`),
	})
	// Fact without chapter metadata (applies universally).
	store.Insert(ctx, memstore.Fact{
		Content:  "The knight is brave and strong",
		Subject:  "Knight",
		Category: "character",
	})
	// Fact with chapter beyond the filter range.
	store.Insert(ctx, memstore.Fact{
		Content:  "The knight defeats the dragon in chapter ten",
		Subject:  "Knight",
		Category: "event",
		Metadata: json.RawMessage(`{"chapter":10}`),
	})

	// Without IncludeNull: only the chapter-2 fact matches.
	exclusive, err := store.Search(ctx, "knight", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exclusive) != 1 {
		t.Fatalf("exclusive: got %d results, want 1", len(exclusive))
	}

	// With IncludeNull: chapter-2 fact + the no-metadata fact match.
	inclusive, err := store.Search(ctx, "knight", memstore.SearchOpts{
		MaxResults: 10,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 5, IncludeNull: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inclusive) != 2 {
		t.Fatalf("inclusive: got %d results, want 2", len(inclusive))
	}
}

func TestSearch_TemporalFilter(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert facts with explicit timestamps.
	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	store.Insert(ctx, memstore.Fact{
		Content: "Old fact about testing", Subject: "X", Category: "test", CreatedAt: old,
	})
	store.Insert(ctx, memstore.Fact{
		Content: "Mid fact about testing", Subject: "X", Category: "test", CreatedAt: mid,
	})
	store.Insert(ctx, memstore.Fact{
		Content: "Recent fact about testing", Subject: "X", Category: "test", CreatedAt: recent,
	})

	// CreatedAfter: only mid and recent.
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	results, err := store.Search(ctx, "testing", memstore.SearchOpts{
		MaxResults:   10,
		CreatedAfter: &cutoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("CreatedAfter: got %d results, want 2", len(results))
	}

	// CreatedBefore: only old.
	results, err = store.Search(ctx, "testing", memstore.SearchOpts{
		MaxResults:    10,
		CreatedBefore: &cutoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("CreatedBefore: got %d results, want 1", len(results))
	}
	if results[0].Fact.Content != "Old fact about testing" {
		t.Errorf("CreatedBefore result = %q", results[0].Fact.Content)
	}

	// Both: only mid.
	before := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	results, err = store.Search(ctx, "testing", memstore.SearchOpts{
		MaxResults:    10,
		CreatedAfter:  &cutoff,
		CreatedBefore: &before,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("range: got %d results, want 1", len(results))
	}
	if results[0].Fact.Content != "Mid fact about testing" {
		t.Errorf("range result = %q", results[0].Fact.Content)
	}
}

func TestSearch_DecayHalfLife(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)   // 30 days ago
	recent := now.Add(-1 * time.Hour)       // 1 hour ago

	// Insert two facts with identical content relevance but different ages.
	store.Insert(ctx, memstore.Fact{
		Content: "important fact about testing decay", Subject: "X", Category: "test", CreatedAt: old,
	})
	store.Insert(ctx, memstore.Fact{
		Content: "important fact about testing decay recently", Subject: "X", Category: "test", CreatedAt: recent,
	})

	// Without decay: order depends on FTS relevance (both similar).
	noDecay, err := store.Search(ctx, "testing decay", memstore.SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(noDecay) != 2 {
		t.Fatalf("no decay: got %d results, want 2", len(noDecay))
	}

	// With decay (30-day half-life): the recent fact should rank higher.
	halfLife := 30 * 24 * time.Hour
	withDecay, err := store.Search(ctx, "testing decay", memstore.SearchOpts{
		MaxResults:    10,
		DecayHalfLife: halfLife,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withDecay) != 2 {
		t.Fatalf("with decay: got %d results, want 2", len(withDecay))
	}

	// Recent fact should be ranked first with decay applied.
	if withDecay[0].Fact.CreatedAt.Before(withDecay[1].Fact.CreatedAt) {
		t.Error("expected recent fact to rank higher with decay")
	}

	// The old fact's combined score should be substantially lower.
	if withDecay[1].Combined >= withDecay[0].Combined {
		t.Errorf("old fact combined=%f should be < recent combined=%f",
			withDecay[1].Combined, withDecay[0].Combined)
	}
}

func TestSearch_DecayHalfLife_Zero(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "fact about testing no decay", Subject: "X", Category: "test",
		CreatedAt: time.Now().UTC().Add(-365 * 24 * time.Hour),
	})

	// DecayHalfLife == 0 means no decay; old facts keep full score.
	results, err := store.Search(ctx, "testing no decay", memstore.SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	// FTSScore and Combined should be equal (no decay applied).
	r := results[0]
	expected := 0.6*r.FTSScore + 0.4*r.VecScore
	if r.Combined != expected {
		t.Errorf("combined=%f, want %f (no decay)", r.Combined, expected)
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

func TestSearch_FTSColumnPrefixInQuery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content:  "START: The party enters the tavern. END: They order drinks.",
		Subject:  "Scene",
		Category: "event",
	})

	// Queries containing "WORD:" patterns would be interpreted as FTS5
	// column-prefix syntax without quoting, causing "no such column" errors.
	results, err := store.Search(ctx, "START: tavern END: drinks", memstore.SearchOpts{
		MaxResults: 10,
		OnlyActive: true,
	})
	if err != nil {
		t.Fatalf("Search with colon-prefix words: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content:  "Some fact",
		Subject:  "Test",
		Category: "test",
	})

	results, err := store.Search(ctx, "", memstore.SearchOpts{
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Search with empty query: %v", err)
	}
	// Empty query should return no FTS results (vector-only if embedder present).
	_ = results
}

func TestSearchBatch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "The cat is orange and fluffy", Subject: "Cat", Category: "character"},
		{Content: "The server runs on port 8080", Subject: "Server", Category: "system"},
		{Content: "Matthew prefers dark mode", Subject: "Matthew", Category: "preference"},
	}
	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatal(err)
	}

	results, err := store.SearchBatch(ctx, []string{"cat orange", "server port"}, memstore.SearchOpts{
		MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("SearchBatch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d result sets, want 2", len(results))
	}

	// First query should match the cat fact.
	if len(results[0]) == 0 {
		t.Fatal("expected results for query 0")
	}
	if results[0][0].Fact.Subject != "Cat" {
		t.Errorf("query 0 top result subject = %q, want Cat", results[0][0].Fact.Subject)
	}

	// Second query should match the server fact.
	if len(results[1]) == 0 {
		t.Fatal("expected results for query 1")
	}
	if results[1][0].Fact.Subject != "Server" {
		t.Errorf("query 1 top result subject = %q, want Server", results[1][0].Fact.Subject)
	}
}

func TestSearchBatch_Empty(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	results, err := store.SearchBatch(ctx, nil, memstore.SearchOpts{})
	if err != nil {
		t.Fatalf("SearchBatch empty: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil for empty queries, got %v", results)
	}
}

func TestSearchBatch_NoEmbedder(t *testing.T) {
	store := openTestStoreWith(t, nil)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "The weather is sunny", Subject: "Weather", Category: "event",
	})

	_, err := store.SearchBatch(ctx, []string{"sunny weather"}, memstore.SearchOpts{
		MaxResults: 5,
	})
	if err == nil {
		t.Fatal("expected error when no embedder configured")
	}
}

func TestSearchBatch_EmbedderError(t *testing.T) {
	store := openTestStoreWith(t, &mockEmbedder{dim: 4, err: fmt.Errorf("model loading")})
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "test fact", Subject: "X", Category: "test",
	})

	_, err := store.SearchBatch(ctx, []string{"test"}, memstore.SearchOpts{MaxResults: 5})
	if err == nil {
		t.Fatal("expected error from failing embedder")
	}
}

func TestSearchBatch_TransientEmbedderError(t *testing.T) {
	// Embedder that fails twice then succeeds on third attempt.
	embedder := &transientEmbedder{
		dim:        4,
		failsLeft:  2,
		failErr:    fmt.Errorf("connection timeout"),
	}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "The cat is orange", Subject: "Cat", Category: "test",
	})

	results, err := store.SearchBatch(ctx, []string{"cat orange"}, memstore.SearchOpts{MaxResults: 5})
	if err != nil {
		t.Fatalf("SearchBatch should succeed after retries: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d result sets, want 1", len(results))
	}
	if embedder.callCount != 3 {
		t.Errorf("embed calls = %d, want 3 (2 failures + 1 success)", embedder.callCount)
	}
}

// transientEmbedder fails a set number of times then succeeds.
type transientEmbedder struct {
	dim       int
	failsLeft int
	failErr   error
	callCount int
}

func (e *transientEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.callCount++
	if e.failsLeft > 0 {
		e.failsLeft--
		return nil, e.failErr
	}
	result := make([][]float32, len(texts))
	for i := range texts {
		emb := make([]float32, e.dim)
		for j := range emb {
			emb[j] = float32(i+1) * 0.1 * float32(j+1)
		}
		result[i] = emb
	}
	return result, nil
}

func (e *transientEmbedder) Model() string { return "transient-mock" }
