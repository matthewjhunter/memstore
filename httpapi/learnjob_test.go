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

// testGenerator implements memstore.Generator for tests.
type testGenerator struct{}

func (g *testGenerator) Generate(_ context.Context, _ string) (string, error) {
	return "mock summary of the file", nil
}

func (g *testGenerator) Model() string { return "mock" }

func newTestHandlerWithGen(t *testing.T) *httpapi.Handler {
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

	return httpapi.New(store, embedder, "",
		httpapi.WithSessionContext(sc),
		httpapi.WithGenerator(&testGenerator{}),
	)
}

const testGoSource = `package feeds

// Parser fetches and parses RSS/Atom feeds.
type Parser struct {
	client *http.Client
}

// Parse fetches a feed URL and returns parsed items.
func (p *Parser) Parse(url string) ([]Item, error) {
	return nil, nil
}
`

func TestLearn_RequiresGenerator(t *testing.T) {
	h, _, _ := newTestHandlerWithRecall(t) // no generator configured
	resp := doJSON(t, h, "POST", "/v1/learn", map[string]any{
		"subject":   "herald",
		"file_path": "internal/feeds/parser.go",
		"content":   testGoSource,
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without generator, got %d", resp.StatusCode)
	}
}

func TestLearn_RequiresFields(t *testing.T) {
	h := newTestHandlerWithGen(t)
	resp := doJSON(t, h, "POST", "/v1/learn", map[string]any{
		"subject": "herald",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without file_path/content, got %d", resp.StatusCode)
	}
}

func TestLearn_SingleFile(t *testing.T) {
	h := newTestHandlerWithGen(t)
	resp := doJSON(t, h, "POST", "/v1/learn", map[string]any{
		"subject":      "herald",
		"file_path":    "internal/feeds/parser.go",
		"content":      testGoSource,
		"package_name": "feeds",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		FileFactID int64   `json:"file_fact_id"`
		Symbols    int     `json:"symbols"`
		SymbolIDs  []int64 `json:"symbol_ids"`
		Links      int     `json:"links"`
		LLMCalls   int     `json:"llm_calls"`
		Skipped    bool    `json:"skipped"`
	}
	decodeJSON(t, resp, &result)

	if result.FileFactID == 0 {
		t.Fatal("expected file fact ID")
	}
	if result.Symbols == 0 {
		t.Fatal("expected at least one symbol")
	}
	if result.LLMCalls != 1 {
		t.Errorf("expected 1 LLM call, got %d", result.LLMCalls)
	}
	if result.Skipped {
		t.Error("should not be skipped on first learn")
	}
}

func TestLearn_ContentHashDedup(t *testing.T) {
	h := newTestHandlerWithGen(t)
	body := map[string]any{
		"subject":      "herald",
		"file_path":    "internal/feeds/parser.go",
		"content":      testGoSource,
		"content_hash": "abc123",
	}

	// First call — should learn.
	resp1 := doJSON(t, h, "POST", "/v1/learn", body)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp1.StatusCode)
	}
	var r1 struct {
		Skipped bool `json:"skipped"`
	}
	decodeJSON(t, resp1, &r1)
	if r1.Skipped {
		t.Error("first call should not be skipped")
	}

	// Second call with same hash — should skip.
	resp2 := doJSON(t, h, "POST", "/v1/learn", body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	var r2 struct {
		Skipped bool `json:"skipped"`
	}
	decodeJSON(t, resp2, &r2)
	if !r2.Skipped {
		t.Error("second call with same hash should be skipped")
	}
}
