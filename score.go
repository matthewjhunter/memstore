package memstore

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/matthewjhunter/go-embedding"
)

// Rerank knobs applied when a reranker is configured but the matching
// SearchOpts field is left at its zero value. RerankCandidates follows the
// homelab design's latency guidance (rescore a shortlist of 30-50, not the
// whole result set); RerankWeight defaults rerank-leaning but not absolute, so
// the cross-encoder dominates ordering while the first-stage score still
// breaks ties and hedges a noisy rerank.
const (
	// DefaultRerankCandidates is the rerank shortlist size used when a reranker
	// is configured but SearchOpts.RerankCandidates is unset.
	DefaultRerankCandidates = 40
	// DefaultRerankWeight is rerank's fusion share used when a reranker is
	// configured but SearchOpts.RerankWeight is unset.
	DefaultRerankWeight = 0.7
)

// ScoreResults builds the final ranked result set from first-stage FTS and
// vector hits. It deduplicates by fact ID, computes the weighted first-stage
// relevance, optionally reranks the top opts.RerankCandidates with rr and fuses
// that relevance in, then applies the confirmation trust boost and recency
// decay before sorting and truncating to opts.MaxResults.
//
// It is shared by every backend (SQLite and Postgres) so the scoring policy
// lives in one place. When rr is nil or opts.RerankWeight <= 0 it reduces
// exactly to the first-stage weighted-sum behaviour. When the reranker is
// unreachable it degrades to that same first-stage ordering (see
// embedding.IsRerankAvailable); only a non-availability rerank error — e.g. a
// 4xx caller bug such as an unknown model — surfaces.
func ScoreResults(ctx context.Context, rr embedding.Reranker, query string, fts, vec []SearchResult, opts SearchOpts) ([]SearchResult, error) {
	merged := mergeFirstStage(fts, vec, opts)

	if rr != nil && opts.RerankWeight > 0 {
		if err := fuseRerank(ctx, rr, query, merged, opts); err != nil {
			return nil, err
		}
	}

	applyTrustDecay(merged, opts)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Combined > merged[j].Combined })
	if len(merged) > opts.MaxResults {
		merged = merged[:opts.MaxResults]
	}
	return merged, nil
}

// mergeFirstStage deduplicates FTS and vector hits by fact ID, min-max
// normalizes the FTS scores to [0,1] (vector cosine is already [0,1]), sets
// Combined to the weighted sum, and returns the results sorted by descending
// Combined so a reranker can select the top candidates.
func mergeFirstStage(fts, vec []SearchResult, opts SearchOpts) []SearchResult {
	byID := make(map[int64]*SearchResult)

	var maxFTS float64
	for _, r := range fts {
		if r.FTSScore > maxFTS {
			maxFTS = r.FTSScore
		}
	}
	for _, r := range fts {
		norm := r.FTSScore
		if maxFTS > 0 {
			norm = r.FTSScore / maxFTS
		}
		sr := SearchResult{Fact: r.Fact, FTSScore: norm}
		byID[r.Fact.ID] = &sr
	}
	for _, r := range vec {
		if existing, ok := byID[r.Fact.ID]; ok {
			existing.VecScore = r.VecScore
		} else {
			sr := SearchResult{Fact: r.Fact, VecScore: r.VecScore}
			byID[r.Fact.ID] = &sr
		}
	}

	merged := make([]SearchResult, 0, len(byID))
	for _, r := range byID {
		r.Combined = opts.FTSWeight*r.FTSScore + opts.VecWeight*r.VecScore
		merged = append(merged, *r)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Combined > merged[j].Combined })
	return merged
}

// fuseRerank rescores the top opts.RerankCandidates of merged (already sorted
// by first-stage Combined) with rr and blends the result into Combined:
//
//	Combined = RerankWeight*rerankScore + (1-RerankWeight)*firstStageCombined
//
// rerankScore is expected on a [0,1] scale — memstore configures the reranker
// with NormalizeScores so a raw-logit backend (llama.cpp) is sigmoided before
// it reaches here. The transform is order-preserving within the reranked pool;
// candidates outside the pool keep their first-stage Combined, which is on the
// same [0,1] scale, so the two mix coherently. merged is mutated in place.
//
// An unavailable backend is swallowed (merged keeps its first-stage order); any
// other rerank error is returned for the caller to surface.
func fuseRerank(ctx context.Context, rr embedding.Reranker, query string, merged []SearchResult, opts SearchOpts) error {
	if len(merged) == 0 {
		return nil
	}
	n := opts.RerankCandidates
	if n <= 0 {
		n = DefaultRerankCandidates
	}
	if n > len(merged) {
		n = len(merged)
	}

	docs := make([]string, n)
	for i := range docs {
		docs[i] = merged[i].Fact.Content
	}

	results, err := rr.Rerank(ctx, embedding.RerankRequest{Query: query, Documents: docs})
	if err != nil {
		if !embedding.IsRerankAvailable(err) {
			return nil // degrade to first-stage ordering
		}
		return fmt.Errorf("memstore: rerank: %w", err)
	}

	w := opts.RerankWeight
	for _, res := range results {
		// Defensive: the index comes back from the reranker; never use it to
		// address the pool without bounds-checking.
		if res.Index < 0 || res.Index >= n {
			continue
		}
		merged[res.Index].RerankScore = res.Score
		merged[res.Index].Combined = w*res.Score + (1-w)*merged[res.Index].Combined
	}
	return nil
}

// applyTrustDecay adjusts each result's Combined in place by the confirmation
// trust boost (capped at 0.15) and the per-category recency decay. It runs
// after any rerank fusion so the adjustments apply to the final relevance.
func applyTrustDecay(results []SearchResult, opts SearchOpts) {
	now := time.Now()
	for i := range results {
		if results[i].Fact.ConfirmedCount > 0 {
			results[i].Combined += min(float64(results[i].Fact.ConfirmedCount)*0.05, 0.15)
		}
		if hl := decayHalfLife(results[i].Fact.Category, opts); hl > 0 {
			age := now.Sub(results[i].Fact.CreatedAt).Seconds()
			results[i].Combined *= math.Pow(0.5, age/hl.Seconds())
		}
	}
}

// decayHalfLife returns the effective decay half-life for a fact's category.
// If CategoryDecay has an entry for the category, that value is used (0 means
// explicitly no decay). Otherwise DecayHalfLife is the fallback default.
func decayHalfLife(category string, opts SearchOpts) time.Duration {
	if opts.CategoryDecay != nil {
		if hl, ok := opts.CategoryDecay[category]; ok {
			return hl
		}
	}
	return opts.DecayHalfLife
}

// FetchLimit is how many rows each first-stage arm should return: enough to
// cover the rerank candidate pool when reranking, otherwise twice MaxResults so
// the merge has headroom. RerankCandidates is only non-zero once a reranker is
// configured, so non-rerank paths keep the original MaxResults*2 behaviour.
// Exported for Store backends to size their first-stage queries consistently.
func FetchLimit(opts SearchOpts) int {
	n := opts.MaxResults * 2
	if opts.RerankCandidates > n {
		n = opts.RerankCandidates
	}
	return n
}
