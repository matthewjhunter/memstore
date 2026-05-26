package memstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// scriptedReranker scores a document by the relevance assigned to the first
// keyword it contains, letting a test force a rerank order independent of the
// first-stage FTS rank. Scores are already in [0,1] (as a NormalizeScores-
// configured backend would return).
type scriptedReranker struct {
	scores map[string]float64
}

func (r scriptedReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	out := make([]embedding.RerankResult, len(req.Documents))
	for i, doc := range req.Documents {
		var best float64
		for kw, s := range r.scores {
			if strings.Contains(doc, kw) && s > best {
				best = s
			}
		}
		out[i] = embedding.RerankResult{Index: i, Score: best}
	}
	return out, nil
}

func (r scriptedReranker) Model() string { return "scripted" }

// TestSearch_RerankerReordersThroughStore exercises the full SQLite Search path
// with a reranker attached: defaulting, lock handling, and fusion. The
// first-stage FTS rank favours the term-dense doc, but the reranker favours a
// different doc, which fusion must lift to the top.
func TestSearch_RerankerReordersThroughStore(t *testing.T) {
	store := openTestStoreWith(t, &mockEmbedder{dim: 4})
	ctx := context.Background()

	// All three match the query "alpha"; doc1 is the densest (best FTS rank).
	dense, _ := store.Insert(ctx, memstore.Fact{Content: "alpha alpha alpha", Subject: "X", Category: "test"})
	_, _ = store.Insert(ctx, memstore.Fact{Content: "alpha beta", Subject: "X", Category: "test"})
	target, _ := store.Insert(ctx, memstore.Fact{Content: "alpha gamma target", Subject: "X", Category: "test"})

	opts := memstore.SearchOpts{MaxResults: 10, RerankMode: memstore.RerankBalanced}

	// Without a reranker, the term-dense doc should lead.
	base, err := store.Search(ctx, "alpha", opts)
	if err != nil {
		t.Fatalf("Search (no rerank): %v", err)
	}
	if len(base) == 0 || base[0].Fact.ID != dense {
		t.Fatalf("first-stage top = %v, want dense doc %d", topID(base), dense)
	}

	// Attach a reranker that strongly prefers the "target" doc.
	store.SetReranker(scriptedReranker{scores: map[string]float64{"target": 0.95, "alpha": 0.1}})

	got, err := store.Search(ctx, "alpha", opts)
	if err != nil {
		t.Fatalf("Search (rerank): %v", err)
	}
	if len(got) == 0 || got[0].Fact.ID != target {
		t.Fatalf("reranked top = %v, want target doc %d", topID(got), target)
	}
	if got[0].RerankScore != 0.95 {
		t.Errorf("target RerankScore = %v, want 0.95", got[0].RerankScore)
	}
}

// TestSearch_DegradesWhenRerankerDown confirms a Search still returns
// first-stage results when the reranker reports the backend unavailable.
func TestSearch_DegradesWhenRerankerDown(t *testing.T) {
	store := openTestStoreWith(t, &mockEmbedder{dim: 4})
	ctx := context.Background()
	dense, _ := store.Insert(ctx, memstore.Fact{Content: "alpha alpha alpha", Subject: "X", Category: "test"})
	_, _ = store.Insert(ctx, memstore.Fact{Content: "alpha gamma target", Subject: "X", Category: "test"})

	store.SetReranker(downReranker{})

	// Threshold set too: degradation must NOT apply the threshold, or an outage
	// would empty the results.
	got, err := store.Search(ctx, "alpha", memstore.SearchOpts{
		MaxResults: 10, RerankMode: memstore.RerankBalanced, RerankThreshold: 0.9,
	})
	if err != nil {
		t.Fatalf("Search should degrade, not error: %v", err)
	}
	if len(got) == 0 || got[0].Fact.ID != dense {
		t.Fatalf("degraded top = %v, want first-stage dense doc %d", topID(got), dense)
	}
}

// downReranker always reports the backend as unavailable.
type downReranker struct{}

func (downReranker) Rerank(context.Context, embedding.RerankRequest) ([]embedding.RerankResult, error) {
	return nil, embedding.ErrRerankUnavailable
}
func (downReranker) Model() string { return "down" }

func topID(rs []memstore.SearchResult) int64 {
	if len(rs) == 0 {
		return -1
	}
	return rs[0].Fact.ID
}
