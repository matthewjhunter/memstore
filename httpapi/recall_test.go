package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	_ "modernc.org/sqlite"
)

func newTestHandlerWithRecall(t *testing.T) (*httpapi.Handler, *memstore.SQLiteStore, *httpapi.SessionContext) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	embedder := &mockEmbedder{dim: 4}
	store, err := memstore.NewSQLiteStore(db, embedder, "test")
	if err != nil {
		t.Fatal(err)
	}

	sc := httpapi.NewSessionContext()
	t.Cleanup(sc.Stop)

	h := httpapi.New(store, embedder, "", httpapi.WithSessionContext(sc))
	return h, store, sc
}

func seedFacts(t *testing.T, store *memstore.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	facts := []memstore.Fact{
		{Content: "Herald is a Go RSS feed aggregator", Subject: "herald", Category: "project"},
		{Content: "Memstore uses SQLite with FTS5 for full-text search", Subject: "memstore", Category: "project"},
		{Content: "Matthew prefers small logical commits", Subject: "matthew", Category: "preference"},
		{Content: "The bancroft module handles authentication tokens", Subject: "bancroft", Category: "project"},
		{Content: "Common session activity note", Subject: "session-activity", Category: "note"},
	}
	for _, f := range facts {
		if _, err := store.Insert(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecall_BasicSearch(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "tell me about the herald feed aggregator",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Context  string   `json:"context"`
		Keywords []string `json:"keywords"`
		Facts    []struct {
			ID      int64  `json:"id"`
			Subject string `json:"subject"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact in recall results")
	}
	if result.Context == "" {
		t.Fatal("expected non-empty context block")
	}
	if len(result.Keywords) == 0 {
		t.Fatal("expected keywords to be returned")
	}
}

func TestRecall_SkipsDrafts(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Insert a draft fact.
	meta, _ := json.Marshal(map[string]any{"quality": "local:qwen2.5:7b"})
	store.Insert(ctx, memstore.Fact{
		Content:  "The nergulite crystal powers the reactor",
		Subject:  "nergulite",
		Category: "project",
		Metadata: meta,
	})
	// Insert a non-draft fact.
	store.Insert(ctx, memstore.Fact{
		Content:  "Nergulite is a rare mineral found on Titan",
		Subject:  "nergulite",
		Category: "world",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "what do we know about nergulite crystals",
	})

	var result struct {
		Facts []struct {
			ID       int64  `json:"id"`
			Category string `json:"category"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	for _, f := range result.Facts {
		if f.Category == "project" {
			t.Error("draft fact should have been filtered out")
		}
	}
}

func TestRecall_SkipsSessionActivity(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "show common session activity information",
	})

	var result struct {
		Facts []struct {
			Subject string `json:"subject"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	for _, f := range result.Facts {
		if f.Subject == "session-activity" {
			t.Error("session-activity facts should be filtered out")
		}
	}
}

func TestRecall_ProjectBoost(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Insert two facts with similar content but different subjects.
	store.Insert(ctx, memstore.Fact{
		Content:  "Parser handles timeout retries for feeds",
		Subject:  "herald",
		Category: "project",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "Parser handles timeout retries for requests",
		Subject:  "other-project",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "parser timeout retries",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})

	var result struct {
		Facts []struct {
			ID      int64  `json:"id"`
			Subject string `json:"subject"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) > 0 && result.Facts[0].Subject != "herald" {
		t.Errorf("expected herald fact to be boosted to top, got %s", result.Facts[0].Subject)
	}
}

func TestRecall_EmptyPrompt(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty prompt, got %d", resp.StatusCode)
	}
}

func TestRecall_BudgetEnforced(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Insert many facts.
	for range 20 {
		store.Insert(ctx, memstore.Fact{
			Content:  "Detailed information about component number for the system architecture review",
			Subject:  "system",
			Category: "project",
		})
	}

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "system architecture component review",
		"budget": 200,
		"limit":  20,
	})

	var result struct {
		Context string `json:"context"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Context) > 300 { // some overhead for formatting
		t.Errorf("context exceeded budget: %d chars", len(result.Context))
	}
}

func TestContextTouch(t *testing.T) {
	h, _, sc := newTestHandlerWithRecall(t)

	resp := doJSON(t, h, "POST", "/v1/context/touch", map[string]any{
		"session_id": "test-session",
		"files":      []string{"/a/foo.go", "/a/bar.go"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	files := sc.RecentFiles("test-session")
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
}

func TestContextTouch_MissingSessionID(t *testing.T) {
	h, _, _ := newTestHandlerWithRecall(t)

	resp := doJSON(t, h, "POST", "/v1/context/touch", map[string]any{
		"files": []string{"/a/foo.go"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRecall_SessionDedup(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)

	body := map[string]any{
		"prompt":     "tell me about the herald feed aggregator",
		"session_id": "dedup-session",
	}

	// First recall should return results.
	resp1 := doJSON(t, h, "POST", "/v1/recall", body)
	var result1 struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp1, &result1)

	if len(result1.Facts) == 0 {
		t.Fatal("expected facts on first recall")
	}
	firstIDs := make(map[int64]bool)
	for _, f := range result1.Facts {
		firstIDs[f.ID] = true
	}

	// Second recall with the same session should not return the same facts.
	resp2 := doJSON(t, h, "POST", "/v1/recall", body)
	var result2 struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp2, &result2)

	for _, f := range result2.Facts {
		if firstIDs[f.ID] {
			t.Errorf("fact %d was returned in both first and second recall", f.ID)
		}
	}
}

func TestRecall_NoSessionID_NoDedup(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)

	body := map[string]any{
		"prompt": "tell me about the herald feed aggregator",
		// No session_id — dedup should not apply.
	}

	resp1 := doJSON(t, h, "POST", "/v1/recall", body)
	var result1 struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp1, &result1)

	resp2 := doJSON(t, h, "POST", "/v1/recall", body)
	var result2 struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp2, &result2)

	// Without session_id, both calls should return the same results.
	if len(result1.Facts) != len(result2.Facts) {
		t.Errorf("without session_id, expected same result count: %d vs %d",
			len(result1.Facts), len(result2.Facts))
	}
}

func TestRecall_CWDTrigger(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Create a cwd_pattern trigger that loads frontend conventions.
	triggerMeta, _ := json.Marshal(map[string]any{
		"signal_type":    "cwd_pattern",
		"signal":         "**/hugo/**",
		"load_subsystem": "frontend",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "Load frontend conventions for Hugo repos",
		Subject:  "global",
		Category: "project",
		Kind:     "trigger",
		Metadata: triggerMeta,
	})

	// Create the fact that should be loaded by the trigger.
	store.Insert(ctx, memstore.Fact{
		Content:   "Hugo repos use TypeScript for shortcodes and custom themes",
		Subject:   "global",
		Category:  "project",
		Kind:      "convention",
		Subsystem: "frontend",
	})

	// Create an unrelated fact.
	store.Insert(ctx, memstore.Fact{
		Content:   "Backend convention for Go services",
		Subject:   "global",
		Category:  "project",
		Kind:      "convention",
		Subsystem: "backend",
	})

	// Recall with CWD matching the trigger.
	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "working on shortcodes",
		"cwd":    "/home/matthew/hugo/mjh",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Facts []struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	found := false
	for _, f := range result.Facts {
		if f.Content == "Hugo repos use TypeScript for shortcodes and custom themes" {
			found = true
		}
	}
	if !found {
		t.Error("expected CWD-triggered frontend fact to appear in recall results")
	}
}

func TestRecall_CWDTrigger_NoMatch(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Same trigger as above.
	triggerMeta, _ := json.Marshal(map[string]any{
		"signal_type":    "cwd_pattern",
		"signal":         "**/hugo/**",
		"load_subsystem": "frontend",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "Load frontend conventions for Hugo repos",
		Subject:  "global",
		Category: "project",
		Kind:     "trigger",
		Metadata: triggerMeta,
	})
	store.Insert(ctx, memstore.Fact{
		Content:   "Enable TypeScript linting and Tailwind CSS compilation for frontend repos",
		Subject:   "global",
		Category:  "project",
		Kind:      "convention",
		Subsystem: "frontend",
	})

	// Recall with CWD that does NOT match — use a prompt with no keyword overlap.
	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "explain the database migration strategy",
		"cwd":    "/home/matthew/go/src/memstore",
	})

	var result struct {
		Facts []struct {
			Content string `json:"content"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	for _, f := range result.Facts {
		if f.Content == "Enable TypeScript linting and Tailwind CSS compilation for frontend repos" {
			t.Error("frontend fact should NOT appear when CWD doesn't match trigger")
		}
	}
}

func TestRecall_DemotesUnrelatedFacts(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Build a corpus large enough for meaningful IDF values.
	seedFacts(t, store)
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact number %d about various topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Insert a project-matching fact and an unrelated D&D-style fact,
	// both containing the same distinctive keyword.
	store.Insert(ctx, memstore.Fact{
		Content:  "The extraction pipeline parses markdown frontmatter",
		Subject:  "memstore",
		Category: "project",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "Riyou extraction of the cursed amulet triggered a trap",
		Subject:  "riyou",
		Category: "identity",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "how does the extraction pipeline work",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}
	// The memstore fact should rank first due to project boost + demotion of unrelated.
	if result.Facts[0].Subject != "memstore" {
		t.Errorf("expected memstore fact first, got %s", result.Facts[0].Subject)
	}
}

func TestRecall_IDFThresholdFiltersCommonWords(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Create a corpus where "system" appears in every document (very common).
	for range 20 {
		store.Insert(ctx, memstore.Fact{
			Content:  "The system handles various operations and tasks",
			Subject:  "generic",
			Category: "project",
		})
	}
	// One fact with a distinctive word.
	store.Insert(ctx, memstore.Fact{
		Content:  "The system uses zygomorphic compression for storage",
		Subject:  "memstore",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "tell me about zygomorphic compression in the system",
	})

	var result struct {
		Keywords []string `json:"keywords"`
	}
	decodeJSON(t, resp, &result)

	// "zygomorphic" should be selected as a keyword (high IDF).
	// "system" should be filtered out (appears in all 21 docs, very low IDF).
	foundZygo := false
	foundSystem := false
	for _, kw := range result.Keywords {
		if kw == "zygomorphic" {
			foundZygo = true
		}
		if kw == "system" {
			foundSystem = true
		}
	}
	if !foundZygo {
		t.Error("expected 'zygomorphic' to be selected as keyword")
	}
	if foundSystem {
		t.Error("expected 'system' to be filtered by IDF threshold")
	}
}

func TestRecall_ScoreCutoffDropsWeakResults(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Build a corpus with one highly relevant fact and several weakly matching ones.
	// The strong match has two distinctive keywords; the weak ones share only one
	// common word with the prompt.
	for i := range 15 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Generic background information item %d for padding", i),
			Subject:  "filler",
			Category: "project",
		})
	}
	store.Insert(ctx, memstore.Fact{
		Content:  "The zygomorphic compression algorithm uses quaternion transforms",
		Subject:  "memstore",
		Category: "project",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "Standard compression ratios for text files",
		Subject:  "other",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "explain the zygomorphic compression quaternion implementation",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
		"limit":  10,
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least the strong match")
	}

	// Verify the top result is the strong match.
	if result.Facts[0].Subject != "memstore" {
		t.Errorf("expected memstore fact first, got %s", result.Facts[0].Subject)
	}

	// All returned facts should score at least 30% of the top fact.
	topScore := result.Facts[0].Score
	for _, f := range result.Facts[1:] {
		if f.Score < topScore*0.3 {
			t.Errorf("fact %d (subject=%s, score=%.2f) is below 30%% of top score %.2f",
				f.ID, f.Subject, f.Score, topScore)
		}
	}
}

func TestRecall_SymbolDemotion(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Pad corpus for IDF.
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// A human-stored project fact about extraction.
	store.Insert(ctx, memstore.Fact{
		Content:  "The extraction pipeline parses markdown frontmatter for metadata",
		Subject:  "memstore",
		Category: "project",
	})

	// A symbol fact (surface=symbol in metadata) about extraction.
	symMeta, _ := json.Marshal(map[string]any{"surface": "symbol", "symbol_name": "Extract"})
	store.Insert(ctx, memstore.Fact{
		Content:  "Extract parses extraction directives from document headers",
		Subject:  "memstore",
		Category: "project",
		Metadata: symMeta,
	})

	// A file: prefixed fact (also a code doc).
	store.Insert(ctx, memstore.Fact{
		Content:  "The file extraction module handles file-based imports",
		Subject:  "file:memstore/extract.go",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "how does the extraction pipeline work",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}

	// The non-symbol project fact should rank first.
	if result.Facts[0].Subject != "memstore" {
		t.Errorf("expected memstore project fact first, got %s", result.Facts[0].Subject)
	}

	// Symbol/file facts should be demoted below the project fact.
	topScore := result.Facts[0].Score
	for _, f := range result.Facts[1:] {
		if f.Subject == "file:memstore/extract.go" || f.Subject == "memstore" {
			// If it's a symbol fact, it should score much less than the top.
			if f.Score > topScore*0.5 {
				t.Errorf("symbol/file fact %d (subject=%s, score=%.2f) should be well below top score %.2f",
					f.ID, f.Subject, f.Score, topScore)
			}
		}
	}
}

func TestRecall_DecisionKindBoost(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Pad corpus for IDF.
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Unclassified fact about authentication.
	store.Insert(ctx, memstore.Fact{
		Content:  "Authentication uses OIDC tokens for session management",
		Subject:  "webauth",
		Category: "project",
	})

	// Decision-kind fact about authentication with similar content.
	store.Insert(ctx, memstore.Fact{
		Content:  "Authentication tokens stored as HTTP-only session cookies",
		Subject:  "webauth",
		Category: "project",
		Kind:     "decision",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "authentication session tokens",
		"cwd":    "/home/matthew/go/src/github.com/infodancer/webauth",
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) < 2 {
		t.Fatalf("expected at least 2 facts, got %d", len(result.Facts))
	}

	// The decision fact should rank higher than the unclassified fact.
	if result.Facts[0].Content != "Authentication tokens stored as HTTP-only session cookies" {
		t.Errorf("expected decision fact to rank first, got: %s", result.Facts[0].Content)
	}
}

func TestRecall_SymbolNotBoostedByProject(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Pad corpus.
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about various items", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Symbol fact from the current project.
	symMeta, _ := json.Marshal(map[string]any{"surface": "symbol", "symbol_name": "SearchFTS"})
	store.Insert(ctx, memstore.Fact{
		Content:  "SearchFTS performs full-text search across all facts",
		Subject:  "memstore",
		Category: "project",
		Metadata: symMeta,
	})

	// Non-symbol fact from a different project.
	store.Insert(ctx, memstore.Fact{
		Content:  "Search architecture uses hybrid full-text and vector retrieval",
		Subject:  "other-project",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "search full-text architecture",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}

	// The non-symbol fact from another project should outrank the symbol fact
	// from the current project, because symbol demotion (0.2x) should dominate
	// even without the project boost.
	for _, f := range result.Facts {
		if f.Subject == "memstore" {
			// Symbol fact should not have gotten the 2.5x project boost.
			// Its effective multiplier is 0.2x, while the other-project fact
			// gets a 0.3x demotion (non-project/non-preference from other subject).
			// In absolute terms both are low, but the symbol should be lower.
			for _, g := range result.Facts {
				if g.Subject == "other-project" && f.Score > g.Score {
					t.Errorf("symbol fact from current project (score=%.2f) should not outrank "+
						"non-symbol fact from other project (score=%.2f)", f.Score, g.Score)
				}
			}
		}
	}
}

func TestRecall_SubjectMatchesProject_OrgPrefix(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Pad corpus.
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about various items", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Fact with org/repo subject.
	store.Insert(ctx, memstore.Fact{
		Content:  "oidclient wraps go-oidc and x/oauth2 for OIDC authentication",
		Subject:  "infodancer/oidclient",
		Category: "project",
		Kind:     "decision",
	})

	// Competing fact from another project.
	store.Insert(ctx, memstore.Fact{
		Content:  "OAuth2 authentication requires client credentials",
		Subject:  "other",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "oidclient oauth2 authentication wrapping",
		"cwd":    "/home/matthew/go/src/github.com/infodancer/oidclient",
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}

	// The infodancer/oidclient fact should rank first — subjectMatchesProject
	// should match "infodancer/oidclient" against project "oidclient".
	if result.Facts[0].Subject != "infodancer/oidclient" {
		t.Errorf("expected infodancer/oidclient fact first, got %s (score=%.2f)",
			result.Facts[0].Subject, result.Facts[0].Score)
	}
}

// mockSessionStore implements SessionStore and FeedbackScorer for testing recall
// feedback integration without PostgreSQL.
type mockSessionStore struct {
	injections    []mockInjection
	feedbackStats map[string]memstore.FeedbackStat // refID -> stats
}

type mockInjection struct {
	SessionID string
	RefID     string
	RefType   string
	Rank      int
}

func (m *mockSessionStore) SaveTurns(context.Context, string, []memstore.SessionTurn) error {
	return nil
}
func (m *mockSessionStore) SaveHook(context.Context, []byte) error { return nil }
func (m *mockSessionStore) StoreHint(context.Context, memstore.ContextHint) (int64, error) {
	return 0, nil
}
func (m *mockSessionStore) GetPendingHints(context.Context, string, string) ([]memstore.ContextHint, error) {
	return nil, nil
}
func (m *mockSessionStore) MarkHintConsumed(context.Context, int64) error { return nil }
func (m *mockSessionStore) RecordInjection(_ context.Context, sessionID, refID, refType string, rank int) error {
	m.injections = append(m.injections, mockInjection{sessionID, refID, refType, rank})
	return nil
}
func (m *mockSessionStore) WasInjected(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (m *mockSessionStore) RecordFeedback(context.Context, memstore.ContextFeedback) error {
	return nil
}
func (m *mockSessionStore) FeedbackScores(_ context.Context, refIDs []string, _ string) (map[string]memstore.FeedbackStat, error) {
	if m.feedbackStats == nil {
		return nil, nil
	}
	result := make(map[string]memstore.FeedbackStat)
	for _, id := range refIDs {
		if stat, ok := m.feedbackStats[id]; ok {
			result[id] = stat
		}
	}
	return result, nil
}

func newTestHandlerWithFeedback(t *testing.T, ss *mockSessionStore) (*httpapi.Handler, *memstore.SQLiteStore, *httpapi.SessionContext) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	embedder := &mockEmbedder{dim: 4}
	store, err := memstore.NewSQLiteStore(db, embedder, "test")
	if err != nil {
		t.Fatal(err)
	}

	sc := httpapi.NewSessionContext()
	t.Cleanup(sc.Stop)

	h := httpapi.New(store, embedder, "",
		httpapi.WithSessionContext(sc),
		httpapi.WithSessionStore(ss),
	)
	return h, store, sc
}

func TestRecall_FeedbackBoostsRanking(t *testing.T) {
	ss := &mockSessionStore{
		feedbackStats: make(map[string]memstore.FeedbackStat),
	}
	h, store, _ := newTestHandlerWithFeedback(t, ss)
	ctx := context.Background()

	// Pad corpus for IDF.
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Two facts with similar content about authentication.
	id1, _ := store.Insert(ctx, memstore.Fact{
		Content:  "Authentication uses OIDC tokens for session validation",
		Subject:  "webauth",
		Category: "project",
	})
	id2, _ := store.Insert(ctx, memstore.Fact{
		Content:  "Authentication uses JWT tokens for session verification",
		Subject:  "webauth",
		Category: "project",
	})

	// Give id2 positive feedback and id1 negative feedback.
	ss.feedbackStats[fmt.Sprintf("%d", id1)] = memstore.FeedbackStat{Avg: -1.0, Count: 1}
	ss.feedbackStats[fmt.Sprintf("%d", id2)] = memstore.FeedbackStat{Avg: 1.0, Count: 1}

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt":     "authentication session tokens",
		"session_id": "feedback-test",
		"cwd":        "/home/matthew/go/src/github.com/infodancer/webauth",
	})

	var result struct {
		Facts []struct {
			ID    int64   `json:"id"`
			Score float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) < 2 {
		t.Fatalf("expected at least 2 facts, got %d", len(result.Facts))
	}

	// The positively-rated fact should rank higher.
	if result.Facts[0].ID != id2 {
		t.Errorf("expected positively-rated fact %d first, got %d", id2, result.Facts[0].ID)
	}
}

func TestRecall_FeedbackConfidenceWeighting(t *testing.T) {
	// A fact rated -1 five times should be demoted much more than an otherwise
	// identical fact rated -1 once. Confirms the count-weighted shrinkage.
	ss := &mockSessionStore{
		feedbackStats: make(map[string]memstore.FeedbackStat),
	}
	h, store, _ := newTestHandlerWithFeedback(t, ss)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Two similar facts; same content, different IDs.
	id1, _ := store.Insert(ctx, memstore.Fact{
		Content:  "Authentication uses zygomorphic quaternion tokens",
		Subject:  "webauth",
		Category: "project",
	})
	id2, _ := store.Insert(ctx, memstore.Fact{
		Content:  "Authentication uses zygomorphic quaternion session cookies",
		Subject:  "webauth",
		Category: "project",
	})

	// id1 rated -1 once (low confidence); id2 rated -1 five times (high confidence).
	ss.feedbackStats[fmt.Sprintf("%d", id1)] = memstore.FeedbackStat{Avg: -1.0, Count: 1}
	ss.feedbackStats[fmt.Sprintf("%d", id2)] = memstore.FeedbackStat{Avg: -1.0, Count: 5}

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt":     "zygomorphic quaternion authentication",
		"session_id": "confidence-test",
		"cwd":        "/home/matthew/go/src/github.com/infodancer/webauth",
	})

	var result struct {
		Facts []struct {
			ID    int64   `json:"id"`
			Score float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	var score1, score2 float64
	for _, f := range result.Facts {
		switch f.ID {
		case id1:
			score1 = f.Score
		case id2:
			score2 = f.Score
		}
	}
	if score1 == 0 || score2 == 0 {
		t.Fatalf("expected both facts in results, got score1=%.3f score2=%.3f", score1, score2)
	}
	// High-confidence negative should score meaningfully below low-confidence.
	if score2 >= score1 {
		t.Errorf("expected high-confidence -1 (count=5, score=%.3f) to rank below low-confidence -1 (count=1, score=%.3f)",
			score2, score1)
	}
}

func TestRecall_NoFeedbackNoEffect(t *testing.T) {
	// With no feedback scores, ranking should be unaffected.
	ss := &mockSessionStore{}
	h, store, _ := newTestHandlerWithFeedback(t, ss)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	store.Insert(ctx, memstore.Fact{
		Content:  "The extraction pipeline parses markdown frontmatter",
		Subject:  "memstore",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt":     "extraction pipeline markdown",
		"session_id": "no-feedback-test",
		"cwd":        "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})

	var result struct {
		Facts []struct {
			ID    int64   `json:"id"`
			Score float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}
	// No crash, results returned — feedback had no effect (no scores to apply).
}

func TestRecall_NilSessionStore_NoFeedback(t *testing.T) {
	// Without a session store, feedback scoring is skipped gracefully.
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}
	store.Insert(ctx, memstore.Fact{
		Content:  "The extraction pipeline parses markdown frontmatter",
		Subject:  "memstore",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt":     "extraction pipeline markdown",
		"session_id": "nil-session-test",
	})

	var result struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}
}

func TestRecall_RecordsInjections(t *testing.T) {
	ss := &mockSessionStore{}
	h, store, _ := newTestHandlerWithFeedback(t, ss)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}
	id1, _ := store.Insert(ctx, memstore.Fact{
		Content:  "The extraction pipeline parses markdown frontmatter",
		Subject:  "memstore",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt":     "extraction pipeline markdown",
		"session_id": "injection-test",
		"cwd":        "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})

	var result struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}

	// Verify injections were recorded.
	if len(ss.injections) == 0 {
		t.Fatal("expected injections to be recorded")
	}

	// Check that the correct fact was recorded with rank 0.
	found := false
	for _, inj := range ss.injections {
		if inj.RefID == fmt.Sprintf("%d", id1) && inj.RefType == "fact" && inj.Rank == 0 && inj.SessionID == "injection-test" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected injection for fact %d at rank 0, got: %+v", id1, ss.injections)
	}
}

func TestRecall_NoInjectionWithoutSessionID(t *testing.T) {
	ss := &mockSessionStore{}
	h, store, _ := newTestHandlerWithFeedback(t, ss)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}
	store.Insert(ctx, memstore.Fact{
		Content:  "The extraction pipeline parses markdown frontmatter",
		Subject:  "memstore",
		Category: "project",
	})

	doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "extraction pipeline markdown",
		// No session_id — injections should not be recorded.
	})

	if len(ss.injections) != 0 {
		t.Errorf("expected no injections without session_id, got %d", len(ss.injections))
	}
}

func TestTermDocCounts_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	embedder := &mockEmbedder{dim: 4}
	store, err := memstore.NewSQLiteStore(db, embedder, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store.Insert(ctx, memstore.Fact{Content: "herald feed parser", Subject: "herald", Category: "project"})
	store.Insert(ctx, memstore.Fact{Content: "memstore search engine", Subject: "memstore", Category: "project"})
	store.Insert(ctx, memstore.Fact{Content: "herald auth tokens", Subject: "herald", Category: "project"})

	counts, total, err := store.TermDocCounts(ctx, []string{"herald", "memstore", "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("expected 3 total docs, got %d", total)
	}
	if counts["herald"] != 2 {
		t.Errorf("expected herald in 2 docs, got %d", counts["herald"])
	}
	if counts["memstore"] != 1 {
		t.Errorf("expected memstore in 1 doc, got %d", counts["memstore"])
	}
	if counts["nonexistent"] != 0 {
		t.Errorf("expected nonexistent in 0 docs, got %d", counts["nonexistent"])
	}
}

func TestRecall_ExcludesKindPattern(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	// Pad corpus for IDF.
	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// A human-stored project fact.
	store.Insert(ctx, memstore.Fact{
		Content:  "The zygomorphic compression algorithm uses quaternion transforms",
		Subject:  "memstore",
		Category: "project",
	})

	// A learn-generated pattern fact (surface=symbol) with the same keywords.
	// This represents the 979 sym:* facts the user flagged as noise.
	symMeta, _ := json.Marshal(map[string]any{"surface": "symbol", "symbol_name": "Compress"})
	store.Insert(ctx, memstore.Fact{
		Content:  "Compress applies zygomorphic quaternion transforms to byte slices",
		Subject:  "sym:memstore.Compress",
		Category: "project",
		Kind:     "pattern",
		Metadata: symMeta,
	})

	// A file-surface pattern fact.
	fileMeta, _ := json.Marshal(map[string]any{"surface": "file"})
	store.Insert(ctx, memstore.Fact{
		Content:  "file compress.go implements zygomorphic quaternion compression",
		Subject:  "file:memstore/compress.go",
		Category: "project",
		Kind:     "pattern",
		Metadata: fileMeta,
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "explain zygomorphic quaternion compression",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
		"limit":  10,
	})

	var result struct {
		Facts []struct {
			Subject string `json:"subject"`
			Kind    string `json:"kind"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	for _, f := range result.Facts {
		if strings.HasPrefix(f.Subject, "sym:") || strings.HasPrefix(f.Subject, "file:") {
			t.Errorf("kind=pattern code-doc fact should be excluded, got %s", f.Subject)
		}
	}
}

func TestRecall_IncludesProjectSurfacePattern(t *testing.T) {
	// Curated repo-level patterns (kind=pattern + surface=project) should
	// NOT be excluded — they're the high-value summaries learn generates.
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	repoMeta, _ := json.Marshal(map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "memstore is a zygomorphic fact store with quaternion-indexed retrieval",
		Subject:  "repo:memstore",
		Category: "project",
		Kind:     "pattern",
		Metadata: repoMeta,
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "zygomorphic quaternion fact store",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
		"limit":  10,
	})

	var result struct {
		Facts []struct {
			Subject string `json:"subject"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	found := false
	for _, f := range result.Facts {
		if f.Subject == "repo:memstore" {
			found = true
		}
	}
	if !found {
		t.Error("expected repo-level project-surface pattern to survive exclusion")
	}
}

func TestRecall_ProjectSurfaceBoost(t *testing.T) {
	// A project-surface fact matching CWD via project_path should dominate
	// over a plain keyword-matching fact — even one whose subject matches
	// the project name.
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// Plain subject-match fact (would have ranked top under the old logic).
	store.Insert(ctx, memstore.Fact{
		Content:  "herald parses RSS and Atom feeds",
		Subject:  "herald",
		Category: "project",
	})

	// Project-surface fact whose subject does NOT look like the project name,
	// but whose project_path contains the CWD.
	psMeta, _ := json.Marshal(map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "herald architecture: fetcher, parser, dedup, publisher pipeline",
		Subject:  "repo:herald",
		Category: "project",
		Kind:     "pattern",
		Metadata: psMeta,
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "herald parser pipeline architecture",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/herald",
		"limit":  5,
	})

	var result struct {
		Facts []struct {
			ID      int64   `json:"id"`
			Subject string  `json:"subject"`
			Score   float64 `json:"score"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if len(result.Facts) == 0 {
		t.Fatal("expected at least one fact")
	}
	if result.Facts[0].Subject != "repo:herald" {
		t.Errorf("expected project-surface fact (repo:herald) first, got %s (score=%.2f)",
			result.Facts[0].Subject, result.Facts[0].Score)
	}
}

func TestRecall_SkipsSummaryFromOtherProject(t *testing.T) {
	// kind=summary facts from unrelated projects should be dropped — they're
	// auto-generated session digests, keyword-rich but context-bound.
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	// A summary from a different project — should be excluded under memstore cwd.
	store.Insert(ctx, memstore.Fact{
		Content:  "The homelab session covered zygomorphic quaternion reconfiguration",
		Subject:  "homelab",
		Category: "project",
		Kind:     "summary",
	})

	// A fact we expect to rank instead.
	store.Insert(ctx, memstore.Fact{
		Content:  "memstore uses zygomorphic quaternion indexing for facts",
		Subject:  "memstore",
		Category: "project",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "zygomorphic quaternion",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
		"limit":  10,
	})

	var result struct {
		Facts []struct {
			Subject string `json:"subject"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	for _, f := range result.Facts {
		if f.Subject == "homelab" {
			t.Error("summary from unrelated project should be excluded")
		}
	}
}

func TestRecall_KeepsSummaryFromSameProject(t *testing.T) {
	// kind=summary from the current project should survive — session digests
	// of prior work on the same codebase are legitimately useful.
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	store.Insert(ctx, memstore.Fact{
		Content:  "Previous memstore session established zygomorphic quaternion indexing",
		Subject:  "memstore",
		Category: "project",
		Kind:     "summary",
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "zygomorphic quaternion indexing",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore",
		"limit":  10,
	})

	var result struct {
		Facts []struct {
			Subject string `json:"subject"`
			Kind    string `json:"kind"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	found := false
	for _, f := range result.Facts {
		if f.Subject == "memstore" {
			found = true
		}
	}
	if !found {
		t.Error("summary from same project should survive exclusion")
	}
}

func TestRecall_ProjectSurface_CWDInSubtree(t *testing.T) {
	// CWD inside a subdirectory of project_path should still match.
	h, store, _ := newTestHandlerWithRecall(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("Background fact %d about miscellaneous topics", i),
			Subject:  "filler",
			Category: "project",
		})
	}

	psMeta, _ := json.Marshal(map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "memstore zygomorphic quaternion architecture overview",
		Subject:  "repo:memstore",
		Category: "project",
		Kind:     "pattern",
		Metadata: psMeta,
	})

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "zygomorphic quaternion architecture",
		"cwd":    "/home/matthew/go/src/github.com/matthewjhunter/memstore/httpapi",
		"limit":  5,
	})

	var result struct {
		Facts []struct {
			Subject string `json:"subject"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	found := false
	for _, f := range result.Facts {
		if f.Subject == "repo:memstore" {
			found = true
		}
	}
	if !found {
		t.Error("expected project-surface fact to match when CWD is inside project_path subtree")
	}
}
