package memstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/matthewjhunter/go-embedding"
)

// fakeReranker implements embedding.Reranker over an in-memory scoring function,
// returning results sorted by descending score like a real backend.
type fakeReranker struct {
	score    func(doc string) float64 // normalized [0,1] relevance per document
	err      error
	calls    int
	lastDocs []string
}

func (f *fakeReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	f.calls++
	f.lastDocs = append([]string(nil), req.Documents...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]embedding.RerankResult, len(req.Documents))
	for i, d := range req.Documents {
		out[i] = embedding.RerankResult{Index: i, Score: f.score(d)}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

func (f *fakeReranker) Model() string { return "fake-reranker" }

// ftsHits builds first-stage FTS results from (id, content, rawScore) triples.
func ftsHits(triples ...any) []SearchResult {
	var out []SearchResult
	for i := 0; i < len(triples); i += 3 {
		out = append(out, SearchResult{
			Fact:     Fact{ID: int64(triples[i].(int)), Content: triples[i+1].(string)},
			FTSScore: triples[i+2].(float64),
		})
	}
	return out
}

func ids(results []SearchResult) []int64 {
	out := make([]int64, len(results))
	for i, r := range results {
		out[i] = r.Fact.ID
	}
	return out
}

// ftsOnlyOpts uses FTS weight 1.0 so first-stage order is exactly the FTS rank,
// making fusion assertions easy to reason about. Balanced mode at weight 0.7.
func ftsOnlyOpts() SearchOpts {
	return SearchOpts{
		MaxResults: 10, FTSWeight: 1.0, VecWeight: 0.0,
		RerankMode: RerankBalanced, RerankWeight: 0.7, RerankCandidates: 10,
	}
}

func TestScoreResults_NoReranker_ReducesToWeightedSum(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0)

	got, err := ScoreResults(context.Background(), nil, "q", fts, nil, ftsOnlyOpts())
	if err != nil {
		t.Fatalf("ScoreResults: %v", err)
	}
	// FTS normalized to 1.0, 0.5, 0.1; no rerank → first-stage order.
	if want := []int64{1, 2, 3}; !equalIDs(ids(got), want) {
		t.Errorf("order = %v, want %v", ids(got), want)
	}
	if got[0].Combined != 1.0 {
		t.Errorf("top Combined = %v, want 1.0", got[0].Combined)
	}
}

func TestScoreResults_FusesAndReorders(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0) // first-stage: a, b, c
	rr := &fakeReranker{score: func(doc string) float64 {
		return map[string]float64{"a": 0.1, "b": 0.2, "c": 0.9}[doc] // rerank prefers c
	}}

	got, err := ScoreResults(context.Background(), rr, "q", fts, nil, ftsOnlyOpts())
	if err != nil {
		t.Fatalf("ScoreResults: %v", err)
	}
	// Combined = 0.7*rerank + 0.3*firstStage:
	//   c = 0.7*0.9 + 0.3*0.1 = 0.66 ; a = 0.7*0.1 + 0.3*1.0 = 0.37 ; b = 0.29
	if want := []int64{3, 1, 2}; !equalIDs(ids(got), want) {
		t.Fatalf("order = %v, want %v (rerank should lift c)", ids(got), want)
	}
	if d := got[0].Combined - 0.66; d > 1e-9 || d < -1e-9 {
		t.Errorf("c Combined = %v, want 0.66", got[0].Combined)
	}
	if got[0].RerankScore != 0.9 {
		t.Errorf("c RerankScore = %v, want 0.9", got[0].RerankScore)
	}
}

func TestScoreResults_DegradesWhenUnavailable(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0)
	rr := &fakeReranker{err: fmt.Errorf("%w: sidecar down", embedding.ErrRerankUnavailable)}

	got, err := ScoreResults(context.Background(), rr, "q", fts, nil, ftsOnlyOpts())
	if err != nil {
		t.Fatalf("ScoreResults should degrade, not error: %v", err)
	}
	if want := []int64{1, 2, 3}; !equalIDs(ids(got), want) {
		t.Errorf("order = %v, want first-stage %v on degrade", ids(got), want)
	}
}

func TestScoreResults_SurfacesPermanentError(t *testing.T) {
	fts := ftsHits(1, "a", 10.0)
	rr := &fakeReranker{err: errors.New("HTTP 400: unknown model")} // reachable → caller bug

	_, err := ScoreResults(context.Background(), rr, "q", fts, nil, ftsOnlyOpts())
	if err == nil {
		t.Fatal("expected a permanent rerank error to surface")
	}
}

func TestScoreResults_LimitsCandidatePool(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0)
	rr := &fakeReranker{score: func(string) float64 { return 0.5 }}
	opts := ftsOnlyOpts()
	opts.RerankCandidates = 2 // only the top-2 first-stage docs get reranked

	if _, err := ScoreResults(context.Background(), rr, "q", fts, nil, opts); err != nil {
		t.Fatalf("ScoreResults: %v", err)
	}
	if len(rr.lastDocs) != 2 {
		t.Fatalf("reranked %d docs, want 2", len(rr.lastDocs))
	}
	if rr.lastDocs[0] != "a" || rr.lastDocs[1] != "b" {
		t.Errorf("reranked docs = %v, want [a b] (top first-stage)", rr.lastDocs)
	}
}

func TestScoreResults_DominantMode(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0) // first-stage: a, b, c
	rr := &fakeReranker{score: func(doc string) float64 {
		return map[string]float64{"a": 0.1, "b": 0.2, "c": 0.9}[doc]
	}}
	opts := ftsOnlyOpts()
	opts.RerankMode = RerankDominant

	got, err := ScoreResults(context.Background(), rr, "q", fts, nil, opts)
	if err != nil {
		t.Fatalf("ScoreResults: %v", err)
	}
	// Pure rerank order (firstStage only tie-breaks): c, b, a.
	if want := []int64{3, 2, 1}; !equalIDs(ids(got), want) {
		t.Errorf("order = %v, want %v (rerank-dominant)", ids(got), want)
	}
}

func TestScoreResults_GateMode_PreservesOrderFiltersByThreshold(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0) // first-stage: a, b, c
	rr := &fakeReranker{score: func(doc string) float64 {
		return map[string]float64{"a": 0.1, "b": 0.2, "c": 0.9}[doc]
	}}
	opts := ftsOnlyOpts()
	opts.RerankMode = RerankGate
	opts.RerankThreshold = 0.15 // drops "a" (0.1); keeps b, c

	got, err := ScoreResults(context.Background(), rr, "q", fts, nil, opts)
	if err != nil {
		t.Fatalf("ScoreResults: %v", err)
	}
	// Gate keeps first-stage order (b before c) and drops a below threshold.
	if want := []int64{2, 3}; !equalIDs(ids(got), want) {
		t.Errorf("order = %v, want %v (gate preserves first-stage order, filters a)", ids(got), want)
	}
}

func TestScoreResults_ThresholdDropsLowRelevance(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0)
	rr := &fakeReranker{score: func(doc string) float64 {
		return map[string]float64{"a": 0.1, "b": 0.2, "c": 0.9}[doc]
	}}
	opts := ftsOnlyOpts() // balanced
	opts.RerankThreshold = 0.15

	got, err := ScoreResults(context.Background(), rr, "q", fts, nil, opts)
	if err != nil {
		t.Fatalf("ScoreResults: %v", err)
	}
	// a (0.1) dropped; c then b by balanced score.
	if want := []int64{3, 2}; !equalIDs(ids(got), want) {
		t.Errorf("order = %v, want %v (threshold drops a)", ids(got), want)
	}
}

func TestScoreResults_ThresholdNotAppliedOnDegrade(t *testing.T) {
	fts := ftsHits(1, "a", 10.0, 2, "b", 5.0, 3, "c", 1.0)
	// Unavailable backend with a high threshold: must NOT empty the results.
	rr := &fakeReranker{err: fmt.Errorf("%w: down", embedding.ErrRerankUnavailable)}
	opts := ftsOnlyOpts()
	opts.RerankThreshold = 0.99

	got, err := ScoreResults(context.Background(), rr, "q", fts, nil, opts)
	if err != nil {
		t.Fatalf("ScoreResults should degrade, not error: %v", err)
	}
	if want := []int64{1, 2, 3}; !equalIDs(ids(got), want) {
		t.Errorf("order = %v, want first-stage %v (no threshold filtering on degrade)", ids(got), want)
	}
}

func TestFuseScore(t *testing.T) {
	const eps = 1e-9
	cases := []struct {
		name   string
		mode   RerankMode
		rerank float64
		want   float64
	}{
		{"balanced", RerankBalanced, 0.9, 0.7*0.9 + 0.3*0.2},
		{"dominant", RerankDominant, 0.4, 0.4 + rerankTieBreak*0.2},
		{"gate keeps first-stage", RerankGate, 0.9, 0.2},
		{"off keeps first-stage", RerankOff, 0.9, 0.2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FuseScore(tc.mode, 0.7, tc.rerank, 0.2)
			if got-tc.want > eps || tc.want-got > eps {
				t.Errorf("FuseScore(%s) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

func TestParseRerankMode(t *testing.T) {
	for _, in := range []string{"", "off", "OFF", "balanced", "Dominant", "GATE"} {
		if _, err := ParseRerankMode(in); err != nil {
			t.Errorf("ParseRerankMode(%q) unexpected error: %v", in, err)
		}
	}
	if _, err := ParseRerankMode("fancy"); err == nil {
		t.Error("ParseRerankMode(\"fancy\") should error")
	}
}

func TestRerankPolicyFromEnv(t *testing.T) {
	// Prefixed values win; candidates parse to int.
	t.Setenv("MEMSTORE_RERANK_MODE", "dominant")
	t.Setenv("MEMSTORE_RERANK_THRESHOLD", "0.3")
	t.Setenv("MEMSTORE_RERANK_CANDIDATES", "24")
	t.Setenv("MEMSTORE_RERANK_RECALL_CANDIDATES", "12")
	t.Setenv("MEMSTORE_RERANK_DOC_BYTES", "2800")
	t.Setenv("MEMSTORE_RERANK_RECALL_DOC_BYTES", "1500")
	pol, err := RerankPolicyFromEnv("MEMSTORE_RERANK")
	if err != nil {
		t.Fatalf("RerankPolicyFromEnv: %v", err)
	}
	if pol.Mode != RerankDominant || pol.Threshold != 0.3 || pol.Candidates != 24 ||
		pol.RecallCandidates != 12 || pol.DocBytes != 2800 || pol.RecallDocBytes != 1500 {
		t.Errorf("got %+v, want {dominant 0.3 24 12 2800 1500}", pol)
	}

	// Cascade to the bare RERANK_* names when the prefix is unset.
	t.Setenv("MEMSTORE_RERANK_CANDIDATES", "")
	t.Setenv("RERANK_CANDIDATES", "16")
	pol, err = RerankPolicyFromEnv("MEMSTORE_RERANK")
	if err != nil {
		t.Fatalf("RerankPolicyFromEnv cascade: %v", err)
	}
	if pol.Candidates != 16 {
		t.Errorf("candidates cascade: got %d, want 16", pol.Candidates)
	}

	// A non-positive or non-numeric candidate count is an error.
	for _, bad := range []string{"0", "-5", "abc"} {
		t.Setenv("RERANK_CANDIDATES", bad)
		if _, err := RerankPolicyFromEnv("MEMSTORE_RERANK"); err == nil {
			t.Errorf("RERANK_CANDIDATES=%q should error", bad)
		}
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
