package pgstore_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	return newTestStoreNS(t, "test")
}

// newTestStoreNS creates a fresh store in the given namespace with a default
// user seeded so that Insert works and UserID is non-zero. It drops all
// memstore_* tables from previous runs before migrating.
func newTestStoreNS(t *testing.T, ns string) *pgstore.PostgresStore {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	// Clean up tables from previous test runs. api_tokens is dropped too so the
	// V4 migration can't infer a default user from a token another package left
	// on the shared CI Postgres (which would turn the tolerated tier3-init error
	// into an ambiguous-user failure).
	pool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)

	embedder := &mockEmbedder{dim: 4}

	// First pass migrates the schema. On a fresh DB it fails at user
	// resolution (no default user recorded yet); the migration itself is
	// committed before that check, so the failure is expected and benign.
	if _, err := pgstore.New(ctx, pool, embedder, ns, 4, 512); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("first pgstore.New (schema init): %v", err)
	}
	// Seed the identity so subsequent opens get a real userID.
	if err := pgstore.InitIdentity(ctx, pool, ns, "testuser"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
	// Second pass: resolveUser now finds the seeded row.
	store, err := pgstore.New(ctx, pool, embedder, ns, 4, 512)
	if err != nil {
		t.Fatalf("second pgstore.New (with identity): %v", err)
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

	// api_tokens is dropped too so the V4 migration can't infer a default user
	// from a token another package left on the shared CI Postgres.
	pool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)

	// Migrate first (expected to fail at user resolution on a fresh DB),
	// seed identity, then open again with resolved userID.
	if _, err := pgstore.New(ctx, pool, embedder, "test", dim, cacheSize); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("first pgstore.New (schema init): %v", err)
	}
	if err := pgstore.InitIdentity(ctx, pool, "test", "testuser"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
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
	// api_tokens is dropped too so the V4 migration can't infer a default user
	// from a token another package left on the shared CI Postgres.
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
	rawPool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)

	// Fresh-DB construction fails at user resolution until an identity is
	// seeded; the first New still commits the schema migration.
	if _, err := pgstore.New(ctx, rawPool, &mockEmbedder{dim: 4}, "test", 4, 512); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("pgstore.New (schema init): %v", err)
	}
	if err := pgstore.InitIdentity(ctx, rawPool, "test", "testuser"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
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
		// api_tokens is dropped too so the V4 migration can't infer a default
		// user from a token another package left on the shared CI Postgres.
		pool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
		pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)
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

	// initStore migrates schema, seeds identity for ns, and returns a store
	// with a non-zero userID. The first pgstore.New on a fresh pool fails at
	// user resolution (no default user recorded yet); that is expected.
	initStore := func(t *testing.T, pool *pgxpool.Pool, ns string) *pgstore.PostgresStore {
		t.Helper()
		emb := &mockEmbedder{dim: 4}
		if _, err := pgstore.New(ctx, pool, emb, ns, 4, 512); err != nil && !strings.Contains(err.Error(), "tier3-init") {
			t.Fatalf("pgstore.New schema init ns=%q: %v", ns, err)
		}
		if err := pgstore.InitIdentity(ctx, pool, ns, "testuser"); err != nil {
			t.Fatalf("InitIdentity ns=%q: %v", ns, err)
		}
		store, err := pgstore.New(ctx, pool, emb, ns, 4, 512)
		if err != nil {
			t.Fatalf("pgstore.New ns=%q: %v", ns, err)
		}
		return store
	}

	conformance.Run(t, conformance.Options{
		NewStore: func(t *testing.T) memstore.Store {
			pool := newPool(t)
			lastPool = pool
			return initStore(t, pool, "test")
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
			return initStore(t, sharedNSPool, ns)
		},

		// NewTwoUserStores derives two user-scoped stores from one base store
		// on a fresh pool, exercising the owner predicates at the SQL level.
		NewTwoUserStores: func(t *testing.T) (memstore.Store, memstore.Store) {
			pool := newPool(t)
			lastPool = pool
			base := initStore(t, pool, "test")
			uidA, err := pgstore.EnsureUser(ctx, pool, "test", "iso-user-a")
			if err != nil {
				t.Fatalf("EnsureUser iso-user-a: %v", err)
			}
			uidB, err := pgstore.EnsureUser(ctx, pool, "test", "iso-user-b")
			if err != nil {
				t.Fatalf("EnsureUser iso-user-b: %v", err)
			}
			a, err := base.ForUser(uidA)
			if err != nil {
				t.Fatalf("ForUser(%d): %v", uidA, err)
			}
			b, err := base.ForUser(uidB)
			if err != nil {
				t.Fatalf("ForUser(%d): %v", uidB, err)
			}
			return a, b
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

func TestForUser_InvalidID(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.ForUser(0); err == nil {
		t.Error("ForUser(0) succeeded, want error")
	}
	if _, err := store.ForUser(-5); err == nil {
		t.Error("ForUser(-5) succeeded, want error")
	}
}

// secondUserStore provisions an extra user in the "test" namespace and
// returns a store scoped to it. Must be called after newTestStore so the
// schema and identity exist.
func secondUserStore(t *testing.T, base *pgstore.PostgresStore, name string) memstore.Store {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	uid, err := pgstore.EnsureUser(ctx, pool, "test", name)
	if err != nil {
		t.Fatalf("EnsureUser(%q): %v", name, err)
	}
	scoped, err := base.ForUser(uid)
	if err != nil {
		t.Fatalf("ForUser(%d): %v", uid, err)
	}
	return scoped
}

func TestServiceScope_SeesAllUsers(t *testing.T) {
	store := newTestStore(t) // scoped to the default user "testuser"
	ctx := context.Background()
	other := secondUserStore(t, store, "seconduser")

	if _, err := store.Insert(ctx, memstore.Fact{Content: "default user fact", Subject: "svc", Category: "test"}); err != nil {
		t.Fatalf("Insert default: %v", err)
	}
	if _, err := other.Insert(ctx, memstore.Fact{Content: "second user fact", Subject: "svc", Category: "test"}); err != nil {
		t.Fatalf("Insert second: %v", err)
	}

	// Scoped stores each see only their own fact.
	defFacts, err := store.List(ctx, memstore.QueryOpts{Subject: "svc"})
	if err != nil {
		t.Fatalf("default List: %v", err)
	}
	if len(defFacts) != 1 {
		t.Errorf("default-scoped store sees %d facts, want 1", len(defFacts))
	}
	otherFacts, err := other.List(ctx, memstore.QueryOpts{Subject: "svc"})
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if len(otherFacts) != 1 {
		t.Errorf("second-scoped store sees %d facts, want 1", len(otherFacts))
	}

	// The service scope is the one place cross-user visibility is correct.
	svc := store.ServiceScope()
	all, err := svc.List(ctx, memstore.QueryOpts{Subject: "svc"})
	if err != nil {
		t.Fatalf("service List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("service scope sees %d facts, want 2", len(all))
	}
}

func TestLinkFacts_CrossUserRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	other := secondUserStore(t, store, "seconduser")

	src, err := store.Insert(ctx, memstore.Fact{Content: "owner src", Subject: "links", Category: "test"})
	if err != nil {
		t.Fatalf("Insert src: %v", err)
	}
	tgt, err := store.Insert(ctx, memstore.Fact{Content: "owner tgt", Subject: "links", Category: "test"})
	if err != nil {
		t.Fatalf("Insert tgt: %v", err)
	}

	// Another user linking the owner's facts must fail with a
	// not-found-shaped error: existence must not leak.
	_, err = other.LinkFacts(ctx, src, tgt, "ref", false, "", nil)
	if err == nil {
		t.Fatal("cross-user LinkFacts succeeded")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("cross-user LinkFacts error %q is not not-found-shaped", err)
	}

	// The owner can still link, including self-links (preserved semantics).
	if _, err := store.LinkFacts(ctx, src, tgt, "ref", false, "ok", nil); err != nil {
		t.Errorf("owner LinkFacts failed: %v", err)
	}
	if _, err := store.LinkFacts(ctx, src, src, "self", false, "", nil); err != nil {
		t.Errorf("owner self-link failed: %v", err)
	}
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

// --- V4 migration tests ---

// dropAll cleans up all memstore_* tables on the given pool for a fresh slate.
func dropAll(ctx context.Context, pool *pgxpool.Pool) {
	pool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)
}

// TestMigrateV4_Fresh verifies that V4 migration on a clean DB with no tokens
// and no facts creates memstore_users but leaves it empty.
func TestMigrateV4_Fresh(t *testing.T) {
	if os.Getenv("MEMSTORE_TEST_PG") == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping pg migration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("MEMSTORE_TEST_PG"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	dropAll(ctx, pool)

	// Fresh DB: no tokens, no facts. The migration succeeds with no user rows,
	// but construction fails at user resolution with the tier3-init
	// instruction -- new deployments run tier3-init once.
	_, err = pgstore.New(ctx, pool, &mockEmbedder{dim: 4}, "test", 4, 512)
	if err == nil {
		t.Fatal("expected pgstore.New on a fresh DB to fail with the tier3-init instruction, got nil")
	}
	if !strings.Contains(err.Error(), "tier3-init") {
		t.Errorf("fresh-DB construction error should mention tier3-init: %v", err)
	}

	// memstore_users must exist.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'memstore_users')`,
	).Scan(&exists); err != nil {
		t.Fatalf("checking memstore_users: %v", err)
	}
	if !exists {
		t.Error("memstore_users table not found after V4 migration")
	}

	// No user rows expected on a fresh DB.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM memstore_users`).Scan(&count); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if count != 0 {
		t.Errorf("fresh DB: expected 0 user rows, got %d", count)
	}
}

// setupPreV4 builds a V3-shape schema by hand (version row = 3) with the
// given api_tokens names, so that opening the store triggers the V4
// migration against realistic pre-identity data. It returns the pool.
func setupPreV4(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tokenNames ...string) {
	t.Helper()
	dropAll(ctx, pool)
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	preV4Stmts := []string{
		`CREATE TABLE memstore_version (version INTEGER NOT NULL)`,
		`INSERT INTO memstore_version VALUES (3)`,
		`CREATE TABLE memstore_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		fmt.Sprintf(`CREATE TABLE memstore_facts (
			id BIGSERIAL PRIMARY KEY,
			namespace TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL CHECK (length(content) <= %d),
			subject TEXT NOT NULL,
			category TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			subsystem TEXT NOT NULL DEFAULT '',
			metadata JSONB,
			superseded_by BIGINT REFERENCES memstore_facts(id),
			superseded_at TIMESTAMPTZ,
			confirmed_count INTEGER NOT NULL DEFAULT 0,
			last_confirmed_at TIMESTAMPTZ,
			use_count INTEGER NOT NULL DEFAULT 0,
			last_used_at TIMESTAMPTZ,
			embedding vector(4),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			fts TSVECTOR GENERATED ALWAYS AS (
				setweight(to_tsvector('english', coalesce(subject, '')), 'A') ||
				setweight(to_tsvector('english', coalesce(content, '')), 'B') ||
				setweight(to_tsvector('english', coalesce(category, '')), 'C')
			) STORED,
			embed_failed_at TIMESTAMPTZ,
			embed_error TEXT
		)`, memstore.MaxContentLength),
		`CREATE TABLE memstore_links (
			id BIGSERIAL PRIMARY KEY,
			namespace TEXT NOT NULL DEFAULT '',
			source_id BIGINT NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			target_id BIGINT NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE,
			link_type TEXT NOT NULL DEFAULT 'reference',
			bidirectional BOOLEAN NOT NULL DEFAULT FALSE,
			label TEXT NOT NULL DEFAULT '',
			metadata JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE api_tokens (
			id BIGSERIAL PRIMARY KEY,
			token_hash BYTEA NOT NULL UNIQUE,
			name TEXT NOT NULL,
			scopes TEXT[] NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			last_used_at TIMESTAMPTZ,
			expires_at TIMESTAMPTZ,
			revoked_at TIMESTAMPTZ
		)`,
	}
	for _, s := range preV4Stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("pre-V4 setup: %v\nstmt: %s", err, s)
		}
	}
	for i, name := range tokenNames {
		if _, err := pool.Exec(ctx,
			`INSERT INTO api_tokens (token_hash, name, scopes) VALUES ($1, $2, '{"read"}')`,
			[]byte{byte(i + 1)}, name,
		); err != nil {
			t.Fatalf("pre-V4 token %q: %v", name, err)
		}
	}
}

// insertPreV4Fact inserts a fact directly into a pre-V4 facts table and
// returns its id.
func insertPreV4Fact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ns, content, subject, category string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO memstore_facts (namespace, content, subject, category) VALUES ($1, $2, $3, $4) RETURNING id`,
		ns, content, subject, category,
	).Scan(&id); err != nil {
		t.Fatalf("pre-V4 fact insert: %v", err)
	}
	return id
}

// TestMigrateV4_InferUser verifies that V4 migration infers the default user
// from a unanimous token name prefix, backfills facts, and rewrites
// ownership-only subjects to ” (empty string) -- subject stays NOT NULL.
func TestMigrateV4_InferUser(t *testing.T) {
	if os.Getenv("MEMSTORE_TEST_PG") == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping pg migration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("MEMSTORE_TEST_PG"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// All tokens share the "matthew" prefix -- unambiguous.
	setupPreV4(t, ctx, pool, "matthew-laptop", "matthew-workstation")
	// Pre-identity facts: one with subject overloaded as ownership marker
	// (rewritten to '') and one where the name is a genuine identity topic
	// (kept).
	projectID := insertPreV4Fact(t, ctx, pool, "test", "owned project fact", "matthew", "project")
	identityID := insertPreV4Fact(t, ctx, pool, "test", "identity fact", "matthew", "identity")

	// Opening with pgstore.New triggers V4 (store migration).
	// V4 infers "matthew" as the default user from the token prefix.
	store, err := pgstore.New(ctx, pool, &mockEmbedder{dim: 4}, "test", 4, 512)
	if err != nil {
		t.Fatalf("pgstore.New (V4 migration): %v", err)
	}

	// memstore_users should have one row for "matthew".
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM memstore_users`).Scan(&count); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 user row, got %d", count)
	}

	// All facts backfilled: no NULL user_id remains.
	var nullCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM memstore_facts WHERE user_id IS NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("counting NULL user_id: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("%d facts have NULL user_id after backfill", nullCount)
	}

	// Subject rewrite: the project fact's subject becomes '' (NOT NULL kept);
	// the identity fact keeps its subject.
	var projSubject, identSubject string
	if err := pool.QueryRow(ctx, `SELECT subject FROM memstore_facts WHERE id = $1`, projectID).Scan(&projSubject); err != nil {
		t.Fatalf("reading project fact subject: %v", err)
	}
	if projSubject != "" {
		t.Errorf("project fact subject = %q, want ''", projSubject)
	}
	if err := pool.QueryRow(ctx, `SELECT subject FROM memstore_facts WHERE id = $1`, identityID).Scan(&identSubject); err != nil {
		t.Fatalf("reading identity fact subject: %v", err)
	}
	if identSubject != "matthew" {
		t.Errorf("identity fact subject = %q, want \"matthew\"", identSubject)
	}

	// The subject column must remain NOT NULL.
	var subjectNullable string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'memstore_facts' AND column_name = 'subject'`,
	).Scan(&subjectNullable); err != nil {
		t.Fatalf("checking subject nullability: %v", err)
	}
	if subjectNullable != "NO" {
		t.Errorf("memstore_facts.subject is_nullable = %q, want NO", subjectNullable)
	}

	// Read the rewritten fact back through the store: the scan must handle
	// the rewritten subject and the new user_id column.
	got, err := store.Get(ctx, projectID)
	if err != nil {
		t.Fatalf("store.Get(rewritten fact): %v", err)
	}
	if got.Subject != "" {
		t.Errorf("Get: subject = %q, want ''", got.Subject)
	}
	if got.UserID == 0 {
		t.Error("Get: UserID = 0, want non-zero after backfill")
	}

	// Opening TokenStore triggers token migration, which rewrites token names.
	if _, err := pgstore.NewTokenStore(ctx, pool); err != nil {
		t.Fatalf("NewTokenStore (token migration): %v", err)
	}

	// Token names should have been rewritten to user@host shape.
	rows, err := pool.Query(ctx, `SELECT name FROM api_tokens ORDER BY name`)
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scanning name: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating names: %v", err)
	}
	wantNames := []string{"matthew@laptop", "matthew@workstation"}
	if len(names) != len(wantNames) {
		t.Fatalf("token names = %v, want %v", names, wantNames)
	}
	for i, want := range wantNames {
		if names[i] != want {
			t.Errorf("token[%d] name = %q, want %q", i, names[i], want)
		}
	}
}

// TestMigrateV4_AmbiguousUser verifies that V4 migration fails when token
// prefixes are ambiguous (multiple distinct user prefixes).
func TestMigrateV4_AmbiguousUser(t *testing.T) {
	if os.Getenv("MEMSTORE_TEST_PG") == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping pg migration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("MEMSTORE_TEST_PG"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Two different user prefixes: ambiguous.
	setupPreV4(t, ctx, pool, "matthew-laptop", "alice-desktop")

	// V4 must fail with an ambiguous-prefix error.
	_, err = pgstore.New(ctx, pool, &mockEmbedder{dim: 4}, "test", 4, 512)
	if err == nil {
		t.Fatal("expected error for ambiguous token prefixes, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguous prefixes: %v", err)
	}
}

// TestInitIdentity verifies the operator-recovery sequence end to end: an
// ambiguous fixture makes pgstore.New fail (the whole V4 transaction rolls
// back, including the memstore_users CREATE), then InitIdentity runs the
// full V4 work with an explicit user, and a subsequent pgstore.New succeeds
// with facts backfilled and constraints in place.
func TestInitIdentity(t *testing.T) {
	if os.Getenv("MEMSTORE_TEST_PG") == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping pg migration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("MEMSTORE_TEST_PG"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Ambiguous token prefixes plus a fact whose subject is the owner name.
	setupPreV4(t, ctx, pool, "matthew-laptop", "alice-desktop")
	factID := insertPreV4Fact(t, ctx, pool, "test", "owned project fact", "matthew", "project")

	// V4 inference must fail and roll back everything, including memstore_users.
	if _, err := pgstore.New(ctx, pool, &mockEmbedder{dim: 4}, "test", 4, 512); err == nil {
		t.Fatal("expected pgstore.New to fail on ambiguous token prefixes, got nil")
	}
	var usersExist bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'memstore_users')`,
	).Scan(&usersExist); err != nil {
		t.Fatalf("checking memstore_users after rollback: %v", err)
	}
	if usersExist {
		t.Error("memstore_users exists after failed V4 migration; expected full rollback")
	}

	// InitIdentity performs the full V4 work with the explicit user.
	if err := pgstore.InitIdentity(ctx, pool, "test", "matthew"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}

	// Subsequent open must resolve the user without re-running V4.
	store, err := pgstore.New(ctx, pool, &mockEmbedder{dim: 4}, "test", 4, 512)
	if err != nil {
		t.Fatalf("pgstore.New after InitIdentity: %v", err)
	}

	// The pre-existing fact is backfilled and its subject rewritten to ''.
	got, err := store.Get(ctx, factID)
	if err != nil {
		t.Fatalf("Get(pre-V4 fact): %v", err)
	}
	if got.UserID == 0 {
		t.Error("pre-V4 fact UserID is 0 after InitIdentity; expected backfill")
	}
	if got.Subject != "" {
		t.Errorf("pre-V4 fact subject = %q, want '' after rewrite", got.Subject)
	}

	// Constraints are in place: user_id NOT NULL and the FK to memstore_users.
	var userIDNullable string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'memstore_facts' AND column_name = 'user_id'`,
	).Scan(&userIDNullable); err != nil {
		t.Fatalf("checking user_id nullability: %v", err)
	}
	if userIDNullable != "NO" {
		t.Errorf("memstore_facts.user_id is_nullable = %q, want NO", userIDNullable)
	}
	var fkExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.table_constraints
			 WHERE table_name = 'memstore_facts'
			   AND constraint_name = 'memstore_facts_user_id_fkey'
			   AND constraint_type = 'FOREIGN KEY'
		)`,
	).Scan(&fkExists); err != nil {
		t.Fatalf("checking facts FK: %v", err)
	}
	if !fkExists {
		t.Error("memstore_facts_user_id_fkey not found after InitIdentity")
	}

	// The schema version was recorded as 4.
	var version int
	if err := pool.QueryRow(ctx, `SELECT version FROM memstore_version`).Scan(&version); err != nil {
		t.Fatalf("reading schema version: %v", err)
	}
	if version != 4 {
		t.Errorf("schema version = %d, want 4 after InitIdentity", version)
	}

	// Insert must succeed (non-zero userID bound to the store).
	id, err := store.Insert(ctx, memstore.Fact{
		Content: "identity check fact", Subject: "test", Category: "note",
	})
	if err != nil {
		t.Fatalf("Insert after InitIdentity: %v", err)
	}
	f, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if f.UserID == 0 {
		t.Error("fact UserID is 0 after InitIdentity; expected non-zero")
	}

	// InitIdentity is idempotent.
	if err := pgstore.InitIdentity(ctx, pool, "test", "matthew"); err != nil {
		t.Fatalf("InitIdentity idempotent call: %v", err)
	}
}
