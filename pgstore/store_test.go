package pgstore_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/conformance"
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

func (m *mockEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "mock", Dim: m.dim}
}

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
	store, err := pgstore.New(ctx, pool, embedder, "test", 4, 512)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	return store
}

// countingEmbedder wraps mockEmbedder and records how many texts it embeds,
// letting tests assert that the query cache prevents repeat embeds.
type countingEmbedder struct {
	mockEmbedder
	embedded int
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.embedded += len(texts)
	return c.mockEmbedder.Embed(ctx, texts)
}

// newTestStoreWithEmbedder builds a store on a clean schema using the given
// embedder and query-cache size.
func newTestStoreWithEmbedder(t *testing.T, embedder embedding.Embedder, dim, cacheSize int) *pgstore.PostgresStore {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)

	store, err := pgstore.New(ctx, pool, embedder, "test", dim, cacheSize)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	return store
}

func TestSearchCachesQueryEmbedding(t *testing.T) {
	emb := &countingEmbedder{mockEmbedder: mockEmbedder{dim: 4}}
	store := newTestStoreWithEmbedder(t, emb, 4, 512)
	ctx := context.Background()

	if _, err := store.Insert(ctx, memstore.Fact{
		Content: "the quick brown fox", Subject: "animals", Category: "note",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	opts := memstore.SearchOpts{MaxResults: 5}
	if _, err := store.Search(ctx, "quick fox", opts); err != nil {
		t.Fatalf("Search: %v", err)
	}
	afterFirst := emb.embedded

	// A normalized variant of the same query must not re-embed.
	if _, err := store.Search(ctx, "  Quick   Fox ", opts); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if emb.embedded != afterFirst {
		t.Errorf("repeat query re-embedded: %d embeds after second search, want %d", emb.embedded, afterFirst)
	}

	// A genuinely different query must embed.
	if _, err := store.Search(ctx, "lazy dog", opts); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if emb.embedded <= afterFirst {
		t.Errorf("distinct query was not embedded: still %d embeds", emb.embedded)
	}
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

func TestList_InvalidMetadataFilterErrors(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{
		Content: "alpha fact", Subject: "test", Category: "note",
		Metadata: json.RawMessage(`{"tier":"gold"}`),
	})
	store.Insert(ctx, memstore.Fact{
		Content: "beta fact", Subject: "test", Category: "note",
		Metadata: json.RawMessage(`{"tier":"silver"}`),
	})

	// Invalid key must error, not silently drop the filter.
	_, err := store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "bad-key!", Op: "=", Value: "x"}},
	})
	if err == nil {
		t.Fatal("List with invalid metadata key: expected error, got nil")
	}

	// Invalid operator must error too.
	_, err = store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "tier", Op: "LIKE", Value: "x"}},
	})
	if err == nil {
		t.Fatal("List with invalid metadata operator: expected error, got nil")
	}

	// SearchFTS must reject the same invalid filters.
	_, err = store.SearchFTS(ctx, "alpha", memstore.SearchOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "bad-key!", Op: "=", Value: "x"}},
	})
	if err == nil {
		t.Fatal("SearchFTS with invalid metadata key: expected error, got nil")
	}
	_, err = store.SearchFTS(ctx, "alpha", memstore.SearchOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "tier", Op: "LIKE", Value: "x"}},
	})
	if err == nil {
		t.Fatal("SearchFTS with invalid metadata operator: expected error, got nil")
	}

	// A valid filter still works -- this also proves jsonb_extract_path_text
	// is equivalent to the ->> operator for top-level keys.
	facts, err := store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "tier", Op: "=", Value: "gold"}},
	})
	if err != nil {
		t.Fatalf("List with valid metadata filter: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("valid filter: expected 1 fact, got %d", len(facts))
	}
	if facts[0].Content != "alpha fact" {
		t.Fatalf("valid filter: expected alpha fact, got %q", facts[0].Content)
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

func TestHistory_CycleTerminates(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)

	// Build a dedicated pool for raw SQL manipulation.
	rawPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting for raw SQL: %v", err)
	}
	defer rawPool.Close()

	// Clean slate (newTestStore also does this, but we need our own store instance).
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)

	store, err := pgstore.New(ctx, rawPool, &mockEmbedder{dim: 4}, "test", 4, 512)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	idA, err := store.Insert(ctx, memstore.Fact{Content: "fact A", Subject: "X", Category: "test"})
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	idB, err := store.Insert(ctx, memstore.Fact{Content: "fact B", Subject: "X", Category: "test"})
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}

	// Force a cycle: A -> B -> A via raw SQL.
	if _, err := rawPool.Exec(ctx, `UPDATE memstore_facts SET superseded_by = $1 WHERE id = $2`, idB, idA); err != nil {
		t.Fatalf("set A->B: %v", err)
	}
	if _, err := rawPool.Exec(ctx, `UPDATE memstore_facts SET superseded_by = $1 WHERE id = $2`, idA, idB); err != nil {
		t.Fatalf("set B->A: %v", err)
	}

	entries, err := store.History(ctx, idA, "")
	if err != nil {
		t.Fatalf("History returned error on cyclic data: %v", err)
	}
	if len(entries) > 3 {
		t.Errorf("expected at most 3 entries for a 2-node cycle, got %d", len(entries))
	}
	if len(entries) == 0 {
		t.Error("expected at least 1 entry (the anchor itself)")
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

func TestMarkEmbedFailed_Quarantines(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{Content: "unembeddable", Subject: "test", Category: "note"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := store.MarkEmbedFailed(ctx, id, "input exceeds context length"); err != nil {
		t.Fatalf("MarkEmbedFailed: %v", err)
	}

	facts, err := store.NeedingEmbedding(ctx, 10)
	if err != nil {
		t.Fatalf("NeedingEmbedding: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("quarantined fact still needs embedding: got %d facts", len(facts))
	}
}

func TestInsert_RejectsOversizedContent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	atLimit := strings.Repeat("a", memstore.MaxContentLength)
	if _, err := store.Insert(ctx, memstore.Fact{
		Content: atLimit, Subject: "test", Category: "note",
	}); err != nil {
		t.Fatalf("Insert at limit: %v", err)
	}

	overLimit := strings.Repeat("a", memstore.MaxContentLength+1)
	if _, err := store.Insert(ctx, memstore.Fact{
		Content: overLimit, Subject: "test", Category: "note",
	}); err == nil {
		t.Fatal("Insert over limit: expected error, got nil")
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

func TestConformance(t *testing.T) {
	dsn := os.Getenv("MEMSTORE_TEST_PG")
	if dsn == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping PostgreSQL conformance tests")
	}

	ctx := context.Background()

	// newPool opens a pgxpool, drops and recreates the schema, and registers
	// cleanup. A fresh schema ensures subtests start from a clean slate.
	newPool := func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatalf("pgxpool.New: %v", err)
		}
		t.Cleanup(pool.Close)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
		return pool
	}

	// lastPool holds the pool used by the most recent NewStore call so that
	// SetSupersededBy can target the same database as the store it mutates.
	var lastPool *pgxpool.Pool

	// sharedNSPool and sharedNST back the NewStoreNS factory: the first call
	// for a given subtest t creates a fresh pool; subsequent calls with the
	// same t reuse it so both namespaces land in the same Postgres schema.
	var sharedNSPool *pgxpool.Pool
	var sharedNST *testing.T

	conformance.Run(t, conformance.Options{
		NewStore: func(t *testing.T) memstore.Store {
			pool := newPool(t)
			lastPool = pool
			store, err := pgstore.New(ctx, pool, &mockEmbedder{dim: 4}, "test", 4, 512)
			if err != nil {
				t.Fatalf("pgstore.New: %v", err)
			}
			return store
		},

		// NewStoreNS creates stores on a shared pool so namespace isolation is
		// tested at the SQL scoping level rather than the connection level.
		// pgstore.New is idempotent (CREATE TABLE IF NOT EXISTS), so calling it
		// with different namespace strings on the same pool is safe.
		NewStoreNS: func(t *testing.T, ns string) memstore.Store {
			if sharedNST != t {
				sharedNSPool = newPool(t)
				sharedNST = t
			}
			store, err := pgstore.New(ctx, sharedNSPool, &mockEmbedder{dim: 4}, ns, 4, 512)
			if err != nil {
				t.Fatalf("pgstore.New ns=%q: %v", ns, err)
			}
			return store
		},

		// SetSupersededBy writes directly to lastPool using Postgres $N
		// placeholder syntax, bypassing Store validation to force a cycle.
		SetSupersededBy: func(t *testing.T, supersededByID, targetID int64) {
			if lastPool == nil {
				t.Fatal("SetSupersededBy called before any NewStore; no pool available")
			}
			if _, err := lastPool.Exec(ctx,
				`UPDATE memstore_facts SET superseded_by = $1 WHERE id = $2`,
				supersededByID, targetID,
			); err != nil {
				t.Fatalf("SetSupersededBy(%d->%d): %v", supersededByID, targetID, err)
			}
		},
	})
}

// TestList_NumericMetadataFilter verifies that pgstore compares numeric
// metadata filter values numerically rather than as text. Requires a running
// Postgres instance (MEMSTORE_TEST_PG).
func TestList_NumericMetadataFilter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	store.Insert(ctx, memstore.Fact{ //nolint:errcheck
		Content: "low chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{"chapter":1}`),
	})
	store.Insert(ctx, memstore.Fact{ //nolint:errcheck
		Content: "high chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{"chapter":9}`),
	})
	store.Insert(ctx, memstore.Fact{ //nolint:errcheck
		Content: "missing chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{}`),
	})
	store.Insert(ctx, memstore.Fact{ //nolint:errcheck
		Content: "non-numeric chapter", Subject: "numeric", Category: "test",
		Metadata: json.RawMessage(`{"chapter":"not-a-number"}`),
	})

	// <= with int: only the low-chapter fact matches.
	facts, err := store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "chapter", Op: "<=", Value: 3}},
	})
	if err != nil {
		t.Fatalf("List <= int: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "low chapter" {
		t.Errorf("List <=3: got %d facts, want 1 (low chapter); contents: %v", len(facts), factContents(facts))
	}

	// = with float64 (JSON-decoded form): matches chapter:1.
	facts, err = store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "chapter", Op: "=", Value: float64(1)}},
	})
	if err != nil {
		t.Fatalf("List = float64: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "low chapter" {
		t.Errorf("List =1.0: got %d facts, want 1 (low chapter); contents: %v", len(facts), factContents(facts))
	}

	// > excludes all facts with chapter <= 3 and missing/non-numeric.
	facts, err = store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "chapter", Op: ">", Value: 3}},
	})
	if err != nil {
		t.Fatalf("List > int: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "high chapter" {
		t.Errorf("List >3: got %d facts, want 1 (high chapter); contents: %v", len(facts), factContents(facts))
	}

	// Without IncludeNull: missing-key and non-numeric facts are both excluded.
	facts, err = store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "chapter", Op: "<=", Value: 3, IncludeNull: false}},
	})
	if err != nil {
		t.Fatalf("List <= without IncludeNull: %v", err)
	}
	for _, f := range facts {
		if f.Content == "missing chapter" || f.Content == "non-numeric chapter" {
			t.Errorf("List <= without IncludeNull: unexpected fact %q in results", f.Content)
		}
	}

	// With IncludeNull: missing-key fact is included; non-numeric fact is still excluded.
	facts, err = store.List(ctx, memstore.QueryOpts{
		MetadataFilters: []memstore.MetadataFilter{{Key: "chapter", Op: "<=", Value: 3, IncludeNull: true}},
	})
	if err != nil {
		t.Fatalf("List <= with IncludeNull: %v", err)
	}
	var gotLow, gotMissing, gotHigh, gotNonNumeric bool
	for _, f := range facts {
		switch f.Content {
		case "low chapter":
			gotLow = true
		case "missing chapter":
			gotMissing = true
		case "high chapter":
			gotHigh = true
		case "non-numeric chapter":
			gotNonNumeric = true
		}
	}
	if !gotLow {
		t.Error("List <= with IncludeNull: expected low chapter fact, not found")
	}
	if !gotMissing {
		t.Error("List <= with IncludeNull: expected missing chapter fact (IncludeNull), not found")
	}
	if gotHigh {
		t.Error("List <= with IncludeNull: high chapter fact should not match <= 3")
	}
	if gotNonNumeric {
		t.Error("List <= with IncludeNull: non-numeric chapter should not match even with IncludeNull")
	}
}

// factContents is a test helper that returns the Content fields of a slice.
func factContents(facts []memstore.Fact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Content
	}
	return out
}
