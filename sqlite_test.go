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
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := memstore.NewSQLiteStore(db)
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

	if _, err := memstore.NewSQLiteStore(db); err != nil {
		t.Fatal(err)
	}

	tables := []string{"memstore_facts", "memstore_facts_fts", "memstore_version"}
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

	if _, err := memstore.NewSQLiteStore(db); err != nil {
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

	if _, err := memstore.NewSQLiteStore(db); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := memstore.NewSQLiteStore(db); err != nil {
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
	store := openTestStore(t)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "Fact one", Subject: "A", Category: "test"},
		{Content: "Fact two", Subject: "B", Category: "test"},
		{Content: "Fact three", Subject: "C", Category: "test"},
	}
	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatal(err)
	}

	embedder := &mockEmbedder{dim: 4}
	count, err := store.EmbedFacts(ctx, embedder, 10)
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
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "has embedding", Subject: "A", Category: "test",
		Embedding: []float32{1, 2, 3},
	})
	store.Insert(ctx, memstore.Fact{
		Content: "no embedding", Subject: "B", Category: "test",
	})

	embedder := &mockEmbedder{dim: 3}
	count, err := store.EmbedFacts(ctx, embedder, 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("embedded %d, want 1 (should skip existing)", count)
	}
}

func TestEmbedFacts_Batching(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Insert 7 facts, batch size 3 -> 3 embed calls (3+3+1).
	for i := range 7 {
		store.Insert(ctx, memstore.Fact{
			Content:  fmt.Sprintf("fact %d", i),
			Subject:  "X",
			Category: "test",
		})
	}

	embedder := &mockEmbedder{dim: 4}
	count, err := store.EmbedFacts(ctx, embedder, 3)
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
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "test", Subject: "X", Category: "test",
	})

	embedder := &mockEmbedder{dim: 4, err: fmt.Errorf("embedding service down")}
	_, err := store.EmbedFacts(ctx, embedder, 10)
	if err == nil {
		t.Error("expected error from failing embedder")
	}
}

func TestEmbedFacts_NoneToEmbed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "already embedded", Subject: "X", Category: "test",
		Embedding: []float32{1, 2, 3},
	})

	embedder := &mockEmbedder{dim: 3}
	count, err := store.EmbedFacts(ctx, embedder, 10)
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
