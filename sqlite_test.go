package memstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *memstore.SQLiteStore {
	t.Helper()
	return openTestStoreWith(t, &mockEmbedder{dim: 4})
}

func openTestStoreWith(t *testing.T, embedder memstore.Embedder) *memstore.SQLiteStore {
	t.Helper()
	return openTestStoreNS(t, embedder, "test")
}

func openTestStoreNS(t *testing.T, embedder memstore.Embedder, namespace string) *memstore.SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := memstore.NewSQLiteStore(db, embedder, namespace)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return store
}

func TestNewSQLiteStore_TablesExist(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := memstore.NewSQLiteStore(db, nil, ""); err != nil {
		t.Fatal(err)
	}

	tables := []string{"memstore_facts", "memstore_facts_fts", "memstore_version", "memstore_meta"}
	for _, table := range tables {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type IN ('table', 'virtual table') AND name = ?`,
			table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestNewSQLiteStore_IndexesExist(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := memstore.NewSQLiteStore(db, nil, ""); err != nil {
		t.Fatal(err)
	}

	indexes := []string{"idx_memstore_subject", "idx_memstore_category", "idx_memstore_active"}
	for _, idx := range indexes {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`,
			idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q not found: %v", idx, err)
		}
	}
}

func TestNewSQLiteStore_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := memstore.NewSQLiteStore(db, nil, ""); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := memstore.NewSQLiteStore(db, nil, ""); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func TestInsert(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content:  "Matthew prefers dark mode",
		Subject:  "Matthew",
		Category: "preference",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil fact")
	}
	if got.Content != "Matthew prefers dark mode" {
		t.Errorf("content = %q", got.Content)
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
}

func TestInsert_WithMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	meta := json.RawMessage(`{"source_persona":"jarvis","conversation_id":42}`)
	id, err := store.Insert(ctx, memstore.Fact{
		Content:  "The user is left-handed",
		Subject:  "User",
		Category: "identity",
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata == nil {
		t.Fatal("expected non-nil metadata")
	}
	var m map[string]any
	if err := json.Unmarshal(got.Metadata, &m); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if m["source_persona"] != "jarvis" {
		t.Errorf("source_persona = %v", m["source_persona"])
	}
}

func TestInsert_WithEmbedding(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	emb := []float32{0.1, 0.2, 0.3, 0.4}
	id, err := store.Insert(ctx, memstore.Fact{
		Content:   "The sky is blue",
		Subject:   "Sky",
		Category:  "setting",
		Embedding: emb,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Embedding) != 4 {
		t.Fatalf("embedding length = %d, want 4", len(got.Embedding))
	}
	for i, v := range emb {
		if got.Embedding[i] != v {
			t.Errorf("embedding[%d] = %f, want %f", i, got.Embedding[i], v)
		}
	}
}

func TestInsertBatch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "Fact one", Subject: "A", Category: "test"},
		{Content: "Fact two", Subject: "B", Category: "test"},
		{Content: "Fact three", Subject: "C", Category: "test"},
	}

	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	for i, f := range facts {
		if f.ID == 0 {
			t.Errorf("fact[%d] has zero ID", i)
		}
	}

	count, err := store.ActiveCount(ctx)
	if err != nil {
		t.Fatalf("ActiveCount: %v", err)
	}
	if count != 3 {
		t.Errorf("active count = %d, want 3", count)
	}
}

func TestSupersede(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	oldID, err := store.Insert(ctx, memstore.Fact{
		Content: "Matthew uses vim", Subject: "Matthew", Category: "preference",
	})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := store.Insert(ctx, memstore.Fact{
		Content: "Matthew uses neovim", Subject: "Matthew", Category: "preference",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Supersede(ctx, oldID, newID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	// BySubject with onlyActive should only return the new fact.
	facts, err := store.BySubject(ctx, "Matthew", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 active fact, got %d", len(facts))
	}
	if facts[0].Content != "Matthew uses neovim" {
		t.Errorf("content = %q", facts[0].Content)
	}

	// BySubject without onlyActive should return both.
	allFacts, err := store.BySubject(ctx, "Matthew", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(allFacts) != 2 {
		t.Errorf("expected 2 total facts, got %d", len(allFacts))
	}
}

func TestSupersede_AlreadySuperseded(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "A", Subject: "X", Category: "test"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "B", Subject: "X", Category: "test"})
	id3, _ := store.Insert(ctx, memstore.Fact{Content: "C", Subject: "X", Category: "test"})

	if err := store.Supersede(ctx, id1, id2); err != nil {
		t.Fatal(err)
	}
	// Superseding again should fail.
	if err := store.Supersede(ctx, id1, id3); err == nil {
		t.Error("expected error superseding already-superseded fact")
	}
}

func TestExists(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "The cat is orange", Subject: "Cat", Category: "character",
	})

	exists, err := store.Exists(ctx, "The cat is orange", "Cat")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected fact to exist")
	}

	exists, err = store.Exists(ctx, "The cat is blue", "Cat")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected fact to not exist")
	}
}

func TestGet_NotFound(t *testing.T) {
	store := openTestStore(t)
	f, err := store.Get(context.Background(), 99999)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if f != nil {
		t.Error("expected nil for non-existent ID")
	}
}

func TestInsert_SetsCreatedAt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)
	id, err := store.Insert(ctx, memstore.Fact{
		Content: "test", Subject: "test", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC().Add(time.Second)

	got, _ := store.Get(ctx, id)
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v not in expected range", got.CreatedAt)
	}
}

func TestNeedingEmbedding(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert one with, one without embedding.
	store.Insert(ctx, memstore.Fact{
		Content: "has embedding", Subject: "A", Category: "test",
		Embedding: []float32{1, 2, 3},
	})
	store.Insert(ctx, memstore.Fact{
		Content: "no embedding", Subject: "B", Category: "test",
	})

	facts, err := store.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact needing embedding, got %d", len(facts))
	}
	if facts[0].Content != "no embedding" {
		t.Errorf("content = %q", facts[0].Content)
	}
}

func TestSetEmbedding(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, _ := store.Insert(ctx, memstore.Fact{
		Content: "test", Subject: "X", Category: "test",
	})

	emb := []float32{0.5, 0.6, 0.7}
	if err := store.SetEmbedding(ctx, id, emb); err != nil {
		t.Fatalf("SetEmbedding: %v", err)
	}

	got, _ := store.Get(ctx, id)
	if len(got.Embedding) != 3 {
		t.Fatalf("embedding length = %d, want 3", len(got.Embedding))
	}
	for i, v := range emb {
		if got.Embedding[i] != v {
			t.Errorf("embedding[%d] = %f, want %f", i, got.Embedding[i], v)
		}
	}
}

func TestEmbedFacts_Basic(t *testing.T) {
	embedder := &mockEmbedder{dim: 4}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "Fact one", Subject: "A", Category: "test"},
		{Content: "Fact two", Subject: "B", Category: "test"},
		{Content: "Fact three", Subject: "C", Category: "test"},
	}
	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatal(err)
	}

	count, err := store.EmbedFacts(ctx, 10)
	if err != nil {
		t.Fatalf("EmbedFacts: %v", err)
	}
	if count != 3 {
		t.Errorf("embedded %d facts, want 3", count)
	}

	// Verify embeddings were stored.
	for _, f := range facts {
		got, _ := store.BySubject(ctx, f.Subject, true)
		if len(got) == 0 {
			t.Errorf("no facts found for %s", f.Subject)
			continue
		}
		if got[0].Embedding == nil {
			t.Errorf("expected embedding for %s", f.Subject)
		}
		if len(got[0].Embedding) != 4 {
			t.Errorf("embedding dim = %d, want 4", len(got[0].Embedding))
		}
	}
}

func TestEmbedFacts_SkipsExisting(t *testing.T) {
	store := openTestStoreWith(t, &mockEmbedder{dim: 3})
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "has embedding", Subject: "A", Category: "test",
		Embedding: []float32{1, 2, 3},
	})
	store.Insert(ctx, memstore.Fact{
		Content: "no embedding", Subject: "B", Category: "test",
	})

	count, err := store.EmbedFacts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("embedded %d, want 1 (should skip existing)", count)
	}
}

func TestEmbedFacts_Batching(t *testing.T) {
	embedder := &mockEmbedder{dim: 4}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	// Insert 7 facts, batch size 3 -> 3 embed calls (3+3+1).
	for i := range 7 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("fact %d", i),
			Subject:  "X",
			Category: "test",
		})
	}

	count, err := store.EmbedFacts(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Errorf("embedded %d, want 7", count)
	}
	if embedder.callCount != 3 {
		t.Errorf("embed calls = %d, want 3", embedder.callCount)
	}
}

func TestEmbedFacts_ErrorPropagates(t *testing.T) {
	store := openTestStoreWith(t, &mockEmbedder{dim: 4, err: fmt.Errorf("embedding service down")})
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "test", Subject: "X", Category: "test",
	})

	_, err := store.EmbedFacts(ctx, 10)
	if err == nil {
		t.Error("expected error from failing embedder")
	}
}

func TestEmbedFacts_NoneToEmbed(t *testing.T) {
	embedder := &mockEmbedder{dim: 3}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "already embedded", Subject: "X", Category: "test",
		Embedding: []float32{1, 2, 3},
	})

	count, err := store.EmbedFacts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("embedded %d, want 0", count)
	}
	if embedder.callCount != 0 {
		t.Errorf("embed calls = %d, want 0", embedder.callCount)
	}
}

func TestEmbedFacts_NoEmbedder(t *testing.T) {
	store := openTestStoreWith(t, nil)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "test", Subject: "X", Category: "test",
	})

	_, err := store.EmbedFacts(ctx, 10)
	if err == nil {
		t.Error("expected error when no embedder configured")
	}
}

func TestEmbedderModelValidation(t *testing.T) {
	// Open store with embedder A, embed a fact to record the model.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := memstore.NewSQLiteStore(db, &mockEmbedder{dim: 4, model: "model-a"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	store.Insert(context.Background(), memstore.Fact{
		Content: "test", Subject: "X", Category: "test",
	})
	if _, err := store.EmbedFacts(context.Background(), 10); err != nil {
		t.Fatal(err)
	}

	// Re-open with the same model — should succeed.
	if _, err := memstore.NewSQLiteStore(db, &mockEmbedder{dim: 4, model: "model-a"}, "test"); err != nil {
		t.Fatalf("same model should succeed: %v", err)
	}

	// Re-open with a different model — should fail.
	_, err = memstore.NewSQLiteStore(db, &mockEmbedder{dim: 4, model: "model-b"}, "test")
	if err == nil {
		t.Error("expected error for mismatched embedding model")
	}
}

func TestNamespace_Isolation(t *testing.T) {
	// Two stores sharing the same DB but different namespaces.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	storeA, err := memstore.NewSQLiteStore(db, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// Insert into each namespace.
	idA, err := storeA.Insert(ctx, memstore.Fact{
		Content: "Alpha fact", Subject: "X", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := storeB.Insert(ctx, memstore.Fact{
		Content: "Beta fact", Subject: "X", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Each store only sees its own facts.
	gotA, err := storeA.Get(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	if gotA == nil || gotA.Content != "Alpha fact" {
		t.Errorf("storeA.Get(idA) = %v", gotA)
	}

	// storeA should not see storeB's fact.
	gotCross, err := storeA.Get(ctx, idB)
	if err != nil {
		t.Fatal(err)
	}
	if gotCross != nil {
		t.Errorf("storeA should not see storeB's fact, got %v", gotCross)
	}

	// BySubject scoped by namespace.
	factsA, err := storeA.BySubject(ctx, "X", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(factsA) != 1 {
		t.Errorf("storeA.BySubject: got %d, want 1", len(factsA))
	}

	factsB, err := storeB.BySubject(ctx, "X", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(factsB) != 1 {
		t.Errorf("storeB.BySubject: got %d, want 1", len(factsB))
	}

	// ActiveCount scoped by namespace.
	countA, _ := storeA.ActiveCount(ctx)
	countB, _ := storeB.ActiveCount(ctx)
	if countA != 1 || countB != 1 {
		t.Errorf("ActiveCount: alpha=%d beta=%d, want 1 each", countA, countB)
	}

	// Exists scoped by namespace.
	exists, _ := storeA.Exists(ctx, "Beta fact", "X")
	if exists {
		t.Error("storeA should not find Beta fact via Exists")
	}
}

func TestNamespace_SearchIsolation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	embedder := &mockEmbedder{dim: 4}
	storeA, err := memstore.NewSQLiteStore(db, embedder, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, embedder, "beta")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	storeA.Insert(ctx, memstore.Fact{
		Content: "The sky is blue", Subject: "Sky", Category: "test",
	})
	storeB.Insert(ctx, memstore.Fact{
		Content: "The sky is orange at sunset", Subject: "Sky", Category: "test",
	})

	// Search within namespace A should only find A's fact.
	results, err := storeA.Search(ctx, "sky", memstore.SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("storeA search: got %d results, want 1", len(results))
	}
	if results[0].Fact.Content != "The sky is blue" {
		t.Errorf("storeA search result = %q", results[0].Fact.Content)
	}

	// Explicit namespace set should find both.
	all, err := storeA.Search(ctx, "sky", memstore.SearchOpts{
		MaxResults: 10,
		Namespaces: []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("Namespaces [alpha,beta] search: got %d results, want 2", len(all))
	}

	// Namespaces set should find only the listed namespaces.
	ns, err := storeA.Search(ctx, "sky", memstore.SearchOpts{
		MaxResults: 10,
		Namespaces: []string{"beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("Namespaces [beta] search: got %d results, want 1", len(ns))
	}
	if ns[0].Fact.Content != "The sky is orange at sunset" {
		t.Errorf("Namespaces search result = %q", ns[0].Fact.Content)
	}

	// Namespaces restricts to listed namespaces only.
	override, err := storeA.Search(ctx, "sky", memstore.SearchOpts{
		MaxResults: 10,
		Namespaces: []string{"alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(override) != 1 {
		t.Fatalf("Namespaces override: got %d results, want 1", len(override))
	}
	if override[0].Fact.Content != "The sky is blue" {
		t.Errorf("Namespaces override result = %q", override[0].Fact.Content)
	}
}

func TestDelete(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content: "to be deleted", Subject: "X", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestDelete_NotFound(t *testing.T) {
	store := openTestStore(t)
	err := store.Delete(context.Background(), 99999)
	if err == nil {
		t.Error("expected error deleting non-existent fact")
	}
}

func TestDelete_WrongNamespace(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	storeA, err := memstore.NewSQLiteStore(db, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	id, _ := storeA.Insert(ctx, memstore.Fact{
		Content: "alpha fact", Subject: "X", Category: "test",
	})

	// storeB should not be able to delete storeA's fact.
	err = storeB.Delete(ctx, id)
	if err == nil {
		t.Error("expected error deleting fact from wrong namespace")
	}

	// Verify it still exists in storeA.
	got, _ := storeA.Get(ctx, id)
	if got == nil {
		t.Error("fact should still exist in storeA")
	}
}

func TestList_All(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "A", Subject: "X", Category: "cat1"})
	store.Insert(ctx, memstore.Fact{Content: "B", Subject: "Y", Category: "cat2"})
	store.Insert(ctx, memstore.Fact{Content: "C", Subject: "X", Category: "cat1"})

	facts, err := store.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("got %d facts, want 3", len(facts))
	}
	// Should be ordered by ID.
	if facts[0].Content != "A" || facts[2].Content != "C" {
		t.Errorf("unexpected order: %q, %q, %q", facts[0].Content, facts[1].Content, facts[2].Content)
	}
}

func TestList_FilterSubject(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "A", Subject: "X", Category: "test"})
	store.Insert(ctx, memstore.Fact{Content: "B", Subject: "Y", Category: "test"})
	store.Insert(ctx, memstore.Fact{Content: "C", Subject: "X", Category: "test"})

	facts, err := store.List(ctx, memstore.QueryOpts{Subject: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("got %d, want 2", len(facts))
	}
	for _, f := range facts {
		if f.Subject != "X" {
			t.Errorf("subject = %q, want X", f.Subject)
		}
	}
}

func TestList_FilterCategory(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "A", Subject: "X", Category: "pref"})
	store.Insert(ctx, memstore.Fact{Content: "B", Subject: "X", Category: "system"})

	facts, err := store.List(ctx, memstore.QueryOpts{Category: "pref"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d, want 1", len(facts))
	}
	if facts[0].Category != "pref" {
		t.Errorf("category = %q", facts[0].Category)
	}
}

func TestList_OnlyActive(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "old", Subject: "X", Category: "test"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "new", Subject: "X", Category: "test"})
	store.Supersede(ctx, id1, id2)

	active, err := store.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("got %d active, want 1", len(active))
	}
	if active[0].Content != "new" {
		t.Errorf("content = %q, want new", active[0].Content)
	}

	all, err := store.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("got %d total, want 2", len(all))
	}
}

func TestList_Limit(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for i := range 10 {
		store.Insert(ctx, memstore.Fact{
			Content: fmt.Sprintf("fact %d", i), Subject: "X", Category: "test",
		})
	}

	facts, err := store.List(ctx, memstore.QueryOpts{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Errorf("got %d, want 3", len(facts))
	}
}

func TestList_MetadataFilter(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "A", Subject: "X", Category: "test",
		Metadata: json.RawMessage(`{"chapter":1}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content: "B", Subject: "X", Category: "test",
		Metadata: json.RawMessage(`{"chapter":5}`),
	})

	facts, err := store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d, want 1", len(facts))
	}
	if facts[0].Content != "A" {
		t.Errorf("content = %q, want A", facts[0].Content)
	}
}

func TestList_MetadataFilterIncludeNull(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "A", Subject: "X", Category: "test",
		Metadata: json.RawMessage(`{"chapter":1}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content: "B", Subject: "X", Category: "test",
		// No metadata — should be included when IncludeNull is true.
	})
	store.Insert(ctx, memstore.Fact{
		Content: "C", Subject: "X", Category: "test",
		Metadata: json.RawMessage(`{"chapter":9}`),
	})

	// Without IncludeNull: only A matches.
	exclusive, err := store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exclusive) != 1 {
		t.Fatalf("exclusive: got %d, want 1", len(exclusive))
	}
	if exclusive[0].Content != "A" {
		t.Errorf("exclusive content = %q, want A", exclusive[0].Content)
	}

	// With IncludeNull: A and B match.
	inclusive, err := store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "chapter", Op: "<=", Value: 5, IncludeNull: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inclusive) != 2 {
		t.Fatalf("inclusive: got %d, want 2", len(inclusive))
	}
	contents := map[string]bool{}
	for _, f := range inclusive {
		contents[f.Content] = true
	}
	if !contents["A"] || !contents["B"] {
		t.Errorf("inclusive results: %v", contents)
	}
}

func TestList_TemporalFilter(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	store.Insert(ctx, memstore.Fact{Content: "old", Subject: "X", Category: "test", CreatedAt: old})
	store.Insert(ctx, memstore.Fact{Content: "mid", Subject: "X", Category: "test", CreatedAt: mid})
	store.Insert(ctx, memstore.Fact{Content: "recent", Subject: "X", Category: "test", CreatedAt: recent})

	// CreatedAfter.
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	facts, err := store.List(ctx, memstore.QueryOpts{CreatedAfter: &cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("CreatedAfter: got %d, want 2", len(facts))
	}

	// CreatedBefore.
	facts, err = store.List(ctx, memstore.QueryOpts{CreatedBefore: &cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("CreatedBefore: got %d, want 1", len(facts))
	}
	if facts[0].Content != "old" {
		t.Errorf("CreatedBefore content = %q, want old", facts[0].Content)
	}

	// Both.
	before := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	facts, err = store.List(ctx, memstore.QueryOpts{
		CreatedAfter:  &cutoff,
		CreatedBefore: &before,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("range: got %d, want 1", len(facts))
	}
	if facts[0].Content != "mid" {
		t.Errorf("range content = %q, want mid", facts[0].Content)
	}
}

func TestSupersede_RecordsTimestamp(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "old", Subject: "X", Category: "test"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "new", Subject: "X", Category: "test"})
	if err := store.Supersede(ctx, id1, id2); err != nil {
		t.Fatal(err)
	}

	after := time.Now().UTC().Add(time.Second)

	// Retrieve including superseded facts.
	facts, err := store.BySubject(ctx, "X", false)
	if err != nil {
		t.Fatal(err)
	}

	var old *memstore.Fact
	for i := range facts {
		if facts[i].ID == id1 {
			old = &facts[i]
		}
	}
	if old == nil {
		t.Fatal("superseded fact not found")
	}

	if old.SupersededAt == nil {
		t.Fatal("SupersededAt should be set")
	}
	if old.SupersededAt.Before(before) || old.SupersededAt.After(after) {
		t.Errorf("SupersededAt %v not in expected range", old.SupersededAt)
	}

	// The new fact should have nil SupersededAt.
	var newFact *memstore.Fact
	for i := range facts {
		if facts[i].ID == id2 {
			newFact = &facts[i]
		}
	}
	if newFact == nil {
		t.Fatal("new fact not found")
	}
	if newFact.SupersededAt != nil {
		t.Errorf("new fact should have nil SupersededAt, got %v", newFact.SupersededAt)
	}
}

func TestNamespace_FactHasNamespaceField(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content: "test", Subject: "X", Category: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "test" {
		t.Errorf("namespace = %q, want %q", got.Namespace, "test")
	}
}

func TestNamespace_SearchWithNamespaceSets(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	embedder := &mockEmbedder{dim: 4}
	storeA, err := memstore.NewSQLiteStore(db, embedder, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, embedder, "beta")
	if err != nil {
		t.Fatal(err)
	}
	storeG, err := memstore.NewSQLiteStore(db, embedder, "gamma")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	storeA.Insert(ctx, memstore.Fact{Content: "Alpha likes cats", Subject: "Alpha", Category: "test"})
	storeB.Insert(ctx, memstore.Fact{Content: "Beta likes dogs", Subject: "Beta", Category: "test"})
	storeG.Insert(ctx, memstore.Fact{Content: "Gamma likes birds", Subject: "Gamma", Category: "test"})

	// Search alpha+beta should find both but not gamma.
	results, err := storeA.Search(ctx, "likes", memstore.SearchOpts{
		MaxResults: 10,
		Namespaces: []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("alpha+beta search: got %d results, want 2", len(results))
	}
	contents := map[string]bool{}
	for _, r := range results {
		contents[r.Fact.Content] = true
	}
	if !contents["Alpha likes cats"] || !contents["Beta likes dogs"] {
		t.Errorf("unexpected results: %v", contents)
	}
	if contents["Gamma likes birds"] {
		t.Error("gamma fact should not appear in alpha+beta search")
	}

	// Search gamma only.
	gOnly, err := storeA.Search(ctx, "likes", memstore.SearchOpts{
		MaxResults: 10,
		Namespaces: []string{"gamma"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gOnly) != 1 {
		t.Fatalf("gamma-only search: got %d results, want 1", len(gOnly))
	}
	if gOnly[0].Fact.Content != "Gamma likes birds" {
		t.Errorf("gamma search result = %q", gOnly[0].Fact.Content)
	}

	// Empty Namespaces defaults to caller's namespace.
	own, err := storeA.Search(ctx, "likes", memstore.SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Fatalf("default namespace search: got %d results, want 1", len(own))
	}
	if own[0].Fact.Content != "Alpha likes cats" {
		t.Errorf("default search result = %q", own[0].Fact.Content)
	}
}

func TestNamespace_ListWithNamespaceSets(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	storeA, err := memstore.NewSQLiteStore(db, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}
	storeG, err := memstore.NewSQLiteStore(db, nil, "gamma")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	storeA.Insert(ctx, memstore.Fact{Content: "A1", Subject: "X", Category: "test"})
	storeB.Insert(ctx, memstore.Fact{Content: "B1", Subject: "X", Category: "test"})
	storeG.Insert(ctx, memstore.Fact{Content: "G1", Subject: "X", Category: "test"})

	// List alpha+beta.
	facts, err := storeA.List(ctx, memstore.QueryOpts{
		Namespaces: []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("alpha+beta list: got %d, want 2", len(facts))
	}
	contents := map[string]bool{}
	for _, f := range facts {
		contents[f.Content] = true
	}
	if !contents["A1"] || !contents["B1"] {
		t.Errorf("unexpected list results: %v", contents)
	}

	// Empty Namespaces defaults to caller's namespace.
	own, err := storeB.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Fatalf("default namespace list: got %d, want 1", len(own))
	}
	if own[0].Content != "B1" {
		t.Errorf("default list result = %q", own[0].Content)
	}
}

func TestSupersede_RespectsNamespace(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	storeA, err := memstore.NewSQLiteStore(db, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	idA, err := storeA.Insert(ctx, memstore.Fact{Content: "Alpha fact", Subject: "X", Category: "test"})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := storeB.Insert(ctx, memstore.Fact{Content: "Beta replacement", Subject: "X", Category: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// Attempting to supersede alpha's fact from beta's store should fail.
	err = storeB.Supersede(ctx, idA, idB)
	if err == nil {
		t.Fatal("expected error superseding cross-namespace fact")
	}

	// Alpha's fact should remain unsuperseded.
	got, err := storeA.Get(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != nil {
		t.Errorf("alpha fact should not be superseded, got superseded_by=%d", *got.SupersededBy)
	}
}

func TestSetEmbedding_RespectsNamespace(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	storeA, err := memstore.NewSQLiteStore(db, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(db, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	idA, err := storeA.Insert(ctx, memstore.Fact{Content: "Alpha fact", Subject: "X", Category: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// Setting embedding from beta's store should not affect alpha's fact.
	emb := []float32{0.1, 0.2, 0.3}
	if err := storeB.SetEmbedding(ctx, idA, emb); err != nil {
		t.Fatalf("SetEmbedding: %v", err) // no SQL error, just no rows matched
	}

	// Alpha's fact should still have no embedding.
	got, err := storeA.Get(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Embedding != nil {
		t.Errorf("alpha fact should have no embedding after cross-namespace SetEmbedding, got %v", got.Embedding)
	}
}
