package httpclient_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/httpclient"
	_ "modernc.org/sqlite"
)

type mockEmbedder struct{ dim int }

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

func newTestClient(t *testing.T) *httpclient.Client {
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

	handler := httpapi.New(store, embedder, "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return httpclient.New(srv.URL, "")
}

func TestClient_InsertAndGet(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	id, err := c.Insert(ctx, memstore.Fact{
		Content:  "memstore uses SQLite",
		Subject:  "memstore",
		Category: "project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	f, err := c.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("expected fact, got nil")
	}
	if f.Content != "memstore uses SQLite" {
		t.Fatalf("expected 'memstore uses SQLite', got %q", f.Content)
	}
}

func TestClient_GetNotFound(t *testing.T) {
	c := newTestClient(t)
	f, err := c.Get(context.Background(), 999)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Fatalf("expected nil for missing fact, got %+v", f)
	}
}

func TestClient_List(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	c.Insert(ctx, memstore.Fact{Content: "fact one", Subject: "test", Category: "note"})
	c.Insert(ctx, memstore.Fact{Content: "fact two", Subject: "test", Category: "project"})

	facts, err := c.List(ctx, memstore.QueryOpts{Subject: "test", OnlyActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}

	// Filter by category
	facts, err = c.List(ctx, memstore.QueryOpts{Subject: "test", Category: "note", OnlyActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 note, got %d", len(facts))
	}
}

func TestClient_Delete(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	id, _ := c.Insert(ctx, memstore.Fact{Content: "delete me", Subject: "test"})
	if err := c.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	f, err := c.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestClient_ActiveCount(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	count, err := c.ActiveCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}

	c.Insert(ctx, memstore.Fact{Content: "one", Subject: "test"})
	count, _ = c.ActiveCount(ctx)
	if count != 1 {
		t.Fatalf("expected 1, got %d", count)
	}
}

func TestClient_Supersede(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	id1, _ := c.Insert(ctx, memstore.Fact{Content: "v1", Subject: "test"})
	id2, _ := c.Insert(ctx, memstore.Fact{Content: "v2", Subject: "test"})

	if err := c.Supersede(ctx, id1, id2); err != nil {
		t.Fatal(err)
	}

	// Only active should return 1
	facts, _ := c.List(ctx, memstore.QueryOpts{Subject: "test", OnlyActive: true})
	if len(facts) != 1 {
		t.Fatalf("expected 1 active fact, got %d", len(facts))
	}
	if facts[0].ID != id2 {
		t.Fatalf("expected id %d, got %d", id2, facts[0].ID)
	}
}

func TestClient_InsertWithMetadata(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	meta := map[string]any{"agent_name": "debugger", "domains": []string{"debug"}}
	metaJSON, _ := json.Marshal(meta)
	id, err := c.Insert(ctx, memstore.Fact{
		Content:   "debug agent",
		Subject:   "global",
		Category:  "project",
		Kind:      "convention",
		Subsystem: "agent-routing",
		Metadata:  metaJSON,
	})
	if err != nil {
		t.Fatal(err)
	}

	f, _ := c.Get(ctx, id)
	if f.Kind != "convention" {
		t.Fatalf("expected kind=convention, got %q", f.Kind)
	}
	if f.Subsystem != "agent-routing" {
		t.Fatalf("expected subsystem=agent-routing, got %q", f.Subsystem)
	}
}

func TestClient_Links(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	id1, _ := c.Insert(ctx, memstore.Fact{Content: "room A", Subject: "dungeon"})
	id2, _ := c.Insert(ctx, memstore.Fact{Content: "room B", Subject: "dungeon"})

	linkID, err := c.LinkFacts(ctx, id1, id2, "passage", false, "north door", nil)
	if err != nil {
		t.Fatal(err)
	}
	if linkID == 0 {
		t.Fatal("expected non-zero link id")
	}

	links, err := c.GetLinks(ctx, id1, memstore.LinkBoth)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}

	if err := c.DeleteLink(ctx, linkID); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Retry_TransientThenSuccess(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "busy"})
			return
		}
		json.NewEncoder(w).Encode(map[string]int64{"count": 42})
	}))
	defer srv.Close()

	c := httpclient.New(srv.URL, "")
	count, err := c.ActiveCount(context.Background())
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if count != 42 {
		t.Fatalf("expected count=42, got %d", count)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts (2 failures + 1 success), got %d", attempts)
	}
}

func TestClient_Retry_PermanentError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}))
	defer srv.Close()

	c := httpclient.New(srv.URL, "")
	_, err := c.ActiveCount(context.Background())
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt (no retry on 404), got %d", attempts)
	}
}

func TestClient_Retry_AllFail(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "upstream down"})
	}))
	defer srv.Close()

	c := httpclient.New(srv.URL, "")
	_, err := c.ActiveCount(context.Background())
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if attempts != 4 {
		t.Fatalf("expected 4 attempts (max retries), got %d", attempts)
	}
}

func TestClient_Auth(t *testing.T) {
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

	handler := httpapi.New(store, embedder, "test-key")
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Without key — should fail
	bad := httpclient.New(srv.URL, "")
	_, err = bad.ActiveCount(context.Background())
	if err == nil {
		t.Fatal("expected auth error")
	}

	// With correct key — should work
	good := httpclient.New(srv.URL, "test-key")
	_, err = good.ActiveCount(context.Background())
	if err != nil {
		t.Fatalf("expected success with correct key: %v", err)
	}
}
