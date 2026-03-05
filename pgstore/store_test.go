package pgstore_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
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

// testDSN returns the PostgreSQL connection string from MEMSTORE_TEST_PG env var.
// If unset, the test is skipped.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MEMSTORE_TEST_PG")
	if dsn == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping PostgreSQL tests")
	}
	return dsn
}

func newTestStore(t *testing.T) *pgstore.PostgresStore {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	// Clean up tables from previous test runs.
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)

	embedder := &mockEmbedder{dim: 4}
	store, err := pgstore.New(ctx, pool, embedder, "test", 4)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	return store
}

func TestInsertAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content:  "memstore uses PostgreSQL",
		Subject:  "memstore",
		Category: "project",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	f, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if f == nil {
		t.Fatal("expected fact, got nil")
	}
	if f.Content != "memstore uses PostgreSQL" {
		t.Fatalf("expected content 'memstore uses PostgreSQL', got %q", f.Content)
	}
	if f.Subject != "memstore" {
		t.Fatalf("expected subject 'memstore', got %q", f.Subject)
	}
}

func TestGet_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	f, err := store.Get(ctx, 999)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if f != nil {
		t.Fatal("expected nil for non-existent fact")
	}
}

func TestInsertBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	facts := []memstore.Fact{
		{Content: "fact one", Subject: "test", Category: "note"},
		{Content: "fact two", Subject: "test", Category: "project"},
		{Content: "fact three", Subject: "other", Category: "note"},
	}

	if err := store.InsertBatch(ctx, facts); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	for i, f := range facts {
		if f.ID == 0 {
			t.Fatalf("fact %d: expected non-zero id", i)
		}
	}

	count, err := store.ActiveCount(ctx)
	if err != nil {
		t.Fatalf("active count: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3, got %d", count)
	}
}

func TestSupersede(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "old fact", Subject: "test", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "new fact", Subject: "test", Category: "note"})

	if err := store.Supersede(ctx, id1, id2); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	f, _ := store.Get(ctx, id1)
	if f.SupersededBy == nil || *f.SupersededBy != id2 {
		t.Fatal("expected superseded_by to point to new fact")
	}
	if f.SupersededAt == nil {
		t.Fatal("expected superseded_at to be set")
	}
}

func TestConfirm(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, _ := store.Insert(ctx, memstore.Fact{Content: "confirm me", Subject: "test", Category: "note"})

	if err := store.Confirm(ctx, id); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	f, _ := store.Get(ctx, id)
	if f.ConfirmedCount != 1 {
		t.Fatalf("expected confirmed_count 1, got %d", f.ConfirmedCount)
	}
	if f.LastConfirmedAt == nil {
		t.Fatal("expected last_confirmed_at to be set")
	}
}

func TestTouch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, _ := store.Insert(ctx, memstore.Fact{Content: "touch me", Subject: "test", Category: "note"})

	if err := store.Touch(ctx, []int64{id}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	f, _ := store.Get(ctx, id)
	if f.UseCount != 1 {
		t.Fatalf("expected use_count 1, got %d", f.UseCount)
	}
}

func TestDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, _ := store.Insert(ctx, memstore.Fact{Content: "delete me", Subject: "test", Category: "note"})

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	f, _ := store.Get(ctx, id)
	if f != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestUpdateMetadata(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	meta, _ := json.Marshal(map[string]any{"key1": "val1", "key2": "val2"})
	id, _ := store.Insert(ctx, memstore.Fact{
		Content:  "metadata test",
		Subject:  "test",
		Category: "note",
		Metadata: meta,
	})

	// Set a new key, delete an existing one.
	if err := store.UpdateMetadata(ctx, id, map[string]any{
		"key2": nil,
		"key3": "val3",
	}); err != nil {
		t.Fatalf("update metadata: %v", err)
	}

	f, _ := store.Get(ctx, id)
	var m map[string]any
	json.Unmarshal(f.Metadata, &m)
	if m["key1"] != "val1" {
		t.Fatal("expected key1 preserved")
	}
	if _, ok := m["key2"]; ok {
		t.Fatal("expected key2 deleted")
	}
	if m["key3"] != "val3" {
		t.Fatal("expected key3 added")
	}
}

func TestList(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "fact one", Subject: "test", Category: "note"})
	store.Insert(ctx, memstore.Fact{Content: "fact two", Subject: "test", Category: "project"})
	store.Insert(ctx, memstore.Fact{Content: "fact three", Subject: "other", Category: "note"})

	facts, err := store.List(ctx, memstore.QueryOpts{Subject: "test"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}

	// Filter by category.
	facts, err = store.List(ctx, memstore.QueryOpts{Subject: "test", Category: "note"})
	if err != nil {
		t.Fatalf("list with category: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
}

func TestList_KindAndSubsystem(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "inv1", Subject: "test", Category: "project", Kind: "invariant", Subsystem: "auth"})
	store.Insert(ctx, memstore.Fact{Content: "conv1", Subject: "test", Category: "project", Kind: "convention", Subsystem: "auth"})
	store.Insert(ctx, memstore.Fact{Content: "inv2", Subject: "test", Category: "project", Kind: "invariant", Subsystem: "feeds"})

	facts, _ := store.List(ctx, memstore.QueryOpts{Kind: "invariant"})
	if len(facts) != 2 {
		t.Fatalf("expected 2 invariants, got %d", len(facts))
	}

	facts, _ = store.List(ctx, memstore.QueryOpts{Subsystem: "auth"})
	if len(facts) != 2 {
		t.Fatalf("expected 2 auth facts, got %d", len(facts))
	}
}

func TestList_OnlyActive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "old", Subject: "test", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "new", Subject: "test", Category: "note"})
	store.Supersede(ctx, id1, id2)

	facts, _ := store.List(ctx, memstore.QueryOpts{Subject: "test", OnlyActive: true})
	if len(facts) != 1 {
		t.Fatalf("expected 1 active fact, got %d", len(facts))
	}
	if facts[0].ID != id2 {
		t.Fatalf("expected active fact to be id %d, got %d", id2, facts[0].ID)
	}
}

func TestBySubject(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "a", Subject: "alice", Category: "note"})
	store.Insert(ctx, memstore.Fact{Content: "b", Subject: "bob", Category: "note"})

	facts, _ := store.BySubject(ctx, "alice", false)
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact for alice, got %d", len(facts))
	}
}

func TestExists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "unique content", Subject: "test", Category: "note"})

	exists, _ := store.Exists(ctx, "unique content", "test")
	if !exists {
		t.Fatal("expected fact to exist")
	}

	exists, _ = store.Exists(ctx, "no such content", "test")
	if exists {
		t.Fatal("expected fact not to exist")
	}
}

func TestActiveCount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	count, _ := store.ActiveCount(ctx)
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}

	store.Insert(ctx, memstore.Fact{Content: "one", Subject: "test", Category: "note"})
	count, _ = store.ActiveCount(ctx)
	if count != 1 {
		t.Fatalf("expected 1, got %d", count)
	}
}

func TestHistory_ByID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "v1", Subject: "test", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "v2", Subject: "test", Category: "note"})
	id3, _ := store.Insert(ctx, memstore.Fact{Content: "v3", Subject: "test", Category: "note"})
	store.Supersede(ctx, id1, id2)
	store.Supersede(ctx, id2, id3)

	entries, err := store.History(ctx, id2, "")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Fact.ID != id1 {
		t.Fatalf("expected first entry to be id %d, got %d", id1, entries[0].Fact.ID)
	}
}

func TestHistory_BySubject(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "a", Subject: "test", Category: "note"})
	store.Insert(ctx, memstore.Fact{Content: "b", Subject: "test", Category: "note"})

	entries, err := store.History(ctx, 0, "test")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestListSubsystems(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "a", Subject: "test", Category: "project", Subsystem: "auth"})
	store.Insert(ctx, memstore.Fact{Content: "b", Subject: "test", Category: "project", Subsystem: "feeds"})
	store.Insert(ctx, memstore.Fact{Content: "c", Subject: "test", Category: "project", Subsystem: ""})

	subs, err := store.ListSubsystems(ctx, "")
	if err != nil {
		t.Fatalf("list subsystems: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subsystems, got %d", len(subs))
	}
}

func TestSetEmbedding(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, _ := store.Insert(ctx, memstore.Fact{Content: "embed me", Subject: "test", Category: "note"})

	emb := []float32{0.1, 0.2, 0.3, 0.4}
	if err := store.SetEmbedding(ctx, id, emb); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	f, _ := store.Get(ctx, id)
	if len(f.Embedding) != 4 {
		t.Fatalf("expected 4-dim embedding, got %d", len(f.Embedding))
	}
}

func TestNeedingEmbedding(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "no embed", Subject: "test", Category: "note"})

	facts, err := store.NeedingEmbedding(ctx, 10)
	if err != nil {
		t.Fatalf("needing embedding: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact needing embedding, got %d", len(facts))
	}
}

func TestSearchFTS(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{Content: "PostgreSQL is a relational database", Subject: "postgres", Category: "project"})
	store.Insert(ctx, memstore.Fact{Content: "SQLite is an embedded database", Subject: "sqlite", Category: "project"})

	// Give Postgres a moment to update the tsvector column.
	time.Sleep(50 * time.Millisecond)

	results, err := store.SearchFTS(ctx, "PostgreSQL", memstore.SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("search FTS: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 FTS result")
	}
	if results[0].Fact.Subject != "postgres" {
		t.Fatalf("expected top result subject 'postgres', got %q", results[0].Fact.Subject)
	}
}

// --- Link tests ---

func TestLinkFacts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "room A", Subject: "dungeon", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "room B", Subject: "dungeon", Category: "note"})

	linkID, err := store.LinkFacts(ctx, id1, id2, "passage", false, "north door", nil)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if linkID == 0 {
		t.Fatal("expected non-zero link id")
	}

	// GetLink
	link, err := store.GetLink(ctx, linkID)
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if link.LinkType != "passage" {
		t.Fatalf("expected link type 'passage', got %q", link.LinkType)
	}

	// GetLinks
	links, err := store.GetLinks(ctx, id1, memstore.LinkOutbound)
	if err != nil {
		t.Fatalf("get links: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}

	// DeleteLink
	if err := store.DeleteLink(ctx, linkID); err != nil {
		t.Fatalf("delete link: %v", err)
	}

	links, _ = store.GetLinks(ctx, id1, memstore.LinkBoth)
	if len(links) != 0 {
		t.Fatalf("expected 0 links after delete, got %d", len(links))
	}
}

func TestLinkBidirectional(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "A", Subject: "test", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "B", Subject: "test", Category: "note"})

	store.LinkFacts(ctx, id1, id2, "reference", true, "bidi", nil)

	// Outbound from id2 should include the bidirectional link.
	links, _ := store.GetLinks(ctx, id2, memstore.LinkOutbound)
	if len(links) != 1 {
		t.Fatalf("expected 1 outbound link from target of bidi, got %d", len(links))
	}
}

func TestUpdateLink(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "A", Subject: "test", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "B", Subject: "test", Category: "note"})

	linkID, _ := store.LinkFacts(ctx, id1, id2, "ref", false, "original", map[string]any{"k": "v"})

	if err := store.UpdateLink(ctx, linkID, "updated", map[string]any{"k": nil, "k2": "v2"}); err != nil {
		t.Fatalf("update link: %v", err)
	}

	link, _ := store.GetLink(ctx, linkID)
	if link.Label != "updated" {
		t.Fatalf("expected label 'updated', got %q", link.Label)
	}
	var m map[string]any
	json.Unmarshal(link.Metadata, &m)
	if _, ok := m["k"]; ok {
		t.Fatal("expected key 'k' deleted")
	}
	if m["k2"] != "v2" {
		t.Fatal("expected key 'k2' added")
	}
}

func TestCascadeDeleteLinks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id1, _ := store.Insert(ctx, memstore.Fact{Content: "A", Subject: "test", Category: "note"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "B", Subject: "test", Category: "note"})

	store.LinkFacts(ctx, id1, id2, "ref", false, "", nil)

	// Deleting the source fact should cascade-delete the link.
	store.Delete(ctx, id1)

	links, _ := store.GetLinks(ctx, id2, memstore.LinkBoth)
	if len(links) != 0 {
		t.Fatalf("expected 0 links after cascade delete, got %d", len(links))
	}
}

// --- Interface compliance ---

func TestStoreInterface(t *testing.T) {
	// Compile-time check that PostgresStore implements Store.
	var _ memstore.Store = (*pgstore.PostgresStore)(nil)
}
