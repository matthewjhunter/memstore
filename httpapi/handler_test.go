package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	_ "modernc.org/sqlite"
)

type mockEmbedder struct {
	dim int
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i := range texts {
		emb := make([]float32, m.dim)
		for j := range emb {
			emb[j] = float32(i+1) * 0.1 * float32(j+1)
		}
		result[i] = emb
	}
	return result, nil
}

func (m *mockEmbedder) Model() string { return "mock" }

func newTestHandler(t *testing.T) (*httpapi.Handler, *memstore.SQLiteStore) {
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

	return httpapi.New(store, embedder, ""), store
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Result()
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// --- Insert and Get ---

func TestInsertAndGet(t *testing.T) {
	h, _ := newTestHandler(t)

	resp := doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content":  "memstore uses SQLite",
		"subject":  "memstore",
		"category": "project",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("insert: expected 201, got %d", resp.StatusCode)
	}
	var created map[string]any
	decodeJSON(t, resp, &created)
	id := int64(created["id"].(float64))
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	resp = doJSON(t, h, "GET", "/v1/facts/"+itoa(id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", resp.StatusCode)
	}
	var fact memstore.Fact
	decodeJSON(t, resp, &fact)
	if fact.Content != "memstore uses SQLite" {
		t.Fatalf("expected content 'memstore uses SQLite', got %q", fact.Content)
	}
}

func TestInsert_MissingFields(t *testing.T) {
	h, _ := newTestHandler(t)
	resp := doJSON(t, h, "POST", "/v1/facts", map[string]string{
		"content": "no subject",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGet_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	resp := doJSON(t, h, "GET", "/v1/facts/999", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- List ---

func TestList(t *testing.T) {
	h, _ := newTestHandler(t)

	doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "fact one", "subject": "test", "category": "note",
	})
	doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "fact two", "subject": "test", "category": "project",
	})

	resp := doJSON(t, h, "GET", "/v1/facts?subject=test", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var facts []memstore.Fact
	decodeJSON(t, resp, &facts)
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}

	// Filter by category
	resp = doJSON(t, h, "GET", "/v1/facts?subject=test&category=note", nil)
	decodeJSON(t, resp, &facts)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact with category=note, got %d", len(facts))
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	h, _ := newTestHandler(t)

	resp := doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "to delete", "subject": "test",
	})
	var created map[string]any
	decodeJSON(t, resp, &created)
	id := int64(created["id"].(float64))

	resp = doJSON(t, h, "DELETE", "/v1/facts/"+itoa(id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	resp = doJSON(t, h, "GET", "/v1/facts/"+itoa(id), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", resp.StatusCode)
	}
}

// --- Confirm ---

func TestConfirm(t *testing.T) {
	h, _ := newTestHandler(t)

	resp := doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "confirm me", "subject": "test",
	})
	var created map[string]any
	decodeJSON(t, resp, &created)
	id := int64(created["id"].(float64))

	resp = doJSON(t, h, "POST", "/v1/facts/"+itoa(id)+"/confirm", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: expected 200, got %d", resp.StatusCode)
	}
}

// --- ActiveCount ---

func TestActiveCount(t *testing.T) {
	h, _ := newTestHandler(t)

	resp := doJSON(t, h, "GET", "/v1/facts/count", nil)
	var result map[string]any
	decodeJSON(t, resp, &result)
	if result["count"].(float64) != 0 {
		t.Fatalf("expected 0 count, got %v", result["count"])
	}

	doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "one", "subject": "test",
	})
	resp = doJSON(t, h, "GET", "/v1/facts/count", nil)
	decodeJSON(t, resp, &result)
	if result["count"].(float64) != 1 {
		t.Fatalf("expected 1, got %v", result["count"])
	}
}

// --- Auth ---

func TestAuth_Required(t *testing.T) {
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

	h := httpapi.New(store, embedder, "secret-key")

	// No auth header
	resp := doJSON(t, h, "GET", "/v1/facts/count", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}

	// Wrong key
	req := httptest.NewRequest("GET", "/v1/facts/count", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong key, got %d", w.Code)
	}

	// Correct key
	req = httptest.NewRequest("GET", "/v1/facts/count", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct key, got %d", w.Code)
	}
}

// --- Links ---

func TestLinkFacts(t *testing.T) {
	h, _ := newTestHandler(t)

	r1 := doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "room A", "subject": "dungeon",
	})
	r2 := doJSON(t, h, "POST", "/v1/facts", map[string]any{
		"content": "room B", "subject": "dungeon",
	})
	var c1, c2 map[string]any
	decodeJSON(t, r1, &c1)
	decodeJSON(t, r2, &c2)
	id1 := int64(c1["id"].(float64))
	id2 := int64(c2["id"].(float64))

	resp := doJSON(t, h, "POST", "/v1/links", map[string]any{
		"source_id": id1, "target_id": id2, "link_type": "passage", "label": "north door",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("link: expected 201, got %d", resp.StatusCode)
	}
	var linkResult map[string]any
	decodeJSON(t, resp, &linkResult)
	linkID := int64(linkResult["id"].(float64))

	// GetLinks
	resp = doJSON(t, h, "GET", "/v1/facts/"+itoa(id1)+"/links", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get links: expected 200, got %d", resp.StatusCode)
	}
	var links []memstore.Link
	decodeJSON(t, resp, &links)
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}

	// DeleteLink
	resp = doJSON(t, h, "DELETE", "/v1/links/"+itoa(linkID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete link: expected 200, got %d", resp.StatusCode)
	}
}

func itoa(id int64) string {
	return fmt.Sprintf("%d", id)
}
