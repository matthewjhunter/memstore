package mcpserver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

// --- test helpers ---

type mockEmbedder struct {
	dim       int
	callCount int
	err       error
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
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

func newTestServer(t *testing.T) (*mcpserver.MemoryServer, *memstore.SQLiteStore, *mockEmbedder) {
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

	return mcpserver.NewMemoryServer(store, embedder), store, embedder
}

// resultText extracts the text from a CallToolResult's first content block.
func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil {
		t.Fatal("nil result")
	}
	if len(r.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", r.Content[0])
	}
	return tc.Text
}

// insertFact is a test helper that inserts a fact with an embedding.
func insertFact(t *testing.T, store *memstore.SQLiteStore, embedder *mockEmbedder, content, subject, category string) int64 {
	t.Helper()
	ctx := context.Background()
	emb, err := memstore.Single(ctx, embedder, content)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Insert(ctx, memstore.Fact{
		Content:   content,
		Subject:   subject,
		Category:  category,
		Embedding: emb,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// --- memory_store tests ---

func TestHandleStore_Basic(t *testing.T) {
	srv, _, emb := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content: "Matthew prefers dark mode",
		Subject: "matthew",
	})
	if err != nil {
		t.Fatal(err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Stored") {
		t.Errorf("expected success message, got: %s", text)
	}
	if !strings.Contains(text, `category="note"`) {
		t.Errorf("expected default category 'note', got: %s", text)
	}
	if result.IsError {
		t.Error("expected IsError=false")
	}
	if emb.callCount != 1 {
		t.Errorf("expected 1 embed call, got %d", emb.callCount)
	}
}

func TestHandleStore_WithCategory(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content:  "Matthew prefers dark mode",
		Subject:  "matthew",
		Category: "preference",
	})
	if err != nil {
		t.Fatal(err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, `category="preference"`) {
		t.Errorf("expected category 'preference', got: %s", text)
	}
}

func TestHandleStore_WithMetadata(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	meta := map[string]any{"source": "conversation", "confidence": "high"}
	result, _, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content:  "Matthew works at Acme Corp",
		Subject:  "matthew",
		Category: "identity",
		Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	// Verify metadata persisted.
	facts, err := store.List(ctx, memstore.QueryOpts{Subject: "matthew", OnlyActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if !strings.Contains(string(facts[0].Metadata), `"source"`) {
		t.Errorf("metadata missing 'source' key: got %s", facts[0].Metadata)
	}
}

func TestHandleStore_Duplicate(t *testing.T) {
	srv, _, emb := newTestServer(t)
	ctx := context.Background()

	input := mcpserver.StoreInput{
		Content: "Matthew prefers dark mode",
		Subject: "matthew",
	}

	// First insert succeeds.
	result, _, _ := srv.HandleStore(ctx, nil, input)
	if result.IsError {
		t.Fatal("first insert should succeed")
	}

	// Second insert reports duplicate.
	embedBefore := emb.callCount
	result, _, _ = srv.HandleStore(ctx, nil, input)
	text := resultText(t, result)
	if !strings.Contains(text, "duplicate") {
		t.Errorf("expected duplicate message, got: %s", text)
	}
	if result.IsError {
		t.Error("duplicate should not be an error")
	}
	// No embed call for the duplicate.
	if emb.callCount != embedBefore {
		t.Errorf("embed should not be called for duplicate, calls: %d -> %d", embedBefore, emb.callCount)
	}
}

func TestHandleStore_EmptyContent(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content: "",
		Subject: "matthew",
	})
	if !result.IsError {
		t.Error("expected error for empty content")
	}
}

func TestHandleStore_EmptySubject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content: "Some fact",
		Subject: "",
	})
	if !result.IsError {
		t.Error("expected error for empty subject")
	}
}

// --- memory_search tests ---

func TestHandleSearch_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")
	insertFact(t, store, emb, "Matthew uses Go for backend work", "matthew", "capability")
	insertFact(t, store, emb, "The project uses SQLite", "memstore", "project")

	result, _, err := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{
		Query: "dark mode preference",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	if !strings.Contains(text, "dark mode") {
		t.Errorf("expected result containing 'dark mode', got: %s", text)
	}
}

func TestHandleSearch_WithSubjectFilter(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")
	insertFact(t, store, emb, "The project uses SQLite", "memstore", "project")

	result, _, _ := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{
		Query:   "preferences",
		Subject: "memstore",
	})

	text := resultText(t, result)
	if strings.Contains(text, "dark mode") {
		t.Error("subject filter should have excluded matthew's fact")
	}
}

func TestHandleSearch_NoResults(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{
		Query: "nonexistent topic",
	})

	text := resultText(t, result)
	if !strings.Contains(text, "No matching") {
		t.Errorf("expected 'No matching' message, got: %s", text)
	}
}

func TestHandleSearch_EmptyQuery(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{Query: ""})
	if !result.IsError {
		t.Error("expected error for empty query")
	}
}

// --- memory_list tests ---

func TestHandleList_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")
	insertFact(t, store, emb, "Matthew uses Go", "matthew", "capability")

	result, _, err := srv.HandleList(ctx, nil, mcpserver.ListInput{})
	if err != nil {
		t.Fatal(err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "dark mode") {
		t.Error("expected list to contain 'dark mode'")
	}
	if !strings.Contains(text, "uses Go") {
		t.Error("expected list to contain 'uses Go'")
	}
	if !strings.Contains(text, "2 memories listed") {
		t.Errorf("expected '2 memories listed', got: %s", text)
	}
}

func TestHandleList_WithFilters(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")
	insertFact(t, store, emb, "Matthew uses Go", "matthew", "capability")
	insertFact(t, store, emb, "memstore uses SQLite", "memstore", "project")

	// Filter by subject.
	result, _, _ := srv.HandleList(ctx, nil, mcpserver.ListInput{Subject: "memstore"})
	text := resultText(t, result)
	if !strings.Contains(text, "1 memories listed") {
		t.Errorf("expected 1 result for subject=memstore, got: %s", text)
	}

	// Filter by category.
	result, _, _ = srv.HandleList(ctx, nil, mcpserver.ListInput{Category: "preference"})
	text = resultText(t, result)
	if !strings.Contains(text, "1 memories listed") {
		t.Errorf("expected 1 result for category=preference, got: %s", text)
	}
}

func TestHandleList_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleList(ctx, nil, mcpserver.ListInput{})
	text := resultText(t, result)
	if !strings.Contains(text, "No memories") {
		t.Errorf("expected 'No memories', got: %s", text)
	}
}

// --- memory_delete tests ---

func TestHandleDelete_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id := insertFact(t, store, emb, "Old fact to delete", "test", "note")

	result, _, err := srv.HandleDelete(ctx, nil, mcpserver.DeleteInput{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Deleted") {
		t.Errorf("expected 'Deleted' message, got: %s", text)
	}

	// Verify it's gone.
	count, _ := store.ActiveCount(ctx)
	if count != 0 {
		t.Errorf("expected 0 facts after delete, got %d", count)
	}
}

func TestHandleDelete_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleDelete(ctx, nil, mcpserver.DeleteInput{ID: 99999})
	if !result.IsError {
		t.Error("expected error for nonexistent ID")
	}
}

func TestHandleDelete_InvalidID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleDelete(ctx, nil, mcpserver.DeleteInput{ID: 0})
	if !result.IsError {
		t.Error("expected error for zero ID")
	}
}

// --- memory_status tests ---

func TestHandleStatus_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleStatus(ctx, nil, mcpserver.StatusInput{})
	if err != nil {
		t.Fatal(err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Active memories: 0") {
		t.Errorf("expected 'Active memories: 0', got: %s", text)
	}
}

func TestHandleStatus_WithFacts(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")
	insertFact(t, store, emb, "Matthew uses Go", "matthew", "capability")
	insertFact(t, store, emb, "memstore uses SQLite", "memstore", "project")

	result, _, _ := srv.HandleStatus(ctx, nil, mcpserver.StatusInput{})
	text := resultText(t, result)

	if !strings.Contains(text, "Active memories: 3") {
		t.Errorf("expected 'Active memories: 3', got: %s", text)
	}
	if !strings.Contains(text, "preference:") {
		t.Errorf("expected category breakdown, got: %s", text)
	}
	if !strings.Contains(text, "matthew:") {
		t.Errorf("expected subject breakdown, got: %s", text)
	}
}
