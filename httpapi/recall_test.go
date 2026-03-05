package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
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
	for i := 0; i < 20; i++ {
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
	for i := 0; i < 20; i++ {
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
