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
	// Build turns that would exceed the 120 KB total cap in aggregate.
	bigContent := strings.Repeat("x", 4*1024) // 4 KB each
	var turns []memstore.SessionTurn
	for range 50 {
		turns = append(turns, memstore.SessionTurn{
			Role:    "user",
			Content: bigContent,
		})
	}
	corpus := buildCorpus(turns)
	// Allow a small slack for chunk delimiters and role prefixes.
	if len(corpus) > 120*1024+200 {
		t.Errorf("corpus too large: %d bytes (cap 120 KB)", len(corpus))
	}
	// And it should not be trivially tiny — confirm we pulled multiple turns.
	if len(corpus) < 50*1024 {
		t.Errorf("corpus too small: %d bytes (expected near cap)", len(corpus))
	}
}

func TestBuildCorpus_TailFirst(t *testing.T) {
	// The last turn should always appear in the corpus even if early turns are large.
	bigContent := strings.Repeat("x", 20*1024) // 20 KB — exceeds 16 KB per-turn cap, gets truncated
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
	// A single turn exceeding maxTurnBytes (16 KB) should be truncated, not dropped.
	huge := strings.Repeat("a", 40*1024)
	turns := []memstore.SessionTurn{
		{Role: "user", Content: huge},
	}
	corpus := buildCorpus(turns)
	if corpus == "" {
		t.Fatal("expected non-empty corpus for single large turn")
	}
	// The single turn should be truncated to ~16 KB plus role prefix and ellipsis.
	if len(corpus) > 17*1024 {
		t.Errorf("corpus too large: %d bytes (expected per-turn truncation to ~16 KB)", len(corpus))
	}
}

func TestBuildCorpus_Empty(t *testing.T) {
	corpus := buildCorpus(nil)
	if corpus != "" {
		t.Errorf("expected empty corpus for nil turns, got %q", corpus)
	}
}

// --- summary pipeline unit tests ---

func TestParseSummaryResponse_Direct(t *testing.T) {
	raw := `{"outcome":"ok","lead":"Refactored auth.","decisions":["use oidclient"],"outcomes":["herald commit"]}`
	resp, ok := parseSummaryResponse(raw)
	if !ok {
		t.Fatal("expected parse success")
	}
	if resp.Outcome != "ok" {
		t.Errorf("outcome: got %q", resp.Outcome)
	}
	if resp.Lead != "Refactored auth." {
		t.Errorf("lead: got %q", resp.Lead)
	}
	if len(resp.Decisions) != 1 || resp.Decisions[0] != "use oidclient" {
		t.Errorf("decisions: got %v", resp.Decisions)
	}
	if len(resp.Outcomes) != 1 || resp.Outcomes[0] != "herald commit" {
		t.Errorf("outcomes: got %v", resp.Outcomes)
	}
}

func TestParseSummaryResponse_FencedMarkdown(t *testing.T) {
	raw := "```json\n{\"outcome\":\"ok\",\"lead\":\"Did stuff.\"}\n```"
	resp, ok := parseSummaryResponse(raw)
	if !ok {
		t.Fatal("expected parse success despite fences")
	}
	if resp.Lead != "Did stuff." {
		t.Errorf("lead: got %q", resp.Lead)
	}
}

func TestParseSummaryResponse_PreambleProse(t *testing.T) {
	raw := `Here is the JSON: {"outcome":"trivial","lead":"hello world greeting"}`
	resp, ok := parseSummaryResponse(raw)
	if !ok {
		t.Fatal("expected parse success despite preamble")
	}
	if resp.Outcome != "trivial" {
		t.Errorf("outcome: got %q", resp.Outcome)
	}
}

func TestParseSummaryResponse_FormatLapse(t *testing.T) {
	// id=968-style: model addresses the user instead of conforming to the schema.
	raw := "The conversation summary is too long to fit in a single response. Can you please provide a shorter summary?"
	if _, ok := parseSummaryResponse(raw); ok {
		t.Fatal("format lapse should not parse to a valid envelope")
	}
}

func TestParseSummaryResponse_MissingOutcome(t *testing.T) {
	// Valid JSON but no outcome field — still a format lapse.
	raw := `{"lead":"some lead","decisions":["a"]}`
	if _, ok := parseSummaryResponse(raw); ok {
		t.Fatal("missing outcome should fail parse")
	}
}

func TestParseSummaryResponse_Empty(t *testing.T) {
	if _, ok := parseSummaryResponse(""); ok {
		t.Fatal("empty response should not parse")
	}
	if _, ok := parseSummaryResponse("   \n  "); ok {
		t.Fatal("whitespace response should not parse")
	}
}

func TestRenderSummary_Full(t *testing.T) {
	resp := &summaryResponse{
		Outcome:   "ok",
		Lead:      "Switched embedding backend.",
		Decisions: []string{"cube primary", "quad failover"},
		Outcomes:  []string{"olla config updated", "facts 2528, 2529 stored"},
	}
	got := renderSummary(resp)
	want := `Switched embedding backend.

Decisions:
- cube primary
- quad failover

Outcomes:
- olla config updated
- facts 2528, 2529 stored`
	if got != want {
		t.Errorf("render mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRenderSummary_LeadOnly(t *testing.T) {
	resp := &summaryResponse{Outcome: "trivial", Lead: "Hello world greeting only."}
	got := renderSummary(resp)
	if got != "Hello world greeting only." {
		t.Errorf("render: got %q", got)
	}
}

func TestRenderSummary_TrimsAndDropsEmpty(t *testing.T) {
	resp := &summaryResponse{
		Outcome:   "ok",
		Lead:      "  Lead with whitespace.  ",
		Decisions: []string{"  ", "real decision", ""},
	}
	got := renderSummary(resp)
	want := "Lead with whitespace.\n\nDecisions:\n- real decision"
	if got != want {
		t.Errorf("render: got %q want %q", got, want)
	}
}

func TestRenderSummary_AllEmptyReturnsEmpty(t *testing.T) {
	resp := &summaryResponse{Outcome: "ok", Lead: "  ", Decisions: []string{""}, Outcomes: []string{"  "}}
	if got := renderSummary(resp); got != "" {
		t.Errorf("expected empty render, got %q", got)
	}
}

func TestRenderSummary_Nil(t *testing.T) {
	if got := renderSummary(nil); got != "" {
		t.Errorf("nil response should render empty, got %q", got)
	}
}

// summarizeFakeStore tracks insert calls so summary persistence routing
// can be asserted in tests.
type summarizeFakeStore struct {
	memstore.Store
	inserts []memstore.Fact
}

func (s *summarizeFakeStore) Insert(_ context.Context, f memstore.Fact) (int64, error) {
	s.inserts = append(s.inserts, f)
	return int64(len(s.inserts)), nil
}

func TestSummarizeAndPersist_OKInserts(t *testing.T) {
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"project","lead":"Did real work.","decisions":["d1"],"outcomes":["o1"]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	job := extractJob{
		SessionID: "sess-ok",
		CWD:       "/tmp/foo",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "let's work"}},
	}
	q.summarizeAndPersist(context.Background(), job, "foo")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	got := store.inserts[0]
	if got.Subject != "foo" || got.Kind != "summary" || got.Category != "project" {
		t.Errorf("unexpected fact fields: %+v", got)
	}
	if !strings.Contains(got.Content, "Did real work.") || !strings.Contains(got.Content, "- d1") {
		t.Errorf("unexpected content: %q", got.Content)
	}
	if !strings.Contains(string(got.Metadata), `"scope":"project"`) {
		t.Errorf("expected scope=project in metadata: %s", got.Metadata)
	}
}

func TestSummarizeAndPersist_GeneralScopeRoutes(t *testing.T) {
	// Off-topic but substantive — should land under subject=general, not the repo.
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"general","lead":"Discussed Daniel Keys Moran's Ring vs current AI hardware.","decisions":["DGX Station fits the autonomous-AI niche"],"outcomes":["identified $100K-$150K price point"]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	job := extractJob{
		SessionID: "sess-gen",
		CWD:       "/home/matthew/git/homelab",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "what about Ring?"}},
	}
	q.summarizeAndPersist(context.Background(), job, "homelab")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	got := store.inserts[0]
	if got.Subject != "general" {
		t.Errorf("expected subject=general for off-topic session, got %q", got.Subject)
	}
	if got.Category != "note" {
		t.Errorf("expected category=note for general scope, got %q", got.Category)
	}
	if !strings.Contains(string(got.Metadata), `"scope":"general"`) {
		t.Errorf("expected scope=general in metadata: %s", got.Metadata)
	}
	// Project name should still be preserved in metadata for traceability.
	if !strings.Contains(string(got.Metadata), `"project":"homelab"`) {
		t.Errorf("expected project=homelab in metadata: %s", got.Metadata)
	}
}

func TestSummarizeAndPersist_MissingScopeDefaultsToProject(t *testing.T) {
	// Older-schema response without scope field — preserve prior behavior.
	gen := &scoringGenerator{resp: `{"outcome":"ok","lead":"Refactored auth.","decisions":["d1"],"outcomes":["o1"]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-noscope"}, "herald")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	if store.inserts[0].Subject != "herald" || store.inserts[0].Category != "project" {
		t.Errorf("missing scope should default to project: %+v", store.inserts[0])
	}
}

func TestSummarizeAndPersist_UnknownScopeDefaultsToProject(t *testing.T) {
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"weird","lead":"x","decisions":["d"]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-badscope"}, "memstore")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	if store.inserts[0].Subject != "memstore" {
		t.Errorf("unknown scope should fall back to project subject, got %q", store.inserts[0].Subject)
	}
}

func TestSummaryRouting(t *testing.T) {
	cases := []struct {
		name       string
		modelScope string
		project    string
		persona    string
		wantSubj   string
		wantCat    string
		wantScope  string
	}{
		{"project explicit", "project", "memstore", "matthew", "memstore", "project", "project"},
		{"general", "general", "memstore", "matthew", "general", "note", "general"},
		{"user with persona", "user", "memstore", "matthew", "matthew", "identity", "user"},
		{"preference with persona", "preference", "memstore", "matthew", "matthew", "preference", "preference"},
		{"user empty persona falls back", "user", "memstore", "", "user", "identity", "user"},
		{"preference empty persona falls back", "preference", "memstore", "", "user", "preference", "preference"},
		{"missing scope", "", "memstore", "matthew", "memstore", "project", "project"},
		{"unknown scope", "garbage", "memstore", "matthew", "memstore", "project", "project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subj, cat, scope := summaryRouting(tc.modelScope, tc.project, tc.persona)
			if subj != tc.wantSubj || cat != tc.wantCat || scope != tc.wantScope {
				t.Errorf("got (%q,%q,%q) want (%q,%q,%q)", subj, cat, scope, tc.wantSubj, tc.wantCat, tc.wantScope)
			}
		})
	}
}

func TestSummarizeAndPersist_UserScopeRoutesToJobPersona(t *testing.T) {
	// Persona arrives on the job from the client, not from the queue/daemon.
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"user","lead":"Matthew is a security engineer with CISSP and 25 years of experience.","decisions":["expert-level technical baseline confirmed"],"outcomes":[]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	job := extractJob{SessionID: "sess-user", CWD: "/home/matthew/lemonade", Persona: "matthew"}
	q.summarizeAndPersist(context.Background(), job, "lemonade")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	got := store.inserts[0]
	if got.Subject != "matthew" {
		t.Errorf("expected subject=matthew, got %q", got.Subject)
	}
	if got.Category != "identity" {
		t.Errorf("expected category=identity, got %q", got.Category)
	}
	if !strings.Contains(string(got.Metadata), `"scope":"user"`) {
		t.Errorf("expected scope=user in metadata: %s", got.Metadata)
	}
	if !strings.Contains(string(got.Metadata), `"project":"lemonade"`) {
		t.Errorf("expected project=lemonade preserved in metadata: %s", got.Metadata)
	}
}

func TestSummarizeAndPersist_PreferenceScopeRoutesToJobPersona(t *testing.T) {
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"preference","lead":"Matthew prefers small, logical commits.","decisions":["never bundle unrelated changes in one commit"],"outcomes":[]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	job := extractJob{SessionID: "sess-pref", CWD: "/home/matthew/lemonade", Persona: "matthew"}
	q.summarizeAndPersist(context.Background(), job, "lemonade")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	got := store.inserts[0]
	if got.Subject != "matthew" {
		t.Errorf("expected subject=matthew, got %q", got.Subject)
	}
	if got.Category != "preference" {
		t.Errorf("expected category=preference, got %q", got.Category)
	}
	if !strings.Contains(string(got.Metadata), `"scope":"preference"`) {
		t.Errorf("expected scope=preference in metadata: %s", got.Metadata)
	}
}

func TestSummarizeAndPersist_PersonaIsolation(t *testing.T) {
	// Same daemon, two clients with different personas — each user's
	// preference summaries land under that user's subject. This is the
	// core multi-user property: identity comes from the request.
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"preference","lead":"User wants short commits.","decisions":["small commits"]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "s1", Persona: "alice"}, "shared")
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "s2", Persona: "bob"}, "shared")
	if len(store.inserts) != 2 {
		t.Fatalf("expected 2 inserts, got %d", len(store.inserts))
	}
	if store.inserts[0].Subject != "alice" {
		t.Errorf("expected alice's summary under subject=alice, got %q", store.inserts[0].Subject)
	}
	if store.inserts[1].Subject != "bob" {
		t.Errorf("expected bob's summary under subject=bob, got %q", store.inserts[1].Subject)
	}
}

func TestSummarizeAndPersist_UserScopeWithEmptyPersonaFallsBack(t *testing.T) {
	// Old/misconfigured client that sends no persona — daemon's routing
	// falls back to the literal "user" subject so the upload still completes.
	gen := &scoringGenerator{resp: `{"outcome":"ok","scope":"user","lead":"User is new to Go.","decisions":["adjust explanations"]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-nopersona"}, "someproject")
	if len(store.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(store.inserts))
	}
	if store.inserts[0].Subject != "user" {
		t.Errorf("empty persona should fall back to literal 'user', got %q", store.inserts[0].Subject)
	}
}

func TestSummarizeAndPersist_TrivialDrops(t *testing.T) {
	gen := &scoringGenerator{resp: `{"outcome":"trivial","lead":"hello world."}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-tr"}, "tmp")
	if len(store.inserts) != 0 {
		t.Errorf("trivial outcome should not insert, got %d", len(store.inserts))
	}
}

func TestSummarizeAndPersist_ErrorDrops(t *testing.T) {
	gen := &scoringGenerator{resp: `{"outcome":"error","error":{"kind":"too_long","detail":"corpus over limit"}}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-err"}, "tmp")
	if len(store.inserts) != 0 {
		t.Errorf("self-reported error should not insert, got %d", len(store.inserts))
	}
}

func TestSummarizeAndPersist_FormatLapseDrops(t *testing.T) {
	// id=968-style failure: model returns prose addressing the user instead of JSON.
	gen := &scoringGenerator{resp: "Sorry, can you please provide a shorter summary?"}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-lapse"}, "tmp")
	if len(store.inserts) != 0 {
		t.Errorf("format lapse should not insert, got %d", len(store.inserts))
	}
}

func TestSummarizeAndPersist_UnknownOutcomeDrops(t *testing.T) {
	gen := &scoringGenerator{resp: `{"outcome":"weird","lead":"x"}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-unk"}, "tmp")
	if len(store.inserts) != 0 {
		t.Errorf("unknown outcome should not insert, got %d", len(store.inserts))
	}
}

func TestSummarizeAndPersist_OKButEmptyContentDrops(t *testing.T) {
	// outcome=ok but every field is empty/whitespace — render returns "" and we skip.
	gen := &scoringGenerator{resp: `{"outcome":"ok","lead":"  ","decisions":[""],"outcomes":[]}`}
	store := &summarizeFakeStore{}
	q := &ExtractQueue{generator: gen, store: store}
	q.summarizeAndPersist(context.Background(), extractJob{SessionID: "sess-empty"}, "tmp")
	if len(store.inserts) != 0 {
		t.Errorf("empty ok outcome should not insert, got %d", len(store.inserts))
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
		generator: &scoringGenerator{resp: `{"score": 1, "reason": "ok"}`},
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
		generator: &scoringGenerator{resp: `{"score": -1, "reason": "off-topic"}`},
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
	for _, fb := range rater.feedback {
		if fb.RefType != memstore.RefTypeFact {
			t.Errorf("expected ref_type %q, got %q", memstore.RefTypeFact, fb.RefType)
		}
		if fb.SessionID != "sess-f2" {
			t.Errorf("expected session_id sess-f2, got %q", fb.SessionID)
		}
		if fb.Score != -1 {
			t.Errorf("expected score -1, got %d", fb.Score)
		}
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
		generator: &scoringGenerator{resp: `{"score": 1, "reason": "ok"}`},
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
		factIDs: []int64{10},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		10: {ID: 10, Content: "fact A"},
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
	// Parse failure in rateFact returns error, so no feedback is recorded.
	// (rateFact defaults to +1 on error at the caller level, but the error
	// causes autoRateFacts to skip recording.)
	if len(rater.feedback) != 0 {
		t.Errorf("expected no feedback on parse failure, got %d", len(rater.feedback))
	}
}

func TestAutoRateFacts_NegativeScore(t *testing.T) {
	rater := &fakeHintRater{
		factIDs: []int64{42},
	}
	store := &fakeFactStore{facts: map[int64]*memstore.Fact{
		42: {ID: 42, Content: "some fact"},
	}}
	q := &ExtractQueue{
		generator: &scoringGenerator{resp: `{"score": -1, "reason": "off-topic"}`},
		rater:     rater,
		store:     store,
	}
	job := extractJob{
		SessionID: "sess-f5",
		Turns:     []memstore.SessionTurn{{Role: "user", Content: "hello"}},
	}
	q.autoRateFacts(context.Background(), job)
	if len(rater.feedback) != 1 {
		t.Fatalf("expected 1 feedback record, got %d", len(rater.feedback))
	}
	if rater.feedback[0].Score != -1 {
		t.Errorf("expected score -1, got %d", rater.feedback[0].Score)
	}
	if rater.feedback[0].Reason != "off-topic" {
		t.Errorf("expected reason 'off-topic', got %q", rater.feedback[0].Reason)
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
		generator: &scoringGenerator{resp: `{"score": 1, "reason": "relevant"}`},
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
		generator: &scoringGenerator{resp: `{"score": -1, "reason": "off-topic"}`},
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
