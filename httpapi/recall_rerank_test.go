package httpapi_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	_ "modernc.org/sqlite"
)

// fakeRecallReranker scores a document by a content predicate, or fails.
type fakeRecallReranker struct {
	score func(doc string) float64
	err   error
}

func (f fakeRecallReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]embedding.RerankResult, len(req.Documents))
	for i, d := range req.Documents {
		out[i] = embedding.RerankResult{Index: i, Score: f.score(d)}
	}
	return out, nil
}

func (fakeRecallReranker) Model() string { return "fake" }

func recallHandlerWithReranker(t *testing.T, rr embedding.Reranker, mode memstore.RerankMode, threshold float64) (*httpapi.Handler, *memstore.SQLiteStore) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	embedder := &mockEmbedder{dim: 4}
	store, err := memstore.NewSQLiteStore(db, embedder, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(store, embedder, "",
		httpapi.WithSessionContext(httpapi.NewSessionContext()),
		httpapi.WithReranker(rr, mode, threshold),
	)
	return h, store
}

// seedWidgetFacts inserts two facts that both match a "widget subsystem" prompt
// via FTS, so the reranker decides which is relevant. It also seeds a diverse
// base corpus so IDF keyword selection keeps "widget"/"subsystem" (with only a
// couple of docs, every shared term has degenerate/negative IDF).
func seedWidgetFacts(t *testing.T, store *memstore.SQLiteStore) {
	t.Helper()
	seedFacts(t, store)
	ctx := context.Background()
	for _, f := range []memstore.Fact{
		{Content: "the widget subsystem uses exponential backoff for retries", Subject: "widget", Category: "decision"},
		{Content: "the widget subsystem has a blue logo and a friendly mascot", Subject: "widget", Category: "decision"},
	} {
		if _, err := store.Insert(ctx, f); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
}

func recallContents(t *testing.T, h *httpapi.Handler) string {
	t.Helper()
	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "widget subsystem retry behavior",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Facts []struct {
			Content string `json:"content"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)
	var b strings.Builder
	for _, f := range result.Facts {
		b.WriteString(f.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestRecall_RerankThresholdFiltersIrrelevant(t *testing.T) {
	// Reranker likes the retry fact, not the logo fact.
	rr := fakeRecallReranker{score: func(doc string) float64 {
		if strings.Contains(doc, "backoff") {
			return 0.9
		}
		return 0.1
	}}
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0.5)
	seedWidgetFacts(t, store)

	got := recallContents(t, h)
	if !strings.Contains(got, "backoff") {
		t.Errorf("relevant retry fact missing from recall:\n%s", got)
	}
	if strings.Contains(got, "mascot") {
		t.Errorf("irrelevant fact (rerank 0.1 < threshold 0.5) should have been filtered:\n%s", got)
	}
}

func TestRecall_DegradesWhenRerankerDown(t *testing.T) {
	rr := fakeRecallReranker{err: embedding.ErrRerankUnavailable}
	// High threshold would empty results if applied — it must not be, on degrade.
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0.9)
	seedWidgetFacts(t, store)

	got := recallContents(t, h)
	if !strings.Contains(got, "widget") {
		t.Errorf("recall should still return first-stage facts when reranker is down:\n%s", got)
	}
}
