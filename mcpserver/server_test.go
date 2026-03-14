package mcpserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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
	return newTestServerWithConfig(t, mcpserver.Config{})
}

func newTestServerWithConfig(t *testing.T, cfg mcpserver.Config) (*mcpserver.MemoryServer, *memstore.SQLiteStore, *mockEmbedder) {
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

	return mcpserver.NewMemoryServerWithConfig(store, embedder, cfg), store, embedder
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

// --- memory_store_batch tests ---

func TestHandleStoreBatch_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{
		Facts: []mcpserver.StoreInput{
			{Content: "oidclient wraps go-oidc", Subject: "oidclient", Category: "project"},
			{Content: "SF uses webauth for SSO", Subject: "sf", Category: "project"},
			{Content: "Herald migrating to oidclient", Subject: "herald", Category: "project"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "3/3 stored") {
		t.Errorf("expected 3/3 stored, got: %s", text)
	}
	if result.IsError {
		t.Error("expected IsError=false")
	}
	if emb.callCount != 3 {
		t.Errorf("expected 3 embed calls, got %d", emb.callCount)
	}

	// Verify all facts persisted.
	facts, _ := store.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if len(facts) != 3 {
		t.Errorf("expected 3 facts in store, got %d", len(facts))
	}
}

func TestHandleStoreBatch_PartialFailure(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{
		Facts: []mcpserver.StoreInput{
			{Content: "Valid fact one", Subject: "test"},
			{Content: "", Subject: "test"},           // empty content
			{Content: "Valid fact two", Subject: ""}, // empty subject
			{Content: "Valid fact three", Subject: "test"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "2/4 stored") {
		t.Errorf("expected 2/4 stored, got: %s", text)
	}

	facts, _ := store.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if len(facts) != 2 {
		t.Errorf("expected 2 facts, got %d", len(facts))
	}
}

func TestHandleStoreBatch_Dedup(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	// Insert a fact first.
	srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content: "Existing fact",
		Subject: "test",
	})

	// Batch with a duplicate.
	result, _, _ := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{
		Facts: []mcpserver.StoreInput{
			{Content: "Existing fact", Subject: "test"},
			{Content: "New fact", Subject: "test"},
		},
	})

	text := resultText(t, result)
	if !strings.Contains(text, "1/2 stored") {
		t.Errorf("expected 1/2 stored (one dup), got: %s", text)
	}
	if !strings.Contains(text, "duplicate") {
		t.Errorf("expected duplicate message, got: %s", text)
	}
}

func TestHandleStoreBatch_WithSupersedes(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	// Insert a fact to supersede.
	r, _, _ := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content: "Old repo list",
		Subject: "matthew",
	})
	text := resultText(t, r)
	// Extract the ID.
	if !strings.Contains(text, "id=1") {
		t.Fatalf("expected id=1, got: %s", text)
	}

	oldID := int64(1)
	result, _, _ := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{
		Facts: []mcpserver.StoreInput{
			{Content: "Updated repo list with oidclient", Subject: "matthew", Supersedes: &oldID},
		},
	})

	text = resultText(t, result)
	if !strings.Contains(text, "1/1 stored") {
		t.Errorf("expected 1/1 stored, got: %s", text)
	}
	if !strings.Contains(text, "superseded 1") {
		t.Errorf("expected superseded message, got: %s", text)
	}
}

func TestHandleStoreBatch_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{
		Facts: nil,
	})
	if !result.IsError {
		t.Error("expected error for empty facts array")
	}
}

func TestHandleStoreBatch_TooMany(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	facts := make([]mcpserver.StoreInput, 21)
	for i := range facts {
		facts[i] = mcpserver.StoreInput{Content: fmt.Sprintf("fact %d", i), Subject: "test"}
	}

	result, _, _ := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{Facts: facts})
	if !result.IsError {
		t.Error("expected error for >20 facts")
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

// --- memory_supersede tests ---

func TestHandleSupersede_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	oldID := insertFact(t, store, emb, "Matthew uses vim", "matthew", "preference")
	newID := insertFact(t, store, emb, "Matthew uses neovim", "matthew", "preference")

	result, _, err := srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: oldID, NewID: newID})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Superseded") {
		t.Errorf("expected 'Superseded', got: %s", text)
	}

	// Verify old fact is superseded.
	old, _ := store.Get(ctx, oldID)
	if old.SupersededBy == nil {
		t.Error("old fact should be superseded")
	}
}

func TestHandleSupersede_AlreadySuperseded(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id1 := insertFact(t, store, emb, "v1", "X", "test")
	id2 := insertFact(t, store, emb, "v2", "X", "test")
	id3 := insertFact(t, store, emb, "v3", "X", "test")

	store.Supersede(ctx, id1, id2)

	result, _, _ := srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: id1, NewID: id3})
	if !result.IsError {
		t.Error("expected error for already-superseded fact")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "already superseded") {
		t.Errorf("expected 'already superseded' message, got: %s", text)
	}
}

func TestHandleSupersede_NotFound(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id := insertFact(t, store, emb, "exists", "X", "test")

	result, _, _ := srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: 99999, NewID: id})
	if !result.IsError {
		t.Error("expected error for non-existent old_id")
	}

	result, _, _ = srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: id, NewID: 99999})
	if !result.IsError {
		t.Error("expected error for non-existent new_id")
	}
}

func TestHandleSupersede_SameID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: 1, NewID: 1})
	if !result.IsError {
		t.Error("expected error when old_id == new_id")
	}
}

func TestHandleSupersede_InvalidIDs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: 0, NewID: 1})
	if !result.IsError {
		t.Error("expected error for zero old_id")
	}

	result, _, _ = srv.HandleSupersede(ctx, nil, mcpserver.SupersedeInput{OldID: 1, NewID: -1})
	if !result.IsError {
		t.Error("expected error for negative new_id")
	}
}

// --- memory_store with supersedes ---

func TestHandleStore_WithSupersedes(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	oldID := insertFact(t, store, emb, "Matthew uses vim", "matthew", "preference")

	result, _, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content:    "Matthew uses neovim",
		Subject:    "matthew",
		Category:   "preference",
		Supersedes: &oldID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Superseded") {
		t.Errorf("expected supersession confirmation, got: %s", text)
	}

	// Verify old fact is superseded.
	old, _ := store.Get(ctx, oldID)
	if old.SupersededBy == nil {
		t.Error("old fact should be superseded")
	}
}

func TestHandleStore_WithSupersedes_InvalidOldID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	badID := int64(99999)
	result, _, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content:    "Matthew uses neovim",
		Subject:    "matthew",
		Category:   "preference",
		Supersedes: &badID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fact should still be stored even if supersession fails.
	text := resultText(t, result)
	if !strings.Contains(text, "Stored") {
		t.Errorf("expected fact to be stored despite supersession failure, got: %s", text)
	}
	if !strings.Contains(text, "Warning") {
		t.Errorf("expected warning about failed supersession, got: %s", text)
	}
	if result.IsError {
		t.Error("should not be an error — fact was stored successfully")
	}
}

// --- memory_search with include_superseded ---

func TestHandleSearch_AutoTouch(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id := insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")

	// Verify initial use_count is 0.
	before, _ := store.Get(ctx, id)
	if before.UseCount != 0 {
		t.Fatalf("initial use_count = %d, want 0", before.UseCount)
	}

	// Search should auto-touch.
	result, _, _ := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{Query: "dark mode"})
	text := resultText(t, result)
	if !strings.Contains(text, "used=1") {
		t.Errorf("expected used=1 in output, got: %s", text)
	}

	// Verify use_count was bumped in the store.
	after, _ := store.Get(ctx, id)
	if after.UseCount != 1 {
		t.Errorf("use_count after search = %d, want 1", after.UseCount)
	}
}

func TestHandleSearch_IncludeSuperseded(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	oldID := insertFact(t, store, emb, "Matthew uses vim editor", "matthew", "preference")
	newID := insertFact(t, store, emb, "Matthew uses neovim editor", "matthew", "preference")
	store.Supersede(ctx, oldID, newID)

	// Without include_superseded: only active fact.
	result, _, _ := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{
		Query: "editor",
	})
	text := resultText(t, result)
	if strings.Contains(text, "SUPERSEDED") {
		t.Error("should not show superseded tag when include_superseded=false")
	}

	// With include_superseded: both facts, superseded one tagged.
	result, _, _ = srv.HandleSearch(ctx, nil, mcpserver.SearchInput{
		Query:             "editor",
		IncludeSuperseded: true,
	})
	text = resultText(t, result)
	if !strings.Contains(text, "SUPERSEDED") {
		t.Errorf("expected [SUPERSEDED] tag, got: %s", text)
	}
	if !strings.Contains(text, "vim") {
		t.Errorf("expected superseded fact in results, got: %s", text)
	}
}

// --- memory_history tests ---

func TestHandleHistory_ByID(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id1 := insertFact(t, store, emb, "v1", "X", "test")
	id2 := insertFact(t, store, emb, "v2", "X", "test")
	id3 := insertFact(t, store, emb, "v3", "X", "test")

	store.Supersede(ctx, id1, id2)
	store.Supersede(ctx, id2, id3)

	result, _, err := srv.HandleHistory(ctx, nil, mcpserver.HistoryInput{ID: id2})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "v1") || !strings.Contains(text, "v2") || !strings.Contains(text, "v3") {
		t.Errorf("expected all chain entries, got: %s", text)
	}
	if !strings.Contains(text, "SUPERSEDED") {
		t.Errorf("expected SUPERSEDED status, got: %s", text)
	}
	if !strings.Contains(text, "ACTIVE") {
		t.Errorf("expected ACTIVE status for latest fact, got: %s", text)
	}
}

func TestHandleHistory_BySubject(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFact(t, store, emb, "fact A", "matthew", "test")
	insertFact(t, store, emb, "fact B", "matthew", "test")
	insertFact(t, store, emb, "other fact", "memstore", "test")

	result, _, err := srv.HandleHistory(ctx, nil, mcpserver.HistoryInput{Subject: "matthew"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "fact A") || !strings.Contains(text, "fact B") {
		t.Errorf("expected both matthew facts, got: %s", text)
	}
	if strings.Contains(text, "other fact") {
		t.Error("should not include facts from other subjects")
	}
}

func TestHandleHistory_NeitherIDNorSubject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleHistory(context.Background(), nil, mcpserver.HistoryInput{})
	if !result.IsError {
		t.Error("expected error when neither id nor subject provided")
	}
}

func TestHandleHistory_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleHistory(context.Background(), nil, mcpserver.HistoryInput{Subject: "nobody"})
	text := resultText(t, result)
	if !strings.Contains(text, "No history") {
		t.Errorf("expected 'No history' message, got: %s", text)
	}
}

// --- memory_confirm tests ---

func TestHandleConfirm_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id := insertFact(t, store, emb, "Matthew prefers dark mode", "matthew", "preference")

	result, _, err := srv.HandleConfirm(ctx, nil, mcpserver.ConfirmInput{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Confirmed") {
		t.Errorf("expected 'Confirmed', got: %s", text)
	}
	if !strings.Contains(text, "count=1") {
		t.Errorf("expected count=1, got: %s", text)
	}

	// Second confirm.
	result, _, _ = srv.HandleConfirm(ctx, nil, mcpserver.ConfirmInput{ID: id})
	text = resultText(t, result)
	if !strings.Contains(text, "count=2") {
		t.Errorf("expected count=2, got: %s", text)
	}
}

func TestHandleConfirm_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleConfirm(context.Background(), nil, mcpserver.ConfirmInput{ID: 99999})
	if !result.IsError {
		t.Error("expected error for non-existent ID")
	}
}

func TestHandleConfirm_InvalidID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleConfirm(context.Background(), nil, mcpserver.ConfirmInput{ID: 0})
	if !result.IsError {
		t.Error("expected error for zero ID")
	}
}

// --- metadata filter tests ---

func TestHandleSearch_MetadataFilter(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	// Insert two facts with different metadata.
	embVec, _ := memstore.Single(ctx, emb, "fact with source")
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew prefers dark mode",
		Subject:   "matthew",
		Category:  "preference",
		Embedding: embVec,
		Metadata:  []byte(`{"source":"conversation"}`),
	})
	embVec2, _ := memstore.Single(ctx, emb, "fact without source")
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew prefers light mode",
		Subject:   "matthew",
		Category:  "preference",
		Embedding: embVec2,
		Metadata:  []byte(`{"source":"import"}`),
	})

	// Filter by source=conversation.
	result, _, err := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{
		Query:    "mode preference",
		Metadata: map[string]any{"source": "conversation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "dark mode") {
		t.Errorf("expected dark mode fact, got: %s", text)
	}
	if strings.Contains(text, "light mode") {
		t.Errorf("should not include light mode fact (wrong source), got: %s", text)
	}
}

// --- memory_update tests ---

func TestHandleUpdate_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id := insertFact(t, store, emb, "test fact", "X", "note")

	result, _, err := srv.HandleUpdate(ctx, nil, mcpserver.UpdateInput{
		ID:       id,
		Metadata: map[string]any{"status": "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Updated") {
		t.Errorf("expected 'Updated', got: %s", text)
	}

	// Verify metadata was patched.
	got, _ := store.Get(ctx, id)
	if !strings.Contains(string(got.Metadata), `"status"`) {
		t.Errorf("metadata not updated: %s", got.Metadata)
	}
}

func TestHandleUpdate_EmptyMetadata(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleUpdate(context.Background(), nil, mcpserver.UpdateInput{
		ID:       1,
		Metadata: map[string]any{},
	})
	if !result.IsError {
		t.Error("expected error for empty metadata")
	}
}

func TestHandleUpdate_InvalidID(t *testing.T) {
	srv, _, _ := newTestServer(t)

	for _, id := range []int64{0, -1} {
		result, _, _ := srv.HandleUpdate(context.Background(), nil, mcpserver.UpdateInput{
			ID:       id,
			Metadata: map[string]any{"key": "val"},
		})
		if !result.IsError {
			t.Errorf("expected error for ID=%d", id)
		}
	}
}

// --- memory_task_create tests ---

func TestHandleTaskCreate_Basic(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{
		Content: "Fix the login bug",
		Scope:   "matthew",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Created task") {
		t.Errorf("expected 'Created task', got: %s", text)
	}

	// Verify metadata schema.
	facts, _ := store.List(ctx, memstore.QueryOpts{Subject: "todo", OnlyActive: true})
	if len(facts) != 1 {
		t.Fatalf("expected 1 task, got %d", len(facts))
	}
	meta := string(facts[0].Metadata)
	for _, key := range []string{`"kind":"task"`, `"scope":"matthew"`, `"status":"pending"`, `"priority":"normal"`, `"surface":"startup"`} {
		if !strings.Contains(meta, key) {
			t.Errorf("missing %s in metadata: %s", key, meta)
		}
	}
}

func TestHandleTaskCreate_Defaults(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	result, _, _ := srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{
		Content: "Default priority task",
		Scope:   "claude",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	facts, _ := store.List(ctx, memstore.QueryOpts{Subject: "todo", OnlyActive: true})
	if len(facts) != 1 {
		t.Fatalf("expected 1 task, got %d", len(facts))
	}
	meta := string(facts[0].Metadata)
	if !strings.Contains(meta, `"priority":"normal"`) {
		t.Errorf("expected default priority=normal: %s", meta)
	}
	if facts[0].Category != "note" {
		t.Errorf("expected category=note, got %s", facts[0].Category)
	}
}

func TestHandleTaskCreate_InvalidScope(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleTaskCreate(context.Background(), nil, mcpserver.TaskCreateInput{
		Content: "Bad scope",
		Scope:   "invalid",
	})
	if !result.IsError {
		t.Error("expected error for invalid scope")
	}
}

func TestHandleTaskCreate_InvalidPriority(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleTaskCreate(context.Background(), nil, mcpserver.TaskCreateInput{
		Content:  "Bad priority",
		Scope:    "matthew",
		Priority: "urgent",
	})
	if !result.IsError {
		t.Error("expected error for invalid priority")
	}
}

// --- memory_task_update tests ---

func TestHandleTaskUpdate_Complete(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	// Create a task first.
	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{
		Content: "Task to complete",
		Scope:   "matthew",
	})
	facts, _ := store.List(ctx, memstore.QueryOpts{Subject: "todo", OnlyActive: true})
	taskID := facts[0].ID

	result, _, err := srv.HandleTaskUpdate(ctx, nil, mcpserver.TaskUpdateInput{
		ID:     taskID,
		Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	// Verify status changed and surface removed.
	got, _ := store.Get(ctx, taskID)
	meta := string(got.Metadata)
	if !strings.Contains(meta, `"status":"completed"`) {
		t.Errorf("expected status=completed: %s", meta)
	}
	if strings.Contains(meta, `"surface"`) {
		t.Errorf("surface should be removed on completion: %s", meta)
	}
}

func TestHandleTaskUpdate_Cancel(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{
		Content: "Task to cancel",
		Scope:   "claude",
	})
	facts, _ := store.List(ctx, memstore.QueryOpts{Subject: "todo", OnlyActive: true})
	taskID := facts[0].ID

	result, _, _ := srv.HandleTaskUpdate(ctx, nil, mcpserver.TaskUpdateInput{
		ID:     taskID,
		Status: "cancelled",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	got, _ := store.Get(ctx, taskID)
	meta := string(got.Metadata)
	if !strings.Contains(meta, `"status":"cancelled"`) {
		t.Errorf("expected status=cancelled: %s", meta)
	}
	if strings.Contains(meta, `"surface"`) {
		t.Errorf("surface should be removed on cancellation: %s", meta)
	}
}

func TestHandleTaskUpdate_NotATask(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	// Insert a regular fact (not a task).
	id := insertFact(t, store, emb, "Not a task", "matthew", "preference")

	result, _, _ := srv.HandleTaskUpdate(ctx, nil, mcpserver.TaskUpdateInput{
		ID:     id,
		Status: "completed",
	})
	if !result.IsError {
		t.Error("expected error updating non-task fact")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "not a task") {
		t.Errorf("expected 'not a task' message, got: %s", text)
	}
}

func TestHandleTaskUpdate_WithNote(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{
		Content: "Task with note",
		Scope:   "matthew",
	})
	facts, _ := store.List(ctx, memstore.QueryOpts{Subject: "todo", OnlyActive: true})
	taskID := facts[0].ID

	note := "Done via PR #42"
	result, _, _ := srv.HandleTaskUpdate(ctx, nil, mcpserver.TaskUpdateInput{
		ID:     taskID,
		Status: "completed",
		Note:   note,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	got, _ := store.Get(ctx, taskID)
	if !strings.Contains(string(got.Metadata), note) {
		t.Errorf("expected note in metadata: %s", got.Metadata)
	}
}

func TestHandleTaskUpdate_InvalidStatus(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleTaskUpdate(context.Background(), nil, mcpserver.TaskUpdateInput{
		ID:     1,
		Status: "invalid",
	})
	if !result.IsError {
		t.Error("expected error for invalid status")
	}
}

// --- memory_task_list tests ---

func TestHandleTaskList_Default(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{Content: "Task A", Scope: "matthew"})
	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{Content: "Task B", Scope: "claude"})

	result, _, err := srv.HandleTaskList(ctx, nil, mcpserver.TaskListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Task A") || !strings.Contains(text, "Task B") {
		t.Errorf("expected both tasks, got: %s", text)
	}
}

func TestHandleTaskList_ByScope(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{Content: "Matthew's task", Scope: "matthew"})
	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{Content: "Claude's task", Scope: "claude"})

	result, _, _ := srv.HandleTaskList(ctx, nil, mcpserver.TaskListInput{Scope: "matthew"})
	text := resultText(t, result)
	if !strings.Contains(text, "Matthew's task") {
		t.Errorf("expected matthew's task, got: %s", text)
	}
	if strings.Contains(text, "Claude's task") {
		t.Errorf("should not include claude's task, got: %s", text)
	}
}

func TestHandleTaskList_ByProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{Content: "Memstore task", Scope: "matthew", Project: "memstore"})
	srv.HandleTaskCreate(ctx, nil, mcpserver.TaskCreateInput{Content: "Other task", Scope: "matthew", Project: "smtpd"})

	result, _, _ := srv.HandleTaskList(ctx, nil, mcpserver.TaskListInput{Project: "memstore"})
	text := resultText(t, result)
	if !strings.Contains(text, "Memstore task") {
		t.Errorf("expected memstore task, got: %s", text)
	}
	if strings.Contains(text, "Other task") {
		t.Errorf("should not include smtpd task, got: %s", text)
	}
}

func TestHandleTaskList_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleTaskList(context.Background(), nil, mcpserver.TaskListInput{})
	text := resultText(t, result)
	if !strings.Contains(text, "No tasks found") {
		t.Errorf("expected 'No tasks found', got: %s", text)
	}
}

func TestHandleList_MetadataFilter(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content:  "fact A",
		Subject:  "X",
		Category: "test",
		Metadata: []byte(`{"project":"memstore"}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "fact B",
		Subject:  "X",
		Category: "test",
		Metadata: []byte(`{"project":"other"}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content:  "fact C",
		Subject:  "X",
		Category: "test",
		// No metadata.
	})

	result, _, err := srv.HandleList(ctx, nil, mcpserver.ListInput{
		Metadata: map[string]any{"project": "memstore"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "fact A") {
		t.Errorf("expected fact A, got: %s", text)
	}
	if strings.Contains(text, "fact B") || strings.Contains(text, "fact C") {
		t.Errorf("should only include fact A, got: %s", text)
	}
	if !strings.Contains(text, "1 memories listed") {
		t.Errorf("expected 1 result, got: %s", text)
	}
}

// --- memory_link / memory_unlink / memory_get_links / memory_update_link tests ---

func TestHandleLink_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	a := insertFact(t, store, emb, "Entry Hall", "hall", "note")
	b := insertFact(t, store, emb, "Guard Room", "guard-room", "note")

	result, _, err := srv.HandleLink(ctx, nil, mcpserver.LinkInput{
		SourceID: a,
		TargetID: b,
		LinkType: "passage",
		Label:    "east door",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Linked") {
		t.Errorf("expected success message, got: %s", text)
	}
	if !strings.Contains(text, "passage") {
		t.Errorf("expected link type in response, got: %s", text)
	}
}

func TestHandleLink_DefaultsType(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	a := insertFact(t, store, emb, "Room A", "room-a", "note")
	b := insertFact(t, store, emb, "Room B", "room-b", "note")

	result, _, err := srv.HandleLink(ctx, nil, mcpserver.LinkInput{
		SourceID: a,
		TargetID: b,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "reference") {
		t.Error("expected default link type 'reference'")
	}
}

func TestHandleLink_MissingSourceID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleLink(ctx, nil, mcpserver.LinkInput{TargetID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for missing source_id")
	}
}

func TestHandleUnlink(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	a := insertFact(t, store, emb, "Room A", "room-a", "note")
	b := insertFact(t, store, emb, "Room B", "room-b", "note")

	linkResult, _, _ := srv.HandleLink(ctx, nil, mcpserver.LinkInput{
		SourceID: a,
		TargetID: b,
		LinkType: "passage",
	})
	if linkResult.IsError {
		t.Fatalf("setup: %s", resultText(t, linkResult))
	}

	// Extract link ID from response text.
	text := resultText(t, linkResult)
	var linkID int64
	if _, err := fmt.Sscanf(text, "Linked (link_id=%d", &linkID); err != nil {
		t.Fatalf("could not parse link ID from %q: %v", text, err)
	}

	result, _, err := srv.HandleUnlink(ctx, nil, mcpserver.UnlinkInput{LinkID: linkID})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("HandleUnlink error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "Deleted") {
		t.Errorf("expected deleted message, got: %s", resultText(t, result))
	}
}

func TestHandleUnlink_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleUnlink(ctx, nil, mcpserver.UnlinkInput{LinkID: 9999})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for non-existent link ID")
	}
}

func TestHandleGetLinks_Basic(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	hall := insertFact(t, store, emb, "Entry Hall with torches", "hall", "note")
	guard := insertFact(t, store, emb, "Guard barracks", "guard-room", "note")

	if _, _, err := srv.HandleLink(ctx, nil, mcpserver.LinkInput{
		SourceID: hall,
		TargetID: guard,
		LinkType: "passage",
		Label:    "north door",
	}); err != nil {
		t.Fatal(err)
	}

	result, _, err := srv.HandleGetLinks(ctx, nil, mcpserver.GetLinksInput{
		FactID: hall,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("HandleGetLinks error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "passage") {
		t.Errorf("expected link type in response, got: %s", text)
	}
	if !strings.Contains(text, "north door") {
		t.Errorf("expected label in response, got: %s", text)
	}
	if !strings.Contains(text, "guard-room") {
		t.Errorf("expected neighbor subject in response, got: %s", text)
	}
}

func TestHandleGetLinks_InboundDirection(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	a := insertFact(t, store, emb, "Room A", "room-a", "note")
	b := insertFact(t, store, emb, "Room B", "room-b", "note")

	srv.HandleLink(ctx, nil, mcpserver.LinkInput{ //nolint
		SourceID: a,
		TargetID: b,
		LinkType: "passage",
	})

	// Outbound from B: none (directed edge only goes A->B).
	outResult, _, _ := srv.HandleGetLinks(ctx, nil, mcpserver.GetLinksInput{
		FactID:    b,
		Direction: "outbound",
	})
	if strings.Contains(resultText(t, outResult), "passage") {
		t.Error("expected no outbound links from B (directed edge)")
	}

	// Inbound to B: should find the A->B edge.
	inResult, _, _ := srv.HandleGetLinks(ctx, nil, mcpserver.GetLinksInput{
		FactID:    b,
		Direction: "inbound",
	})
	if !strings.Contains(resultText(t, inResult), "passage") {
		t.Errorf("expected passage in inbound result, got: %s", resultText(t, inResult))
	}
}

func TestHandleGetLinks_NoLinks(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	id := insertFact(t, store, emb, "Isolated Room", "isolated", "note")

	result, _, err := srv.HandleGetLinks(ctx, nil, mcpserver.GetLinksInput{FactID: id})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "No links") {
		t.Errorf("expected 'No links' message, got: %s", resultText(t, result))
	}
}

func TestHandleUpdateLink(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	a := insertFact(t, store, emb, "Room A", "room-a", "note")
	b := insertFact(t, store, emb, "Room B", "room-b", "note")

	linkResult, _, _ := srv.HandleLink(ctx, nil, mcpserver.LinkInput{
		SourceID: a,
		TargetID: b,
		LinkType: "passage",
		Label:    "old label",
	})
	text := resultText(t, linkResult)
	var linkID int64
	fmt.Sscanf(text, "Linked (link_id=%d", &linkID)

	result, _, err := srv.HandleUpdateLink(ctx, nil, mcpserver.UpdateLinkInput{
		LinkID:   linkID,
		Label:    "new label",
		Metadata: map[string]any{"dc": 15},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("HandleUpdateLink error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "Updated") {
		t.Errorf("expected updated message, got: %s", resultText(t, result))
	}
}

func TestHandleUpdateLink_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	result, _, err := srv.HandleUpdateLink(ctx, nil, mcpserver.UpdateLinkInput{LinkID: 9999, Label: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error updating non-existent link")
	}
}

// --- memory_get_context tests ---

func insertFactFull(t *testing.T, store *memstore.SQLiteStore, embedder *mockEmbedder, f memstore.Fact) int64 {
	t.Helper()
	ctx := context.Background()
	emb, err := memstore.Single(ctx, embedder, f.Content)
	if err != nil {
		t.Fatal(err)
	}
	f.Embedding = emb
	id, err := store.Insert(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandleGetContext_EmptyTask(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleGetContext(context.Background(), nil, mcpserver.GetContextInput{})
	if !result.IsError {
		t.Error("expected error for empty task")
	}
}

func TestHandleGetContext_NoResults(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleGetContext(context.Background(), nil, mcpserver.GetContextInput{
		Task: "something completely unrelated",
	})
	if result.IsError {
		t.Errorf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "No relevant context") {
		t.Errorf("expected no-results message, got: %s", text)
	}
}

func TestHandleGetContext_InvariantsInjected(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	// Store a regular fact with subsystem=feeds that will match the search.
	insertFactFull(t, store, emb, memstore.Fact{
		Content:   "Feed fetcher uses exponential backoff for retries",
		Subject:   "herald",
		Category:  "project",
		Subsystem: "feeds",
	})

	// Store an invariant for that subsystem.
	invID := insertFactFull(t, store, emb, memstore.Fact{
		Content:   "Never retry on HTTP 404 — mark feed dead instead",
		Subject:   "herald",
		Category:  "note",
		Kind:      "invariant",
		Subsystem: "feeds",
	})

	result, _, _ := srv.HandleGetContext(ctx, nil, mcpserver.GetContextInput{
		Task:    "add retry logic to feed fetcher",
		Subject: "herald",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)

	if !strings.Contains(text, "invariants") {
		t.Error("expected invariants section in output")
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", invID)) {
		t.Errorf("expected invariant fact id=%d in output, got:\n%s", invID, text)
	}
	if !strings.Contains(text, "Never retry on HTTP 404") {
		t.Error("expected invariant content in output")
	}
}

func TestHandleGetContext_TriggersMatched(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	// Store a trigger fact whose content matches the task.
	trigID := insertFactFull(t, store, emb, memstore.Fact{
		Content:  "When adding auth: always use PKCE S256, never implicit flow",
		Subject:  "herald",
		Category: "note",
		Kind:     "trigger",
	})

	// Store a regular fact so search returns something.
	insertFactFull(t, store, emb, memstore.Fact{
		Content:  "Auth module handles login sessions",
		Subject:  "herald",
		Category: "project",
	})

	result, _, _ := srv.HandleGetContext(ctx, nil, mcpserver.GetContextInput{
		Task:    "implement auth login flow",
		Subject: "herald",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)

	if !strings.Contains(text, fmt.Sprintf("id=%d", trigID)) {
		t.Errorf("expected trigger fact id=%d in output, got:\n%s", trigID, text)
	}
}

func TestHandleGetContext_SubjectAndSubsystemHeader(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	insertFactFull(t, store, emb, memstore.Fact{
		Content:   "Feed fetcher polls every 15 minutes",
		Subject:   "herald",
		Category:  "project",
		Subsystem: "feeds",
	})

	result, _, _ := srv.HandleGetContext(ctx, nil, mcpserver.GetContextInput{
		Task:    "update feed polling interval",
		Subject: "herald",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "[subject: herald") {
		t.Errorf("expected subject header, got:\n%s", text)
	}
	if !strings.Contains(text, "feeds") {
		t.Errorf("expected subsystem in header, got:\n%s", text)
	}
}

// --- memory_list_project tests ---

func insertProjectFact(t *testing.T, store *memstore.SQLiteStore, content, subject string, meta map[string]any) int64 {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Insert(ctx, memstore.Fact{
		Content:  content,
		Subject:  subject,
		Category: "project",
		Metadata: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandleListProject_NoCWD(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{})
	if !result.IsError {
		t.Error("expected error for empty cwd")
	}
}

func TestHandleListProject_NoResults(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})
	if result.IsError {
		t.Errorf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "No project-surface") {
		t.Error("expected no-results message")
	}
}

func TestHandleListProject_ProjectPathMatch(t *testing.T) {
	srv, store, _ := newTestServer(t)

	id := insertProjectFact(t, store, "Herald uses goroutines for concurrent feed fetching", "herald", map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})

	// cwd is a subdirectory — should match
	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/herald/internal/feeds",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", id)) {
		t.Errorf("expected fact id=%d in output, got:\n%s", id, text)
	}
}

func TestHandleListProject_ProjectPathNoMatch(t *testing.T) {
	srv, store, _ := newTestServer(t)

	insertProjectFact(t, store, "Herald architecture overview", "herald", map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})

	// cwd is a different project — should not match
	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/memstore",
	})
	if !strings.Contains(resultText(t, result), "No project-surface") {
		t.Error("expected no-results message for non-matching cwd")
	}
}

func TestHandleListProject_ProjectSubjectMatch(t *testing.T) {
	cfg := mcpserver.Config{
		ProjectPaths: map[string]string{
			"herald": "/home/matthew/go/src/github.com/matthewjhunter/herald",
		},
	}
	srv, store, _ := newTestServerWithConfig(t, cfg)

	id := insertProjectFact(t, store, "Herald subsystem map: feeds, auth, storage", "herald", map[string]any{
		"surface":         "project",
		"project_subject": "herald",
	})

	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", id)) {
		t.Errorf("expected fact id=%d in output, got:\n%s", id, text)
	}
}

func TestHandleListProject_ProjectSubjectNoConfig(t *testing.T) {
	// Server with no ProjectPaths config — project_subject facts should not match.
	srv, store, _ := newTestServer(t)

	insertProjectFact(t, store, "Herald subsystem map", "herald", map[string]any{
		"surface":         "project",
		"project_subject": "herald",
	})

	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})
	if !strings.Contains(resultText(t, result), "No project-surface") {
		t.Error("expected no-results message when ProjectPaths not configured")
	}
}

func TestHandleListProject_ExactCWDMatch(t *testing.T) {
	srv, store, _ := newTestServer(t)

	id := insertProjectFact(t, store, "Root-level project fact", "myproject", map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/projects/myproject",
	})

	// cwd exactly equals project_path
	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/projects/myproject",
	})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", id)) {
		t.Errorf("expected exact-match fact id=%d, got:\n%s", id, text)
	}
}

// --- memory_list_project package-tier tests ---

func TestHandleListProject_PackageTier(t *testing.T) {
	srv, store, _ := newTestServer(t)

	id := insertProjectFact(t, store, "feeds package: handles RSS/Atom polling", "herald", map[string]any{
		"surface":      "package",
		"package_path": "/home/matthew/go/src/github.com/matthewjhunter/herald/internal/feeds",
	})

	// cwd inside the package directory — should match
	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/herald/internal/feeds/parser",
	})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", id)) {
		t.Errorf("expected package fact id=%d in output, got:\n%s", id, text)
	}
}

func TestHandleListProject_BothTiers(t *testing.T) {
	srv, store, _ := newTestServer(t)

	projID := insertProjectFact(t, store, "Herald is a feed reader", "herald", map[string]any{
		"surface":      "project",
		"project_path": "/home/matthew/go/src/github.com/matthewjhunter/herald",
	})
	pkgID := insertProjectFact(t, store, "feeds: RSS/Atom package", "herald", map[string]any{
		"surface":      "package",
		"package_path": "/home/matthew/go/src/github.com/matthewjhunter/herald/internal/feeds",
	})

	result, _, _ := srv.HandleListProject(context.Background(), nil, mcpserver.ListProjectInput{
		CWD: "/home/matthew/go/src/github.com/matthewjhunter/herald/internal/feeds",
	})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", projID)) {
		t.Errorf("expected project fact id=%d, got:\n%s", projID, text)
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", pkgID)) {
		t.Errorf("expected package fact id=%d, got:\n%s", pkgID, text)
	}
}

// --- memory_list_file tests ---

func insertFileFact(t *testing.T, store *memstore.SQLiteStore, content, surface, filePath, symbolName string) int64 {
	t.Helper()
	meta := map[string]any{
		"surface":   surface,
		"file_path": filePath,
	}
	if symbolName != "" {
		meta["symbol_name"] = symbolName
	}
	return insertProjectFact(t, store, content, "herald", meta)
}

func TestHandleListFile_NoFilePath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleListFile(context.Background(), nil, mcpserver.ListFileInput{})
	if !result.IsError {
		t.Error("expected error for empty file_path")
	}
}

func TestHandleListFile_NoResults(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleListFile(context.Background(), nil, mcpserver.ListFileInput{
		FilePath: "/home/matthew/go/src/herald/internal/feeds/fetcher.go",
	})
	if result.IsError {
		t.Errorf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "No file-surface") {
		t.Error("expected no-results message")
	}
}

func TestHandleListFile_FileTier(t *testing.T) {
	srv, store, _ := newTestServer(t)
	fp := "/home/matthew/go/src/herald/internal/feeds/fetcher.go"

	id := insertFileFact(t, store, "fetcher.go: manages HTTP feed fetching with retry", "file", fp, "")

	result, _, _ := srv.HandleListFile(context.Background(), nil, mcpserver.ListFileInput{FilePath: fp})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", id)) {
		t.Errorf("expected file fact id=%d, got:\n%s", id, text)
	}
	if !strings.Contains(text, "--- file ---") {
		t.Error("expected file section header")
	}
}

func TestHandleListFile_SymbolTier(t *testing.T) {
	srv, store, _ := newTestServer(t)
	fp := "/home/matthew/go/src/herald/internal/feeds/fetcher.go"

	symID := insertFileFact(t, store, "FetchFeed: GETs the feed URL, returns parsed items", "symbol", fp, "FetchFeed")
	otherID := insertFileFact(t, store, "ParseFeed: parses raw XML into Feed structs", "symbol", fp, "ParseFeed")

	// Without symbol filter: both returned
	result, _, _ := srv.HandleListFile(context.Background(), nil, mcpserver.ListFileInput{FilePath: fp})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", symID)) {
		t.Errorf("expected FetchFeed id=%d, got:\n%s", symID, text)
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", otherID)) {
		t.Errorf("expected ParseFeed id=%d, got:\n%s", otherID, text)
	}
}

func TestHandleListFile_SymbolFilter(t *testing.T) {
	srv, store, _ := newTestServer(t)
	fp := "/home/matthew/go/src/herald/internal/feeds/fetcher.go"

	symID := insertFileFact(t, store, "FetchFeed: GETs the feed URL", "symbol", fp, "FetchFeed")
	insertFileFact(t, store, "ParseFeed: parses XML", "symbol", fp, "ParseFeed")

	// With symbol filter: only FetchFeed
	result, _, _ := srv.HandleListFile(context.Background(), nil, mcpserver.ListFileInput{
		FilePath:   fp,
		SymbolName: "FetchFeed",
	})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", symID)) {
		t.Errorf("expected FetchFeed id=%d, got:\n%s", symID, text)
	}
	if strings.Contains(text, "ParseFeed") {
		t.Error("expected ParseFeed to be filtered out")
	}
	if !strings.Contains(text, "--- symbol: FetchFeed ---") {
		t.Error("expected symbol section header with name")
	}
}

func TestHandleListFile_BothTiers(t *testing.T) {
	srv, store, _ := newTestServer(t)
	fp := "/home/matthew/go/src/herald/internal/feeds/fetcher.go"

	fileID := insertFileFact(t, store, "fetcher.go: HTTP feed fetching", "file", fp, "")
	symID := insertFileFact(t, store, "FetchFeed: GETs the URL", "symbol", fp, "FetchFeed")

	result, _, _ := srv.HandleListFile(context.Background(), nil, mcpserver.ListFileInput{FilePath: fp})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", fileID)) {
		t.Errorf("expected file fact id=%d, got:\n%s", fileID, text)
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", symID)) {
		t.Errorf("expected symbol fact id=%d, got:\n%s", symID, text)
	}
}

// --- memory_check_drift tests ---

// fakeGitRunner returns a GitRunnerFunc that uses the provided map to look up
// last-modified times. Key is the filePath argument; the repoPath is ignored.
func fakeGitRunner(files map[string]time.Time) mcpserver.GitRunnerFunc {
	return func(_ context.Context, _ string, filePath string) (time.Time, bool) {
		t, ok := files[filePath]
		return t, ok
	}
}

// insertSourceFact inserts a fact with source_files metadata for drift tests.
func insertSourceFact(t *testing.T, store *memstore.SQLiteStore, content, subject string, sourceFiles []string) int64 {
	t.Helper()
	ctx := context.Background()
	meta, _ := json.Marshal(map[string]any{"source_files": sourceFiles})
	id, err := store.Insert(ctx, memstore.Fact{
		Content:  content,
		Subject:  subject,
		Category: "note",
		Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandleCheckDrift_NoRepoPath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{})
	if !result.IsError {
		t.Error("expected error when repo_path is empty and no subject RepoPaths config")
	}
}

func TestHandleCheckDrift_RepoPathFromConfig(t *testing.T) {
	cfg := mcpserver.Config{
		RepoPaths: map[string]string{
			"herald": "/home/matthew/go/src/github.com/matthewjhunter/herald",
		},
		GitRunner: fakeGitRunner(map[string]time.Time{}),
	}
	srv, _, _ := newTestServerWithConfig(t, cfg)

	// No source_files facts yet — should get a no-facts message (not an error).
	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{
		Subject: "herald",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "No facts with source_files") {
		t.Error("expected no-source-files message")
	}
}

func TestHandleCheckDrift_NoSourceFiles(t *testing.T) {
	cfg := mcpserver.Config{
		GitRunner: fakeGitRunner(map[string]time.Time{}),
	}
	srv, store, _ := newTestServerWithConfig(t, cfg)

	// Insert a fact without source_files.
	ctx := context.Background()
	_, _ = store.Insert(ctx, memstore.Fact{Content: "some fact", Subject: "myproject", Category: "note"})

	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{
		RepoPath: "/repo",
		Subject:  "myproject",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "No facts with source_files") {
		t.Error("expected no-source-files message")
	}
}

func TestHandleCheckDrift_AllCurrent(t *testing.T) {
	past := time.Now().Add(-48 * time.Hour)
	cfg := mcpserver.Config{
		// Git says the file was last changed 48h ago.
		GitRunner: fakeGitRunner(map[string]time.Time{
			"internal/feeds/fetcher.go": past,
		}),
	}
	srv, store, _ := newTestServerWithConfig(t, cfg)

	// Fact inserted now (after the file's last-modified time) → current.
	insertSourceFact(t, store, "FetchFeed fetches RSS feeds", "herald", []string{"internal/feeds/fetcher.go"})

	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{
		RepoPath:  "/repo",
		SinceDays: 30,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "STALE (0 facts)") {
		t.Errorf("expected 0 stale facts, got:\n%s", text)
	}
	if !strings.Contains(text, "CURRENT (1 facts)") {
		t.Errorf("expected 1 current fact, got:\n%s", text)
	}
}

func TestHandleCheckDrift_StaleAndCurrent(t *testing.T) {
	now := time.Now()
	cfg := mcpserver.Config{
		GitRunner: fakeGitRunner(map[string]time.Time{
			"internal/feeds/fetcher.go": now.Add(time.Hour),       // changed after fact insertion
			"internal/auth/auth.go":     now.Add(-48 * time.Hour), // changed before fact insertion
		}),
	}
	srv, store, _ := newTestServerWithConfig(t, cfg)

	staleID := insertSourceFact(t, store, "FetchFeed behavior", "herald", []string{"internal/feeds/fetcher.go"})
	insertSourceFact(t, store, "Auth behavior", "herald", []string{"internal/auth/auth.go"})

	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{
		RepoPath:  "/repo",
		SinceDays: 30,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if !strings.Contains(text, "STALE (1 facts)") {
		t.Errorf("expected 1 stale fact, got:\n%s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", staleID)) {
		t.Errorf("expected stale fact id=%d in output, got:\n%s", staleID, text)
	}
	if !strings.Contains(text, "CURRENT (1 facts)") {
		t.Errorf("expected 1 current fact, got:\n%s", text)
	}
}

func TestHandleCheckDrift_SubjectFilter(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cfg := mcpserver.Config{
		GitRunner: fakeGitRunner(map[string]time.Time{
			"feeds/fetcher.go": future,
			"auth/auth.go":     future,
		}),
	}
	srv, store, _ := newTestServerWithConfig(t, cfg)

	heraldID := insertSourceFact(t, store, "Herald fetcher", "herald", []string{"feeds/fetcher.go"})
	insertSourceFact(t, store, "Memstore thing", "memstore", []string{"auth/auth.go"})

	// Only check herald — memstore fact should not appear.
	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{
		RepoPath:  "/repo",
		Subject:   "herald",
		SinceDays: 30,
	})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", heraldID)) {
		t.Errorf("expected herald stale fact id=%d, got:\n%s", heraldID, text)
	}
	if strings.Contains(text, "memstore") {
		t.Error("expected memstore fact to be excluded by subject filter")
	}
}

func TestHandleCheckDrift_UnknownFileSkipped(t *testing.T) {
	// GitRunner returns false for unknown files — those facts should not be stale.
	cfg := mcpserver.Config{
		GitRunner: fakeGitRunner(map[string]time.Time{}), // no files known
	}
	srv, store, _ := newTestServerWithConfig(t, cfg)

	insertSourceFact(t, store, "Some behavior", "myproject", []string{"nonexistent/file.go"})

	result, _, _ := srv.HandleCheckDrift(context.Background(), nil, mcpserver.CheckDriftInput{
		RepoPath:  "/repo",
		SinceDays: 30,
	})
	text := resultText(t, result)
	if !strings.Contains(text, "STALE (0 facts)") {
		t.Errorf("expected 0 stale facts when file unknown to git, got:\n%s", text)
	}
	// A fact whose file is unknown to git is not stale — treated as current.
	if !strings.Contains(text, "CURRENT (1 facts)") {
		t.Errorf("expected 1 current fact (unknown file treated as not stale), got:\n%s", text)
	}
}

// --- memory_curate_context tests ---

// fakeCurator returns only the facts whose IDs are in the keep set.
type fakeCurator struct {
	keepIDs   []int64
	rationale string
	err       error
}

func (f fakeCurator) Curate(_ context.Context, _ string, candidates []memstore.Fact, _ int) ([]memstore.Fact, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	keep := make(map[int64]bool)
	for _, id := range f.keepIDs {
		keep[id] = true
	}
	var out []memstore.Fact
	for _, c := range candidates {
		if keep[c.ID] {
			out = append(out, c)
		}
	}
	return out, f.rationale, nil
}

func insertBasicFact(t *testing.T, store *memstore.SQLiteStore, content, subject string) int64 {
	t.Helper()
	id, err := store.Insert(context.Background(), memstore.Fact{
		Content:  content,
		Subject:  subject,
		Category: "note",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandleCurateContext_NoFactIDs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		Task: "do something",
	})
	if !result.IsError {
		t.Error("expected error for empty fact_ids")
	}
}

func TestHandleCurateContext_NoTask(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		FactIDs: []int64{1},
	})
	if !result.IsError {
		t.Error("expected error for empty task")
	}
}

func TestHandleCurateContext_NopCurator(t *testing.T) {
	// Default server uses NopCurator — returns top maxOutput unfiltered.
	srv, store, _ := newTestServer(t)
	id1 := insertBasicFact(t, store, "fact one", "proj")
	id2 := insertBasicFact(t, store, "fact two", "proj")
	id3 := insertBasicFact(t, store, "fact three", "proj")

	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		Task:      "build a feature",
		FactIDs:   []int64{id1, id2, id3},
		MaxOutput: 2,
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	// NopCurator returns first 2.
	if !strings.Contains(text, fmt.Sprintf("id=%d", id1)) {
		t.Errorf("expected id=%d in output, got:\n%s", id1, text)
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", id2)) {
		t.Errorf("expected id=%d in output, got:\n%s", id2, text)
	}
	if strings.Contains(text, fmt.Sprintf("id=%d", id3)) {
		t.Errorf("expected id=%d to be excluded (maxOutput=2), got:\n%s", id3, text)
	}
}

func TestHandleCurateContext_FakeCuratorSelects(t *testing.T) {
	srv, store, _ := newTestServerWithConfig(t, mcpserver.Config{
		Curator: fakeCurator{rationale: "only the important one"},
	})
	id1 := insertBasicFact(t, store, "important fact", "proj")
	id2 := insertBasicFact(t, store, "unimportant fact", "proj")

	// fakeCurator with no keepIDs returns nothing — verify graceful empty output.
	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		Task:    "build a feature",
		FactIDs: []int64{id1, id2},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	_ = resultText(t, result) // just verify it doesn't panic
}

func TestHandleCurateContext_FakeCuratorWithKeep(t *testing.T) {
	var keepID int64
	srv, store, _ := newTestServerWithConfig(t, mcpserver.Config{
		// keepIDs will be set after insert — use a wrapper that captures the variable.
		Curator: &dynamicFakeCurator{rationale: "only essential", keepFn: func() []int64 {
			return []int64{keepID}
		}},
	})
	keepID = insertBasicFact(t, store, "essential fact", "proj")
	skipID := insertBasicFact(t, store, "skip this", "proj")

	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		Task:    "build a feature",
		FactIDs: []int64{keepID, skipID},
	})
	text := resultText(t, result)
	if !strings.Contains(text, fmt.Sprintf("id=%d", keepID)) {
		t.Errorf("expected essential fact id=%d, got:\n%s", keepID, text)
	}
	if strings.Contains(text, fmt.Sprintf("id=%d", skipID)) {
		t.Errorf("expected skip fact id=%d excluded, got:\n%s", skipID, text)
	}
	if !strings.Contains(text, "only essential") {
		t.Errorf("expected rationale in output, got:\n%s", text)
	}
}

func TestHandleCurateContext_CuratorError_Fallback(t *testing.T) {
	srv, store, _ := newTestServerWithConfig(t, mcpserver.Config{
		Curator: fakeCurator{err: fmt.Errorf("model unavailable")},
	})
	id1 := insertBasicFact(t, store, "fallback fact", "proj")

	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		Task:      "build a feature",
		FactIDs:   []int64{id1},
		MaxOutput: 5,
	})
	if result.IsError {
		t.Fatalf("expected graceful fallback, got error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	// Fallback message should be present.
	if !strings.Contains(text, "curation failed") {
		t.Errorf("expected fallback message, got:\n%s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("id=%d", id1)) {
		t.Errorf("expected fallback to include id=%d, got:\n%s", id1, text)
	}
}

func TestHandleCurateContext_UnknownIDs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, _ := srv.HandleCurateContext(context.Background(), nil, mcpserver.CurateContextInput{
		Task:    "build a feature",
		FactIDs: []int64{99999, 99998},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "No active facts") {
		t.Error("expected no-facts message for unknown IDs")
	}
}

// dynamicFakeCurator allows keepFn to be set before use, so IDs inserted
// after construction can be referenced.
type dynamicFakeCurator struct {
	keepFn    func() []int64
	rationale string
}

func (d *dynamicFakeCurator) Curate(_ context.Context, _ string, candidates []memstore.Fact, _ int) ([]memstore.Fact, string, error) {
	keep := make(map[int64]bool)
	for _, id := range d.keepFn() {
		keep[id] = true
	}
	var out []memstore.Fact
	for _, c := range candidates {
		if keep[c.ID] {
			out = append(out, c)
		}
	}
	return out, d.rationale, nil
}

// insertAgentRoutingFact inserts a fact with subsystem "agent-routing" and agent metadata.
func insertAgentRoutingFact(t *testing.T, store *memstore.SQLiteStore, embedder *mockEmbedder, content, subject, agentName string, domains []string) int64 {
	t.Helper()
	ctx := context.Background()
	emb, err := memstore.Single(ctx, embedder, content)
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"agent_name": agentName, "domains": domains}
	metaJSON, _ := json.Marshal(meta)
	id, err := store.Insert(ctx, memstore.Fact{
		Content:   content,
		Subject:   subject,
		Category:  "project",
		Subsystem: "agent-routing",
		Kind:      "convention",
		Metadata:  metaJSON,
		Embedding: emb,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// --- memory_suggest_agent tests ---

func TestHandleSuggestAgent_EmptyTask(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, err := srv.HandleSuggestAgent(context.Background(), nil, mcpserver.SuggestAgentInput{})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "task is required") {
		t.Fatalf("expected error about empty task, got: %s", text)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
}

func TestHandleSuggestAgent_NoRoutingFacts(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, _, err := srv.HandleSuggestAgent(context.Background(), nil, mcpserver.SuggestAgentInput{
		Task: "review security of the auth module",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "No agent-routing facts found") {
		t.Fatalf("expected seeding instructions, got: %s", text)
	}
}

func TestHandleSuggestAgent_DomainMatch(t *testing.T) {
	srv, store, embedder := newTestServer(t)
	ctx := context.Background()

	insertAgentRoutingFact(t, store, embedder,
		"Use for reviewing code security, auth flows, and vulnerability analysis",
		"global", "security-reviewer", []string{"security", "auth", "vulnerability"})
	insertAgentRoutingFact(t, store, embedder,
		"Use for optimizing performance hotspots and latency issues",
		"global", "performance-reviewer", []string{"performance", "latency", "optimization"})

	result, _, err := srv.HandleSuggestAgent(ctx, nil, mcpserver.SuggestAgentInput{
		Task: "review the security of the login auth flow",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "security-reviewer") {
		t.Fatalf("expected security-reviewer in results, got: %s", text)
	}
	// security-reviewer should be listed before performance-reviewer (or performance may not appear)
	secIdx := strings.Index(text, "security-reviewer")
	perfIdx := strings.Index(text, "performance-reviewer")
	if perfIdx != -1 && perfIdx < secIdx {
		t.Fatalf("expected security-reviewer ranked above performance-reviewer:\n%s", text)
	}
}

func TestHandleSuggestAgent_ContentKeywordMatch(t *testing.T) {
	srv, store, embedder := newTestServer(t)
	ctx := context.Background()

	// Agent with no domain overlap but content keyword match
	insertAgentRoutingFact(t, store, embedder,
		"Use for debugging complex race conditions and concurrency issues",
		"global", "debugger", []string{"debugging", "race-condition"})

	result, _, err := srv.HandleSuggestAgent(ctx, nil, mcpserver.SuggestAgentInput{
		Task: "investigate the concurrency issue in the worker pool",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "debugger") {
		t.Fatalf("expected debugger from content keyword match, got: %s", text)
	}
}

func TestHandleSuggestAgent_SubjectScoped(t *testing.T) {
	srv, store, embedder := newTestServer(t)
	ctx := context.Background()

	// Global agent
	insertAgentRoutingFact(t, store, embedder,
		"General code review agent",
		"global", "code-reviewer", []string{"review", "code"})
	// Project-scoped agent
	insertAgentRoutingFact(t, store, embedder,
		"Herald-specific feed parsing reviewer",
		"herald", "feed-reviewer", []string{"review", "feeds"})

	// Query scoped to herald — should get both herald-specific and global
	result, _, err := srv.HandleSuggestAgent(ctx, nil, mcpserver.SuggestAgentInput{
		Task:    "review the feed parsing code",
		Subject: "herald",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "feed-reviewer") {
		t.Fatalf("expected project-scoped feed-reviewer, got: %s", text)
	}
	// Global agent should also appear (has "review" domain)
	if !strings.Contains(text, "code-reviewer") {
		t.Fatalf("expected global code-reviewer as fallback, got: %s", text)
	}
}

func TestHandleSuggestAgent_NoMatches(t *testing.T) {
	srv, store, embedder := newTestServer(t)
	ctx := context.Background()

	insertAgentRoutingFact(t, store, embedder,
		"Use for security analysis",
		"global", "security-reviewer", []string{"security", "vulnerability"})

	result, _, err := srv.HandleSuggestAgent(ctx, nil, mcpserver.SuggestAgentInput{
		Task: "deploy to production",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "No agents matched") {
		t.Fatalf("expected no-match message, got: %s", text)
	}
}

func TestHandleSuggestAgent_ConfidenceLevels(t *testing.T) {
	srv, store, embedder := newTestServer(t)
	ctx := context.Background()

	// High-scoring agent (multiple domain matches)
	insertAgentRoutingFact(t, store, embedder,
		"Reviews security vulnerabilities and auth bypass issues",
		"global", "security-reviewer", []string{"security", "auth", "vulnerability"})
	// Lower-scoring agent (single weaker match)
	insertAgentRoutingFact(t, store, embedder,
		"General code quality and style review",
		"global", "quality-reviewer", []string{"quality", "style"})

	result, _, err := srv.HandleSuggestAgent(ctx, nil, mcpserver.SuggestAgentInput{
		Task: "check for security vulnerability in auth handler",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "high") {
		t.Fatalf("expected high confidence for top match, got: %s", text)
	}
}
