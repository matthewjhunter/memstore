package pgstore_test

import (
	"context"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/timing"
)

func phaseAttrs(ctx context.Context) map[string]any {
	m := map[string]any{}
	for _, a := range timing.Attrs(ctx) {
		m[a.Key] = a.Value.Any()
	}
	return m
}

// A hybrid search is three measurable stages, and which one costs is the
// question #59 was filed about. Without this the store could stop recording
// one of them and the log line would simply be missing a field.
func TestSearchRecordsItsPhases(t *testing.T) {
	store := newTestStoreWithEmbedder(t, &mockEmbedder{dim: 4}, 4, 512)
	ctx := timing.NewContext(context.Background())

	if _, err := store.Insert(ctx, memstore.Fact{
		Content: "the quick brown fox", Subject: "animals", Category: "note",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := store.Search(ctx, "quick fox", memstore.SearchOpts{MaxResults: 5}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	m := phaseAttrs(ctx)
	for _, key := range []string{"embed_ms", "embed_calls", "fts_ms", "fts_calls", "vector_ms", "vector_calls"} {
		if _, ok := m[key]; !ok {
			t.Errorf("Search recorded no %s: %v", key, m)
		}
	}
	if got := m["fts_calls"]; got != int64(1) {
		t.Errorf("fts_calls = %v, want 1", got)
	}
}

// The FTS-only path has no embedder to spend time in, so it must not report
// one: an embed_ms of zero reads as a fast embedder rather than none.
func TestSearchFTSRecordsOnlyFTS(t *testing.T) {
	store := newTestStoreWithEmbedder(t, &mockEmbedder{dim: 4}, 4, 512)
	ctx := timing.NewContext(context.Background())

	if _, err := store.SearchFTS(ctx, "anything", memstore.SearchOpts{MaxResults: 5}); err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}

	m := phaseAttrs(ctx)
	if _, ok := m["fts_ms"]; !ok {
		t.Errorf("SearchFTS recorded no fts_ms: %v", m)
	}
	if _, ok := m["embed_ms"]; ok {
		t.Errorf("SearchFTS reported an embed phase it never ran: %v", m)
	}
	if _, ok := m["vector_ms"]; ok {
		t.Errorf("SearchFTS reported a vector phase it never ran: %v", m)
	}
}

// Recall calls the store once per keyword, so the count is what separates
// "one slow query" from "six ordinary ones".
func TestRepeatedSearchesAccumulateCalls(t *testing.T) {
	store := newTestStoreWithEmbedder(t, &mockEmbedder{dim: 4}, 4, 512)
	ctx := timing.NewContext(context.Background())

	for _, kw := range []string{"alpha", "beta", "gamma"} {
		if _, err := store.SearchFTS(ctx, kw, memstore.SearchOpts{MaxResults: 5}); err != nil {
			t.Fatalf("SearchFTS %q: %v", kw, err)
		}
	}

	if got := phaseAttrs(ctx)["fts_calls"]; got != int64(3) {
		t.Errorf("fts_calls = %v, want 3", got)
	}
}

// The store is used by the CLI and in-process callers that install no
// recorder; instrumentation must not require one.
func TestSearchWithoutRecorder(t *testing.T) {
	store := newTestStoreWithEmbedder(t, &mockEmbedder{dim: 4}, 4, 512)
	ctx := context.Background()

	if _, err := store.Search(ctx, "quick fox", memstore.SearchOpts{MaxResults: 5}); err != nil {
		t.Fatalf("Search without a recorder: %v", err)
	}
	if got := timing.Attrs(ctx); got != nil {
		t.Errorf("Attrs on a bare context = %v, want nil", got)
	}
}
