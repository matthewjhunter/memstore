package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
