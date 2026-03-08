package httpapi_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	_ "modernc.org/sqlite"
)

func newTestHandlerWithLearnSession(t *testing.T) (*httpapi.Handler, *memstore.SQLiteStore) {
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

	ls := httpapi.NewLearnSessionStore()
	t.Cleanup(ls.Stop)

	h := httpapi.New(store, embedder, "",
		httpapi.WithSessionContext(sc),
		httpapi.WithGenerator(&testGenerator{}),
		httpapi.WithLearnSessions(ls),
	)
	return h, store
}

func TestLearnFinalize_EmptySession(t *testing.T) {
	h, _ := newTestHandlerWithLearnSession(t)

	resp := doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{
		"session_id": "nonexistent",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Links int `json:"links"`
		Facts int `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	if result.Links != 0 {
		t.Errorf("expected 0 links for empty session, got %d", result.Links)
	}
}

func TestLearnFinalize_RequiresSessionID(t *testing.T) {
	h, _ := newTestHandlerWithLearnSession(t)

	resp := doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without session_id, got %d", resp.StatusCode)
	}
}

const testMarkdownSource = `# Security Design

## Threat Model

We assume a hostile network. All connections require TLS.
The Parser validates all input before processing.

## Key Management

Keys are derived from passwords using Argon2id.
`

func TestLearnFinalize_CrossLinks(t *testing.T) {
	h, store := newTestHandlerWithLearnSession(t)
	sessionID := "test-session-123"

	// Learn a Go file.
	resp1 := doJSON(t, h, "POST", "/v1/learn", map[string]any{
		"subject":      "herald",
		"file_path":    "internal/feeds/parser.go",
		"content":      testGoSource,
		"package_name": "feeds",
		"session_id":   sessionID,
	})
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("learn go file: expected 200, got %d", resp1.StatusCode)
	}
	var r1 struct {
		FileFactID int64 `json:"file_fact_id"`
		Skipped    bool  `json:"skipped"`
	}
	decodeJSON(t, resp1, &r1)
	if r1.Skipped {
		t.Fatal("go file should not be skipped")
	}

	// Learn a markdown file.
	resp2 := doJSON(t, h, "POST", "/v1/learn", map[string]any{
		"subject":    "herald",
		"file_path":  "docs/security.md",
		"content":    testMarkdownSource,
		"session_id": sessionID,
	})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("learn md file: expected 200, got %d", resp2.StatusCode)
	}
	var r2 struct {
		FileFactID int64 `json:"file_fact_id"`
		Skipped    bool  `json:"skipped"`
	}
	decodeJSON(t, resp2, &r2)
	if r2.Skipped {
		t.Fatal("md file should not be skipped")
	}

	// Finalize the session — should create cross-file links.
	resp3 := doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{
		"session_id": sessionID,
		"threshold":  0.01, // very low threshold to ensure links are created with mock embeddings
	})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("finalize: expected 200, got %d", resp3.StatusCode)
	}
	var result struct {
		Links int `json:"links"`
		Facts int `json:"facts"`
	}
	decodeJSON(t, resp3, &result)

	if result.Facts < 2 {
		t.Errorf("expected >= 2 facts in session, got %d", result.Facts)
	}
	if result.Links == 0 {
		t.Error("expected cross-file links to be created")
	}

	// Verify links exist in the store.
	links, err := store.GetLinks(context.Background(), r1.FileFactID, memstore.LinkBoth)
	if err != nil {
		t.Fatal(err)
	}
	// Should have at least the cross-link (in addition to intra-file contains links).
	foundCrossLink := false
	for _, l := range links {
		if l.LinkType == "describes" {
			foundCrossLink = true
			break
		}
	}
	if !foundCrossLink {
		t.Error("expected a 'describes' cross-link between doc and file")
	}
}

func TestLearnFinalize_SessionConsumedOnce(t *testing.T) {
	h, _ := newTestHandlerWithLearnSession(t)
	sessionID := "consume-test"

	// Learn a file.
	resp := doJSON(t, h, "POST", "/v1/learn", map[string]any{
		"subject":      "herald",
		"file_path":    "parser.go",
		"content":      testGoSource,
		"package_name": "feeds",
		"session_id":   sessionID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// First finalize — should have facts.
	resp2 := doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{
		"session_id": sessionID,
	})
	var r1 struct {
		Facts int `json:"facts"`
	}
	decodeJSON(t, resp2, &r1)
	if r1.Facts == 0 {
		t.Error("first finalize should have facts")
	}

	// Second finalize — session consumed, should be empty.
	resp3 := doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{
		"session_id": sessionID,
	})
	var r2 struct {
		Facts int `json:"facts"`
	}
	decodeJSON(t, resp3, &r2)
	if r2.Facts != 0 {
		t.Errorf("second finalize should have 0 facts, got %d", r2.Facts)
	}
}

func TestLearnFinalize_SkippedFilesNotRecorded(t *testing.T) {
	h, _ := newTestHandlerWithLearnSession(t)
	sessionID := "skip-test"

	body := map[string]any{
		"subject":      "herald",
		"file_path":    "parser.go",
		"content":      testGoSource,
		"content_hash": "fixed-hash",
		"package_name": "feeds",
		"session_id":   sessionID,
	}

	// First learn.
	resp1 := doJSON(t, h, "POST", "/v1/learn", body)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()

	// Consume the first session.
	doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{"session_id": sessionID}).Body.Close()

	// Second learn with same hash — should skip.
	sessionID2 := "skip-test-2"
	body["session_id"] = sessionID2
	resp2 := doJSON(t, h, "POST", "/v1/learn", body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	var r2 struct {
		Skipped bool `json:"skipped"`
	}
	decodeJSON(t, resp2, &r2)
	if !r2.Skipped {
		t.Fatal("second learn should be skipped")
	}

	// Finalize second session — should have 0 facts (skipped files not recorded).
	resp3 := doJSON(t, h, "POST", "/v1/learn/finalize", map[string]any{
		"session_id": sessionID2,
	})
	var r3 struct {
		Facts int `json:"facts"`
	}
	decodeJSON(t, resp3, &r3)
	if r3.Facts != 0 {
		t.Errorf("expected 0 facts for skipped-only session, got %d", r3.Facts)
	}
}
