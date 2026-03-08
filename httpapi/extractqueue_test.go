package httpapi

import (
	"context"
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
	for i := 0; i < 40; i++ {
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
