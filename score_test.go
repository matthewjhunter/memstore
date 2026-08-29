package memstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	opts.RerankThreshold = ptrFloat(0.15) // drops "a" (0.1); keeps b, c

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
	opts.RerankThreshold = ptrFloat(0.15)

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
	opts.RerankThreshold = ptrFloat(0.99)

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

// TestRerankPolicyFromEnv_ThresholdDefault covers #163: an unconfigured store
// applied no relevance floor at all, so a query matching nothing still returned
// its ten least-bad candidates. Measured on the live corpus, rerank scores
// separate cleanly -- genuine matches ran 0.38-0.88, pure nonsense 1.7e-5 to
// 4.3e-5 -- so the default sits far above the noise and far below a real hit.
//
// An explicitly configured 0 still means off. That distinction is the reason
// the default lands here rather than in fuseRerank, where a zero threshold
// cannot be told apart from an absent one.
func TestRerankPolicyFromEnv_ThresholdDefault(t *testing.T) {
	t.Setenv("MEMSTORE_RERANK_THRESHOLD", "")
	t.Setenv("RERANK_THRESHOLD", "")
	pol, err := RerankPolicyFromEnv("MEMSTORE_RERANK")
	if err != nil {
		t.Fatalf("RerankPolicyFromEnv: %v", err)
	}
	if pol.Threshold != DefaultRerankThreshold {
		t.Errorf("unset threshold = %v, want the default %v", pol.Threshold, DefaultRerankThreshold)
	}

	// An explicit zero disables the floor; it must not be read as "unset".
	t.Setenv("MEMSTORE_RERANK_THRESHOLD", "0")
	pol, err = RerankPolicyFromEnv("MEMSTORE_RERANK")
	if err != nil {
		t.Fatalf("RerankPolicyFromEnv(explicit 0): %v", err)
	}
	if pol.Threshold != 0 {
		t.Errorf("explicit threshold 0 = %v, want 0 (floor off)", pol.Threshold)
	}

	// An explicit value still wins.
	t.Setenv("MEMSTORE_RERANK_THRESHOLD", "0.42")
	pol, err = RerankPolicyFromEnv("MEMSTORE_RERANK")
	if err != nil {
		t.Fatalf("RerankPolicyFromEnv(explicit): %v", err)
	}
	if pol.Threshold != 0.42 {
		t.Errorf("explicit threshold = %v, want 0.42", pol.Threshold)
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

// TestFuseRerank_RecordsWhatTheFloorDropped covers #170. The floor is only as
// good as the number behind it, and 0.05 was calibrated from three queries.
// Without a count, a floor set too high looks exactly like a store with little
// on the subject -- the same indistinguishability the empty-result framing
// already closes for the empty case.
//
// TopDropped is what makes the count actionable: four facts dropped at 0.049
// argues the floor is too high, four dropped at 0.0001 argues it is working.
func TestFuseRerank_RecordsWhatTheFloorDropped(t *testing.T) {
	ctx := context.Background()
	scores := map[string]float64{"a": 0.9, "b": 0.048, "c": 0.001}
	rr := &fakeReranker{score: func(doc string) float64 { return scores[doc] }}
	merged := []SearchResult{
		{Fact: Fact{Content: "a"}, Combined: 0.9},
		{Fact: Fact{Content: "b"}, Combined: 0.5},
		{Fact: Fact{Content: "c"}, Combined: 0.4},
	}

	var stats RerankStats
	opts := SearchOpts{
		RerankMode:      RerankDominant,
		RerankThreshold: ptrFloat(0.05),
		RerankStats:     &stats,
	}
	got, err := fuseRerank(ctx, rr, "q", merged, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("kept %d results, want 1", len(got))
	}
	if stats.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2", stats.Dropped)
	}
	// The near-miss, not the worst offender: that is the one that says whether
	// the floor is set too high.
	if stats.TopDropped != 0.048 {
		t.Errorf("TopDropped = %v, want 0.048 (the closest miss)", stats.TopDropped)
	}
}

// TestFuseRerank_StatsStayZeroWhenNothingDropped keeps the reporting honest at
// the other end: a search the floor did not touch must not claim it did.
func TestFuseRerank_StatsStayZeroWhenNothingDropped(t *testing.T) {
	ctx := context.Background()
	scores := map[string]float64{"a": 0.9, "b": 0.8}
	rr := &fakeReranker{score: func(doc string) float64 { return scores[doc] }}
	merged := []SearchResult{
		{Fact: Fact{Content: "a"}, Combined: 0.9},
		{Fact: Fact{Content: "b"}, Combined: 0.8},
	}

	var stats RerankStats
	got, err := fuseRerank(ctx, rr, "q", merged, SearchOpts{
		RerankMode:      RerankDominant,
		RerankThreshold: ptrFloat(0.05),
		RerankStats:     &stats,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d, want 2", len(got))
	}
	if stats.Dropped != 0 || stats.TopDropped != 0 {
		t.Errorf("stats = %+v, want zero when the floor dropped nothing", stats)
	}
}

// ptrFloat is a test helper for SearchOpts.RerankThreshold, which is a pointer
// so that nil ("no opinion") and 0 ("no floor") stay distinguishable.
func ptrFloat(f float64) *float64 { return &f }

// budgetReranker rejects any request whose MaxDocumentBytes exceeds fitsAt
// the way llama-server does for a query+document pair past its batch size:
// a *PermanentError flagged TooLong. It records each request's budget so a
// test can see the shrink ladder.
type budgetReranker struct {
	fitsAt  int
	budgets []int
}

func (b *budgetReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	b.budgets = append(b.budgets, req.MaxDocumentBytes)
	if req.MaxDocumentBytes > b.fitsAt {
		return nil, &embedding.PermanentError{
			Err:     errors.New("HTTP 500: input (1234 tokens) is too large to process"),
			TooLong: true,
		}
	}
	out := make([]embedding.RerankResult, len(req.Documents))
	for i := range req.Documents {
		out[i] = embedding.RerankResult{Index: i, Score: 0.9 - 0.1*float64(i)}
	}
	return out, nil
}

func (b *budgetReranker) Model() string { return "budget" }

// TestFuseRerank_ShrinksDocBytesOnTooLong is the 2026-08-28 case: the
// reranker rejected 2800-byte candidates as over its batch size and every
// search silently lost its rerank scores. A too-long rejection is a document
// budget problem, so the request is retried with a smaller budget rather than
// dropping rerank for the whole result set.
func TestFuseRerank_ShrinksDocBytesOnTooLong(t *testing.T) {
	rr := &budgetReranker{fitsAt: 1500}
	merged := []SearchResult{
		{Fact: Fact{Content: "a"}, Combined: 0.5},
		{Fact: Fact{Content: "b"}, Combined: 0.9},
	}
	got, err := fuseRerank(context.Background(), rr, "q", merged, SearchOpts{
		RerankMode: RerankDominant, RerankDocBytes: 2800,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.budgets) < 2 || rr.budgets[0] != 2800 {
		t.Fatalf("budgets = %v, want the configured 2800 first and at least one smaller retry", rr.budgets)
	}
	last := rr.budgets[len(rr.budgets)-1]
	if last > 1500 || last < MinRerankDocBytes {
		t.Errorf("final budget %d, want within [%d, 1500]", last, MinRerankDocBytes)
	}
	if got[0].RerankScore == 0 {
		t.Error("results were not reranked after the shrunk retry")
	}
}

// TestFuseRerank_TooLongAtFloorDegradesWithLog: when even the smallest budget
// is rejected, rerank is dropped for this search (first-stage order, no
// threshold) and the daemon says so -- silence was the bug.
func TestFuseRerank_TooLongAtFloorDegradesWithLog(t *testing.T) {
	resetRerankLogLimiter()
	rr := &budgetReranker{fitsAt: 0}
	merged := []SearchResult{
		{Fact: Fact{Content: "a"}, Combined: 0.9},
		{Fact: Fact{Content: "b"}, Combined: 0.5},
	}
	var got []SearchResult
	var err error
	logged := captureLog(t, func() {
		got, err = fuseRerank(context.Background(), rr, "q", merged, SearchOpts{
			RerankMode: RerankDominant, RerankDocBytes: 2800, RerankThreshold: ptrFloat(0.9),
		})
	})
	if err != nil {
		t.Fatalf("a too-long rejection must degrade, not fail search: %v", err)
	}
	if len(got) != 2 || got[0].Fact.Content != "a" {
		t.Errorf("degraded result = %v, want first-stage order with no floor applied", got)
	}
	if rr.budgets[len(rr.budgets)-1] != MinRerankDocBytes {
		t.Errorf("shrink stopped at %d, want the floor %d", rr.budgets[len(rr.budgets)-1], MinRerankDocBytes)
	}
	if !strings.Contains(logged, "rerank degraded") {
		t.Errorf("expected a degradation log line, got %q", logged)
	}
}

// TestFuseRerank_UnavailableDegradesWithLog: an outage still degrades, but is
// no longer silent. The line is rate-limited so a long outage does not turn
// every search into a log entry.
func TestFuseRerank_UnavailableDegradesWithLog(t *testing.T) {
	resetRerankLogLimiter()
	rr := &fakeReranker{err: embedding.ErrRerankUnavailable}
	merged := []SearchResult{{Fact: Fact{Content: "a"}, Combined: 0.9}}
	logged := captureLog(t, func() {
		for range 3 {
			if _, err := fuseRerank(context.Background(), rr, "q", merged, SearchOpts{RerankMode: RerankDominant}); err != nil {
				t.Fatal(err)
			}
		}
	})
	if n := strings.Count(logged, "rerank degraded"); n != 1 {
		t.Errorf("logged %d times across 3 searches, want 1 (rate-limited):\n%s", n, logged)
	}
}

// TestFuseRerank_OtherPermanentErrorSurfaces keeps the existing contract: a
// caller bug such as an unknown model is not a degradation and must fail loudly.
func TestFuseRerank_OtherPermanentErrorSurfaces(t *testing.T) {
	rr := &fakeReranker{err: &embedding.PermanentError{Err: errors.New("HTTP 404: unknown model")}}
	merged := []SearchResult{{Fact: Fact{Content: "a"}, Combined: 0.9}}
	if _, err := fuseRerank(context.Background(), rr, "q", merged, SearchOpts{RerankMode: RerankDominant}); err == nil {
		t.Fatal("expected a non-TooLong permanent error to surface")
	}
}
