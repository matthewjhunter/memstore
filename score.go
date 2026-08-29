package memstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
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
	// DefaultRerankCandidates is the rerank shortlist size used when rerank is
	// enabled but SearchOpts.RerankCandidates is unset.
	DefaultRerankCandidates = 40
	// DefaultRerankWeight is rerank's fusion share used in RerankBalanced when
	// SearchOpts.RerankWeight is unset.
	DefaultRerankWeight = 0.7
	// DefaultRerankThreshold is the relevance floor applied when none is
	// configured: a reranked fact scoring below it is dropped rather than
	// padding the result set.
	//
	// Without a floor, hybrid search returns its top N no matter how badly they
	// score, and once recalled content is in the context window a weak match is
	// indistinguishable from a strong one -- there is no score attached to the
	// model's memory of having read something (#163).
	//
	// The value is calibrated against the live corpus rather than guessed.
	// Normalized rerank scores are sharply bimodal there: genuine matches scored
	// 0.38-0.88, pure nonsense 1.7e-5 to 4.3e-5. 0.05 sits roughly three orders
	// of magnitude above the noise and an order below the weakest real hit, so
	// it is not a knife-edge in either direction. The first-stage Combined score
	// separates the same two cases by only ~5x, which is why the floor keys on
	// the rerank score instead.
	DefaultRerankThreshold = 0.05
	// DefaultRerankDocBytes is the per-document truncation budget for the search
	// path when SearchOpts.RerankDocBytes is unset. Rerank cost is superlinear in
	// document length, so search caps each candidate at ~700 tokens of lead
	// content (a fact's subject and decision usually sit up front) to keep a
	// 40-candidate pass within a few seconds on CPU. The recall path sets its own
	// tighter budget for its per-prompt latency target.
	DefaultRerankDocBytes = 2800
	// rerankTieBreak scales the first-stage score into RerankDominant's rank key
	// so it only separates equal rerank scores, never overturns them.
	rerankTieBreak = 1e-6
)

// ScoreResults builds the final ranked result set from first-stage FTS and
// vector hits. It deduplicates by fact ID, computes the weighted first-stage
// relevance, optionally reranks the top opts.RerankCandidates with rr and fuses
// that relevance in, then applies the confirmation trust boost and recency
// decay before sorting and truncating to opts.MaxResults.
//
// It is shared by every backend (SQLite and Postgres) so the scoring policy
// lives in one place. Rerank runs only when rr is non-nil AND opts.RerankMode
// is enabled — so callers that don't want rerank (e.g. background extraction)
// just leave the mode off, and it reduces to the first-stage weighted sum. When
// the reranker is unreachable, or rejects the pair as too long even at the
// smallest document budget (see RerankShrinking), it degrades to that
// first-stage ordering, logs the fallback, and never applies the threshold, so
// an outage cannot empty the result set; only a caller-bug rerank error — e.g.
// a 4xx such as an unknown model — surfaces.
func ScoreResults(ctx context.Context, rr embedding.Reranker, query string, fts, vec []SearchResult, opts SearchOpts) ([]SearchResult, error) {
	merged := mergeFirstStage(fts, vec, opts)

	if rr != nil && opts.RerankMode.Enabled() {
		var err error
		merged, err = fuseRerank(ctx, rr, query, merged, opts)
		if err != nil {
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

// MinRerankDocBytes is the floor for shrinking a rerank document budget. A
// backend that still rejects the query+document pair at this size is not
// rejecting it for length, and the pass degrades instead.
const MinRerankDocBytes = 256

// RerankShrinking sends req to rr and, when the backend rejects the pair as
// too long (a *embedding.PermanentError with TooLong set -- llama-server's
// batch-size limit reports this way), halves MaxDocumentBytes and retries
// down to MinRerankDocBytes. A too-long rejection is a document budget
// problem, not an outage: shrinking keeps the rerank rather than dropping it
// for the whole result set. The final rejection is returned if the floor
// does not fit.
func RerankShrinking(ctx context.Context, rr embedding.Reranker, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	for {
		out, err := rr.Rerank(ctx, req)
		if err == nil {
			return out, nil
		}
		var pe *embedding.PermanentError
		if !errors.As(err, &pe) || !pe.TooLong {
			return nil, err
		}
		if req.MaxDocumentBytes <= MinRerankDocBytes {
			return nil, err
		}
		req.MaxDocumentBytes = max(req.MaxDocumentBytes/2, MinRerankDocBytes)
	}
}

// IsRerankDegradation reports whether a rerank error is one the search path
// absorbs by falling back to first-stage order: the backend is unavailable,
// or it rejected the pair as too long even at the smallest budget. Any other
// permanent error -- an unknown model, bad auth -- is a caller bug and
// surfaces.
func IsRerankDegradation(err error) bool {
	if !embedding.IsRerankAvailable(err) {
		return true
	}
	var pe *embedding.PermanentError
	return errors.As(err, &pe) && pe.TooLong
}

// rerankLogEvery bounds how often a degradation is logged per call site. An
// outage degrades every search; one line a minute says so without turning
// the log into a search log.
const rerankLogEvery = time.Minute

var rerankLog struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// LogRerankDegraded records that a rerank pass at site fell back to
// first-stage order, at most once per rerankLogEvery per site. Silent
// degradation was the failure mode: every search lost its rerank scores and
// nothing in the daemon log said so.
func LogRerankDegraded(site string, err error) {
	rerankLog.mu.Lock()
	defer rerankLog.mu.Unlock()
	now := time.Now
	if rerankLog.now != nil {
		now = rerankLog.now
	}
	t := now()
	if last, ok := rerankLog.last[site]; ok && t.Sub(last) < rerankLogEvery {
		return
	}
	if rerankLog.last == nil {
		rerankLog.last = map[string]time.Time{}
	}
	rerankLog.last[site] = t
	log.Printf("%s: rerank degraded to first-stage order (no threshold applied; repeats suppressed for %s): %v", site, rerankLogEvery, err)
}

// resetRerankLogLimiter clears the rate limiter so tests observe the line.
func resetRerankLogLimiter() {
	rerankLog.mu.Lock()
	defer rerankLog.mu.Unlock()
	rerankLog.last = nil
}

// FuseScore combines a first-stage relevance score with a normalized [0,1]
// rerank score according to mode. It is the single place the mode arithmetic
// lives, shared by ScoreResults and the recall pipeline so both rank
// identically. weight is rerank's share in RerankBalanced (defaulted upstream).
//
//   - RerankBalanced: weight*rerank + (1-weight)*firstStage
//   - RerankDominant: rerank, with a tie-break sliver of firstStage
//   - RerankGate:     firstStage unchanged (gate only filters, never reorders)
//   - RerankOff/other: firstStage unchanged
func FuseScore(mode RerankMode, weight, rerank, firstStage float64) float64 {
	switch mode {
	case RerankBalanced:
		return weight*rerank + (1-weight)*firstStage
	case RerankDominant:
		return rerank + rerankTieBreak*firstStage
	default: // RerankGate, RerankOff
		return firstStage
	}
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

// fuseRerank rescores the top opts.RerankCandidates of merged (already sorted by
// first-stage Combined) with rr, folds the rerank score into Combined per
// opts.RerankMode (via FuseScore), and — when opts.RerankThreshold is a
// positive value — drops
// every fact whose normalized rerank score is below it. rerankScore is expected
// on a [0,1] scale (memstore configures the reranker with NormalizeScores, so a
// raw-logit backend like llama.cpp is sigmoided upstream).
//
// It returns the surviving results. A degradation (see IsRerankDegradation)
// is absorbed and logged: the original merged slice is returned unchanged AND
// unfiltered, so an outage degrades to first-stage ordering rather than
// emptying the results. Any other rerank error surfaces.
func fuseRerank(ctx context.Context, rr embedding.Reranker, query string, merged []SearchResult, opts SearchOpts) ([]SearchResult, error) {
	if len(merged) == 0 {
		return merged, nil
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

	docBytes := opts.RerankDocBytes
	if docBytes <= 0 {
		docBytes = DefaultRerankDocBytes
	}
	results, err := RerankShrinking(ctx, rr, embedding.RerankRequest{
		Query:            query,
		Documents:        docs,
		MaxDocumentBytes: docBytes,
	})
	if err != nil {
		if IsRerankDegradation(err) {
			LogRerankDegraded("search", err)
			return merged, nil // degrade: first-stage order, no threshold filtering
		}
		return nil, fmt.Errorf("memstore: rerank: %w", err)
	}

	w := opts.RerankWeight
	if w <= 0 {
		w = DefaultRerankWeight
	}
	reranked := make([]bool, n)
	for _, res := range results {
		// Defensive: the index comes back from the reranker; never use it to
		// address the pool without bounds-checking.
		if res.Index < 0 || res.Index >= n {
			continue
		}
		merged[res.Index].RerankScore = res.Score
		merged[res.Index].Combined = FuseScore(opts.RerankMode, w, res.Score, merged[res.Index].Combined)
		reranked[res.Index] = true
	}

	// Threshold drops low-relevance facts. A fact survives only if it was
	// reranked and scored at/above the threshold; facts outside the pool (or any
	// the backend skipped) were not vouched for, so a positive threshold excludes
	// them too. A zero threshold keeps everything.
	if opts.RerankThreshold != nil && *opts.RerankThreshold > 0 {
		floor := *opts.RerankThreshold
		kept := make([]SearchResult, 0, len(merged))
		var dropped int
		var topDropped float64
		for i := range merged {
			if i < n && reranked[i] && merged[i].RerankScore >= floor {
				kept = append(kept, merged[i])
				continue
			}
			// Only reranked facts have a score worth reporting. Ones outside the
			// pool were never scored, so counting them would inflate the number
			// with facts the floor did not actually judge.
			if i < n && reranked[i] {
				dropped++
				topDropped = max(topDropped, merged[i].RerankScore)
			}
		}
		if opts.RerankStats != nil {
			opts.RerankStats.Dropped = dropped
			opts.RerankStats.TopDropped = topDropped
		}
		return kept, nil
	}
	return merged, nil
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
	n := max(opts.RerankCandidates, opts.MaxResults*2)
	return n
}
