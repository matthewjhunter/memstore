package httpapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// --- hint pipeline unit tests ---

// scoringGenerator returns a fixed JSON score response.
type scoringGenerator struct {
	resp string
}

func (g *scoringGenerator) Generate(_ context.Context, _ string) (string, error) {
	return g.resp, nil
}
func (g *scoringGenerator) GenerateJSON(_ context.Context, _ string) (string, error) {
	return g.resp, nil
}
func (g *scoringGenerator) Model() string { return "mock" }

func TestScoreDesirability_Valid(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `{"score": 2, "reason": "debugging in progress"}`}}
	score, reason, err := q.scoreDesirability(context.Background(), "[user]: it keeps failing")
	if err != nil {
		t.Fatal(err)
	}
	if score != 2 {
		t.Errorf("expected score 2, got %.1f", score)
	}
	if reason != "debugging in progress" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestScoreDesirability_Clamped(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `{"score": 5}`}}
	score, _, err := q.scoreDesirability(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if score != 3 {
		t.Errorf("expected clamped score 3, got %.1f", score)
	}
}

func TestScoreDesirability_BadJSON(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `not json`}}
	_, _, err := q.scoreDesirability(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for bad JSON response")
	}
}

func TestBuildSearchQuery_LastThreeUserTurns(t *testing.T) {
	turns := []memstore.SessionTurn{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
		{Role: "user", Content: "third"},
		{Role: "user", Content: "fourth"},
	}
	q := buildSearchQuery(turns)
	// Should include the 3 most recent user turns (fourth, third, second).
	if !strings.Contains(q, "fourth") {
		t.Error("expected most recent user turn in query")
	}
	if strings.Contains(q, "first") {
		t.Error("oldest user turn should be excluded (only last 3)")
	}
}

func TestBuildSearchQuery_Empty(t *testing.T) {
	q := buildSearchQuery(nil)
	if q != "" {
		t.Errorf("expected empty query, got %q", q)
	}
}

func TestBuildScoreSnippet_LastThree(t *testing.T) {
	turns := []memstore.SessionTurn{
		{Role: "user", Content: "old"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	snippet := buildScoreSnippet(turns)
	if strings.Contains(snippet, "old") {
		t.Error("oldest turn should be excluded from snippet")
	}
	if !strings.Contains(snippet, "[user]: a") {
		t.Errorf("expected turn 'a' in snippet: %q", snippet)
	}
}

func TestAvgVecScore_Empty(t *testing.T) {
	if got := avgVecScore(nil); got != 0 {
		t.Errorf("expected 0 for empty results, got %v", got)
	}
}

func TestAvgVecScore_Mean(t *testing.T) {
	results := []memstore.SearchResult{
		{VecScore: 0.8},
		{VecScore: 0.6},
	}
	if got := avgVecScore(results); got != 0.7 {
		t.Errorf("expected 0.7, got %v", got)
	}
}

func TestProjectNameFromCWD_GitRoot(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myproject")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// CWD is a subdirectory of the repo.
	sub := filepath.Join(repo, "pkg", "foo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	name := projectNameFromCWD(sub)
	if name != "myproject" {
		t.Errorf("expected %q, got %q", "myproject", name)
	}
}

func TestProjectNameFromCWD_NoGit(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "somedir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	name := projectNameFromCWD(sub)
	if name != "somedir" {
		t.Errorf("expected %q, got %q", "somedir", name)
	}
}

func TestProjectNameFromCWD_Empty(t *testing.T) {
	name := projectNameFromCWD("")
	if name != "unknown" {
		t.Errorf("expected %q, got %q", "unknown", name)
	}
}

func TestBuildCorpus_Basic(t *testing.T) {
	turns := []memstore.SessionTurn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}
	corpus := buildCorpus(turns)
	if corpus == "" {
		t.Fatal("expected non-empty corpus")
	}
	if corpus != "[user]: hello\n---\n[assistant]: world" {
		t.Errorf("unexpected corpus: %q", corpus)
	}
}

func TestBuildCorpus_Truncation(t *testing.T) {
	// Build turns that would exceed 32 KB in aggregate.
	bigContent := strings.Repeat("x", 1024) // 1 KB each
	var turns []memstore.SessionTurn
	for range 40 {
		turns = append(turns, memstore.SessionTurn{
			Role:    "user",
			Content: bigContent,
		})
	}
	corpus := buildCorpus(turns)
	if len(corpus) > 32*1024+100 {
		t.Errorf("corpus too large: %d bytes", len(corpus))
	}
}

func TestBuildCorpus_TailFirst(t *testing.T) {
	// The last turn should always appear in the corpus even if early turns are large.
	bigContent := strings.Repeat("x", 5*1024) // 5 KB — exceeds per-turn cap, gets truncated
	turns := []memstore.SessionTurn{
		{Role: "user", Content: bigContent},
		{Role: "user", Content: bigContent},
		{Role: "user", Content: bigContent},
		{Role: "user", Content: bigContent},
		{Role: "assistant", Content: "final answer"},
	}
	corpus := buildCorpus(turns)
	if !strings.Contains(corpus, "final answer") {
		t.Errorf("corpus should contain the last turn; got: %q", corpus[:min(len(corpus), 200)])
	}
}

func TestBuildCorpus_PerTurnCap(t *testing.T) {
	// A single turn exceeding maxTurnBytes should be truncated, not dropped.
	huge := strings.Repeat("a", 10*1024)
	turns := []memstore.SessionTurn{
		{Role: "user", Content: huge},
	}
	corpus := buildCorpus(turns)
	if corpus == "" {
		t.Fatal("expected non-empty corpus for single large turn")
	}
	if len(corpus) > 32*1024 {
		t.Errorf("corpus too large: %d bytes", len(corpus))
	}
}

func TestBuildCorpus_Empty(t *testing.T) {
	corpus := buildCorpus(nil)
	if corpus != "" {
		t.Errorf("expected empty corpus for nil turns, got %q", corpus)
	}
}

// --- rateHint unit tests ---

func TestRateHint_Valid(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `{"score": -1, "reason": "off-topic hint"}`}}
	score, reason, err := q.rateHint(context.Background(), "hint text", "[user]: unrelated work")
	if err != nil {
		t.Fatal(err)
	}
	if score != -1 {
		t.Errorf("expected score -1, got %d", score)
	}
	if reason != "off-topic hint" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestRateHint_PositiveScore(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `{"score": 1, "reason": "directly relevant"}`}}
	score, reason, err := q.rateHint(context.Background(), "hint text", "[user]: related work")
	if err != nil {
		t.Fatal(err)
	}
	if score != 1 {
		t.Errorf("expected score 1, got %d", score)
	}
	if reason != "directly relevant" {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestRateHint_BadJSON(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `not json`}}
	score, _, err := q.rateHint(context.Background(), "hint text", "snippet")
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
	// Default to +1 on parse failure.
	if score != 1 {
		t.Errorf("expected default score 1, got %d", score)
	}
}

func TestRateHint_UnexpectedScoreDefaultsToOne(t *testing.T) {
	q := &ExtractQueue{generator: &scoringGenerator{resp: `{"score": 99, "reason": "weird"}`}}
	score, _, err := q.rateHint(context.Background(), "hint text", "snippet")
	if err != nil {
		t.Fatal(err)
	}
	if score != 1 {
		t.Errorf("expected score clamped to 1, got %d", score)
	}
}

// --- autoRateHints unit tests ---

// fakeHintRater implements hintRater for testing.
type fakeHintRater struct {
	hints    []memstore.ContextHint
	factIDs  []int64
	err      error
	feedback []memstore.ContextFeedback
}

func (f *fakeHintRater) GetInjectedHints(_ context.Context, _ string) ([]memstore.ContextHint, error) {
	return f.hints, f.err
}
func (f *fakeHintRater) GetInjectedFactIDs(_ context.Context, _ string) ([]int64, error) {
	return f.factIDs, f.err
}
func (f *fakeHintRater) RecordFeedback(_ context.Context, fb memstore.ContextFeedback) error {
	f.feedback = append(f.feedback, fb)
	return nil
}

func TestAutoRateHints_NoHints(t *testing.T) {
	rater := &fakeHintRater{}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `{"score": 1, "reason": "fine"}`},
		rater:     rater,
	}
	job := extractJob{
		SessionID: "sess-1",
		Turns: []memstore.SessionTurn{
			{Role: "user", Content: "hello"},
		},
	}
	q.autoRateHints(context.Background(), job)
	if len(rater.feedback) != 0 {
		t.Errorf("expected no feedback calls, got %d", len(rater.feedback))
	}
}

func TestAutoRateHints_RatesAll(t *testing.T) {
	rater := &fakeHintRater{
		hints: []memstore.ContextHint{
			{ID: 10, HintText: "hint A"},
			{ID: 11, HintText: "hint B"},
		},
	}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `{"score": 1, "reason": "relevant"}`},
		rater:     rater,
	}
	job := extractJob{
		SessionID: "sess-2",
		Turns: []memstore.SessionTurn{
			{Role: "user", Content: "working on it"},
			{Role: "assistant", Content: "ok"},
		},
	}
	q.autoRateHints(context.Background(), job)
	if len(rater.feedback) != 2 {
		t.Fatalf("expected 2 feedback records, got %d", len(rater.feedback))
	}
	if rater.feedback[0].RefID != "10" {
		t.Errorf("expected ref_id 10, got %q", rater.feedback[0].RefID)
	}
	if rater.feedback[1].RefID != "11" {
		t.Errorf("expected ref_id 11, got %q", rater.feedback[1].RefID)
	}
	for _, fb := range rater.feedback {
		if fb.RefType != memstore.RefTypeHint {
			t.Errorf("expected ref_type %q, got %q", memstore.RefTypeHint, fb.RefType)
		}
		if fb.SessionID != "sess-2" {
			t.Errorf("expected session_id sess-2, got %q", fb.SessionID)
		}
		if fb.Score != 1 {
			t.Errorf("expected score 1, got %d", fb.Score)
		}
	}
}

// --- fact auto-rating unit tests ---

// fakeFactStore is a minimal Store stub for autoRateFacts testing.
// Only Get is implemented; all other methods panic.
type fakeFactStore struct {
	memstore.Store
	facts map[int64]*memstore.Fact
}

func (s *fakeFactStore) Get(_ context.Context, id int64) (*memstore.Fact, error) {
	f, ok := s.facts[id]
	if !ok {
		return nil, fmt.Errorf("fact %d not found", id)
	}
	return f, nil
}

// fakeBackfillRater extends fakeHintRater with backfill query support.
type fakeBackfillRater struct {
	fakeHintRater
	sessions map[string][]memstore.SessionTurn // sessionID -> turns
}

func (f *fakeBackfillRater) UnratedFactSessions(_ context.Context) ([]string, error) {
	var ids []string
	for id := range f.sessions {
		ids = append(ids, id)
	}
	return ids, nil
}

func (f *fakeBackfillRater) GetSessionTurns(_ context.Context, sessionID string) ([]memstore.SessionTurn, error) {
	turns, ok := f.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	return turns, nil
}

func TestAutoRateFacts_NoFacts(t *testing.T) {
	rater := &fakeHintRater{}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[{"id": 1, "score": 1, "reason": "ok"}]`},
		rater:     rater,
	}
	job := extractJob{
		SessionID: "sess-f1",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "hello"}},
	}
	q.autoRateFacts(context.Background(), job)
	if len(rater.feedback) != 0 {
		t.Errorf("expected no feedback calls, got %d", len(rater.feedback))
	}
}

func TestAutoRateFacts_RatesAll(t *testing.T) {
	rater := &fakeHintRater{
		factIDs: []int64{42, 87},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		42: {ID: 42, Content: "memstore daemon architecture"},
		87: {ID: 87, Content: "homelab infrastructure details"},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[{"id": 42, "score": 1, "reason": "relevant"}, {"id": 87, "score": -1, "reason": "off-topic"}]`},
		rater:     rater,
		store:     store,
	}
	job := extractJob{
		SessionID: "sess-f2",
		Turns: []memstore.SessionTurn{
			{Role: "user", Content: "working on memstore recall"},
			{Role: "assistant", Content: "looking at the code"},
		},
	}
	q.autoRateFacts(context.Background(), job)
	if len(rater.feedback) != 2 {
		t.Fatalf("expected 2 feedback records, got %d", len(rater.feedback))
	}
	// Check that fact 42 got +1 and fact 87 got -1.
	for _, fb := range rater.feedback {
		if fb.RefType != memstore.RefTypeFact {
			t.Errorf("expected ref_type %q, got %q", memstore.RefTypeFact, fb.RefType)
		}
		if fb.SessionID != "sess-f2" {
			t.Errorf("expected session_id sess-f2, got %q", fb.SessionID)
		}
	}
	if rater.feedback[0].RefID != "42" || rater.feedback[0].Score != 1 {
		t.Errorf("expected fact 42 score +1, got %+v", rater.feedback[0])
	}
	if rater.feedback[1].RefID != "87" || rater.feedback[1].Score != -1 {
		t.Errorf("expected fact 87 score -1, got %+v", rater.feedback[1])
	}
}

func TestAutoRateFacts_EmptyTurnsSkips(t *testing.T) {
	rater := &fakeHintRater{
		factIDs: []int64{1},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		1: {ID: 1, Content: "some fact"},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[{"id": 1, "score": 1, "reason": "ok"}]`},
		rater:     rater,
		store:     store,
	}
	job := extractJob{SessionID: "sess-f3", Turns: nil}
	q.autoRateFacts(context.Background(), job)
	if len(rater.feedback) != 0 {
		t.Errorf("expected no feedback for empty turns, got %d", len(rater.feedback))
	}
}

func TestAutoRateFacts_ParseFailureDefaultsToPositive(t *testing.T) {
	rater := &fakeHintRater{
		factIDs: []int64{10, 20},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		10: {ID: 10, Content: "fact A"},
		20: {ID: 20, Content: "fact B"},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `not valid json`},
		rater:     rater,
		store:     store,
	}
	job := extractJob{
		SessionID: "sess-f4",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "doing stuff"}},
	}
	q.autoRateFacts(context.Background(), job)
	if len(rater.feedback) != 2 {
		t.Fatalf("expected 2 feedback records on parse failure, got %d", len(rater.feedback))
	}
	for _, fb := range rater.feedback {
		if fb.Score != 1 {
			t.Errorf("expected default score +1, got %d for ref %s", fb.Score, fb.RefID)
		}
	}
}

func TestAutoRateFacts_MissingFactDefaultsToPositive(t *testing.T) {
	rater := &fakeHintRater{
		factIDs: []int64{10, 20},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		10: {ID: 10, Content: "fact A"},
		20: {ID: 20, Content: "fact B"},
	}}
	// LLM only rates fact 10, omits fact 20.
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[{"id": 10, "score": -1, "reason": "irrelevant"}]`},
		rater:     rater,
		store:     store,
	}
	job := extractJob{
		SessionID: "sess-f5",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "hello"}},
	}
	q.autoRateFacts(context.Background(), job)
	if len(rater.feedback) != 2 {
		t.Fatalf("expected 2 feedback records, got %d", len(rater.feedback))
	}
	if rater.feedback[0].Score != -1 {
		t.Errorf("expected fact 10 score -1, got %d", rater.feedback[0].Score)
	}
	// Fact 20 was omitted by LLM — should default to +1.
	if rater.feedback[1].Score != 1 {
		t.Errorf("expected fact 20 default score +1, got %d", rater.feedback[1].Score)
	}
}

func TestAutoRateHints_EmptyTurnsSkips(t *testing.T) {
	rater := &fakeHintRater{
		hints: []memstore.ContextHint{{ID: 5, HintText: "some hint"}},
	}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `{"score": 1, "reason": "fine"}`},
		rater:     rater,
	}
	job := extractJob{SessionID: "sess-3", Turns: nil}
	q.autoRateHints(context.Background(), job)
	// No snippet means no rating calls.
	if len(rater.feedback) != 0 {
		t.Errorf("expected no feedback for empty turns, got %d", len(rater.feedback))
	}
}

// --- backfill-feedback tests ---

func TestBackfillFeedback_ProcessesMultipleSessions(t *testing.T) {
	rater := &fakeBackfillRater{
		fakeHintRater: fakeHintRater{
			factIDs: []int64{42},
		},
		sessions: map[string][]memstore.SessionTurn{
			"sess-a": {{Role: "user", Content: "working on memstore"}},
			"sess-b": {{Role: "user", Content: "working on herald"}},
		},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		42: {ID: 42, Content: "some fact"},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[{"id": 42, "score": 1, "reason": "relevant"}]`},
		rater:     rater,
		store:     store,
	}

	result, err := q.BackfillFeedback(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 2 {
		t.Errorf("expected 2 sessions processed, got %d", result.Sessions)
	}
	if result.Rated != 2 {
		t.Errorf("expected 2 ratings, got %d", result.Rated)
	}
	if result.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", result.Errors)
	}
}

func TestBackfillFeedback_ReportsProgress(t *testing.T) {
	rater := &fakeBackfillRater{
		fakeHintRater: fakeHintRater{
			factIDs: []int64{1},
		},
		sessions: map[string][]memstore.SessionTurn{
			"sess-x": {{Role: "user", Content: "hello"}},
		},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		1: {ID: 1, Content: "fact"},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[{"id": 1, "score": -1, "reason": "off-topic"}]`},
		rater:     rater,
		store:     store,
	}

	var progressCalls []int
	_, err := q.BackfillFeedback(context.Background(), func(done, total int) {
		progressCalls = append(progressCalls, done)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progressCalls) != 1 {
		t.Errorf("expected 1 progress call, got %d", len(progressCalls))
	}
}

func TestBackfillFeedback_NoBackfillRater(t *testing.T) {
	// Plain hintRater (not backfillRater) should fail gracefully.
	rater := &fakeHintRater{}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `[]`},
		rater:     rater,
	}

	_, err := q.BackfillFeedback(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for non-backfill rater")
	}
}
