package httpapi

import (
	"context"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/teststore"
)

type fixedEmbedder struct{ dim int }

func (e *fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, e.dim)
		out[i][0] = 1
	}
	return out, nil
}

func (e *fixedEmbedder) Model() string { return "fixed" }

func (e *fixedEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "fixed", Dim: e.dim}
}

// A word that appears in no fact has the highest possible IDF and the lowest
// possible retrieval value: it matches nothing. Keyword selection must rank
// among words that actually occur in the corpus, not hand the slots to noise.
// Regression for the 2026-08-28 case where "herald" lost to "partway".
func TestScoreAndSelectKeywords_SkipsWordsAbsentFromCorpus(t *testing.T) {
	store := teststore.New(t, &fixedEmbedder{dim: 4}, "test")
	ctx := context.Background()

	for range 3 {
		if _, err := store.Insert(ctx, memstore.Fact{Content: "herald feed parser notes", Subject: "herald", Category: "project"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 7 {
		if _, err := store.Insert(ctx, memstore.Fact{Content: "unrelated filler about other topics", Subject: "misc", Category: "project"}); err != nil {
			t.Fatal(err)
		}
	}

	got := scoreAndSelectKeywords(ctx, store, []string{"partway", "distracted", "herald", "meant", "sure", "off"})
	if len(got) != 1 || got[0] != "herald" {
		t.Fatalf("expected [herald], got %v", got)
	}

	// Nothing in the corpus at all: no keywords, rather than words that
	// cannot match anything.
	if got := scoreAndSelectKeywords(ctx, store, []string{"partway", "distracted"}); len(got) != 0 {
		t.Fatalf("expected no keywords for absent words, got %v", got)
	}
}
