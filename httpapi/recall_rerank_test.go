package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/teststore"
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

func recallHandlerWithReranker(t *testing.T, rr embedding.Reranker, mode memstore.RerankMode, threshold float64) (*httpapi.Handler, teststore.Store) {
	t.Helper()
	embedder := &mockEmbedder{dim: 4}
	store := teststore.New(t, embedder, "test")
	h := httpapi.New(store, embedder, "",
		httpapi.WithSessionContext(httpapi.NewSessionContext()),
		httpapi.WithReranker(rr, memstore.RerankPolicy{Mode: mode, Threshold: threshold}),
	)
	return h, store
}

// seedWidgetFacts inserts two facts that both match a "widget subsystem" prompt
// via FTS, so the reranker decides which is relevant. It also seeds a diverse
// base corpus so IDF keyword selection keeps "widget"/"subsystem" (with only a
// couple of docs, every shared term has degenerate/negative IDF).
func seedWidgetFacts(t *testing.T, store teststore.Store) {
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

// TestSearch_UsesDaemonThresholdWhenRequestOmitsIt covers the half of #163 that
// only shows up over HTTP: handleSearch applied the daemon's candidate pool and
// doc-byte budget but never its relevance floor, so search had no floor at all
// unless every client sent one. Recall consulted h.rerankThreshold; search did not.
func TestSearch_UsesDaemonThresholdWhenRequestOmitsIt(t *testing.T) {
	rr := fakeRecallReranker{score: func(doc string) float64 {
		if strings.Contains(doc, "backoff") {
			return 0.9
		}
		return 0.1
	}}
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0.5)
	// Search reranks through the store's reranker, not the handler's: recall
	// calls h.reranker directly, search goes through store.Search. Production
	// memstored sets both; the recall harness only sets the handler's.
	store.SetReranker(rr)
	seedWidgetFacts(t, store)

	// No rerank_threshold in the body: the daemon's configured 0.5 must apply.
	body := map[string]any{"query": "widget", "rerank_mode": "dominant"}
	var got []memstore.SearchResult
	resp := doJSON(t, h, "POST", "/v1/search", body)
	decodeJSON(t, resp, &got)

	for _, r := range got {
		if strings.Contains(r.Fact.Content, "mascot") {
			t.Errorf("fact scoring 0.1 survived the daemon floor of 0.5: %q", r.Fact.Content)
		}
	}

	// An explicit 0 is a request for no floor, and must override the daemon's.
	body["rerank_threshold"] = 0.0
	got = nil
	resp = doJSON(t, h, "POST", "/v1/search", body)
	decodeJSON(t, resp, &got)

	var sawLow bool
	for _, r := range got {
		if strings.Contains(r.Fact.Content, "mascot") {
			sawLow = true
		}
	}
	if !sawLow {
		t.Error("explicit threshold 0 did not disable the daemon floor; 0 must mean no floor")
	}
}

// TestSearch_FloorTelemetryCrossesTheWire covers #170. The floor runs
// daemon-side, so a client learns what it dropped only if the daemon says so.
// Without this the count exists but never reaches the one place it would be
// read, which is the same as not having it.
func TestSearch_FloorTelemetryCrossesTheWire(t *testing.T) {
	rr := fakeRecallReranker{score: func(doc string) float64 {
		if strings.Contains(doc, "backoff") {
			return 0.9
		}
		return 0.1
	}}
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0.5)
	store.SetReranker(rr)
	seedWidgetFacts(t, store)

	resp := doJSON(t, h, "POST", "/v1/search", map[string]any{
		"query": "widget", "rerank_mode": "dominant",
	})
	if got := resp.Header.Get(memstore.RerankDroppedHeader); got != "1" {
		t.Errorf("%s = %q, want \"1\"", memstore.RerankDroppedHeader, got)
	}
	if got := resp.Header.Get(memstore.RerankTopDroppedHeader); got != "0.1" {
		t.Errorf("%s = %q, want \"0.1\"", memstore.RerankTopDroppedHeader, got)
	}

	// Nothing dropped means no headers at all: their absence already says zero,
	// and a "dropped 0" header on every search is noise.
	zero := 0.0
	resp = doJSON(t, h, "POST", "/v1/search", map[string]any{
		"query": "widget", "rerank_mode": "dominant", "rerank_threshold": zero,
	})
	if got := resp.Header.Get(memstore.RerankDroppedHeader); got != "" {
		t.Errorf("%s = %q, want it absent when the floor dropped nothing", memstore.RerankDroppedHeader, got)
	}
}

// budgetRecallReranker rejects a request whose document budget exceeds fitsAt
// as llama-server does: a *PermanentError flagged TooLong.
type budgetRecallReranker struct {
	fitsAt  int
	budgets *[]int
	score   func(doc string) float64
}

func (b budgetRecallReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	*b.budgets = append(*b.budgets, req.MaxDocumentBytes)
	if req.MaxDocumentBytes > b.fitsAt {
		return nil, &embedding.PermanentError{Err: errors.New("HTTP 500: input is too large to process"), TooLong: true}
	}
	out := make([]embedding.RerankResult, len(req.Documents))
	for i, d := range req.Documents {
		out[i] = embedding.RerankResult{Index: i, Score: b.score(d)}
	}
	return out, nil
}

func (budgetRecallReranker) Model() string { return "budget" }

// TestRecall_ShrinksDocBytesOnTooLong mirrors the search-side test: a too-long
// rejection retries with a smaller budget, so the threshold still applies and
// the irrelevant fact is still filtered.
func TestRecall_ShrinksDocBytesOnTooLong(t *testing.T) {
	var budgets []int
	rr := budgetRecallReranker{fitsAt: 600, budgets: &budgets, score: func(doc string) float64 {
		if strings.Contains(doc, "backoff") {
			return 0.9
		}
		return 0.1
	}}
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0.5)
	seedWidgetFacts(t, store)

	got := recallContents(t, h)
	if len(budgets) < 2 || budgets[0] <= 600 {
		t.Fatalf("budgets = %v, want the default first and a smaller retry", budgets)
	}
	if !strings.Contains(got, "backoff") {
		t.Errorf("relevant fact missing after shrunk rerank:\n%s", got)
	}
	if strings.Contains(got, "mascot") {
		t.Errorf("threshold was not applied: rerank was dropped instead of shrunk:\n%s", got)
	}
}

// TestSearch_UsesDaemonRerankModeWhenRequestOmitsIt: /v1/search treated a
// missing rerank_mode as off while MCP memory_search used the daemon's
// configured mode, so the same daemon ranked differently per transport. An
// omitted mode now means the daemon's; an explicit "off" still wins.
func TestSearch_UsesDaemonRerankModeWhenRequestOmitsIt(t *testing.T) {
	rr := fakeRecallReranker{score: func(doc string) float64 {
		if strings.Contains(doc, "backoff") {
			return 0.9
		}
		return 0.1
	}}
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0.5)
	store.SetReranker(rr)
	seedWidgetFacts(t, store)

	var got []memstore.SearchResult
	decodeJSON(t, doJSON(t, h, "POST", "/v1/search", map[string]any{"query": "widget"}), &got)
	for _, r := range got {
		if strings.Contains(r.Fact.Content, "mascot") {
			t.Errorf("omitted rerank_mode should use the daemon's dominant mode and 0.5 floor; got %q", r.Fact.Content)
		}
	}
	if len(got) == 0 || got[0].RerankScore == 0 {
		t.Errorf("results carry no rerank score: the daemon mode was not applied")
	}

	got = nil
	decodeJSON(t, doJSON(t, h, "POST", "/v1/search", map[string]any{"query": "widget", "rerank_mode": "off"}), &got)
	for _, r := range got {
		if r.RerankScore != 0 {
			t.Errorf("explicit rerank_mode=off must not rerank; got score %v on %q", r.RerankScore, r.Fact.Content)
		}
	}
}
