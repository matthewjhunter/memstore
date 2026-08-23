package memstore_test

import (
	"database/sql"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

func TestEmbedRecipe_ModelMovesIt(t *testing.T) {
	if memstore.EmbedRecipe("nomic-embed-text") == memstore.EmbedRecipe("embeddinggemma") {
		t.Error("two models share a recipe")
	}
}

// The recipe must not vary between two computations in the same deployment, or
// the store would clear its vectors on every startup.
func TestEmbedRecipe_IsStable(t *testing.T) {
	if a, b := memstore.EmbedRecipe(chunkModel), memstore.EmbedRecipe(chunkModel); a != b {
		t.Errorf("same model gave %q and %q", a, b)
	}
}

// The payoff: a recipe change is detected and self-heals, instead of needing a
// hand-written migration whose only content is "delete every vector". Chunking
// and task prefixes each needed one of those, written from whoever changed the
// recipe remembering that they had.
func TestReopen_RecipeChangeClearsVectorsForReEmbedding(t *testing.T) {
	db := openSharedTestDB(t)
	ctx := t.Context()

	first := newStoreOnDB(t, db, &mockEmbedder{dim: 4, model: "nomic-embed-text"})
	id, err := first.Insert(ctx, memstore.Fact{Content: "a fact", Subject: "S", Category: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.EmbedFacts(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if chunks, _ := first.FactChunks(ctx, id); len(chunks) == 0 {
		t.Fatal("nothing was embedded, so there is nothing for a recipe change to invalidate")
	}

	// Reopening under a different model changes the recipe -- but also the
	// model, which must NOT self-heal. Rewrite just the recipe to isolate it.
	if _, err := db.Exec(
		`UPDATE memstore_meta SET value = 'stale-recipe' WHERE key = 'embedding_recipe'`); err != nil {
		t.Fatal(err)
	}

	second := newStoreOnDB(t, db, &mockEmbedder{dim: 4, model: "nomic-embed-text"})

	chunks, err := second.FactChunks(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Errorf("%d chunk rows survived a recipe change; they were produced by a different "+
			"recipe and would be ranked against queries from the new one", len(chunks))
	}
	pending, err := second.NeedingEmbedding(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("%d facts queued for re-embedding, want 1 -- clearing without re-queueing "+
			"leaves the fact permanently unembedded", len(pending))
	}
}

// A model change must NOT self-heal: it usually means something moved
// underneath the deployment, and clearing vectors would hide that.
func TestReopen_ModelChangeIsRefusedNotCleared(t *testing.T) {
	db := openSharedTestDB(t)
	ctx := t.Context()

	first := newStoreOnDB(t, db, &mockEmbedder{dim: 4, model: "nomic-embed-text"})
	if _, err := first.Insert(ctx, memstore.Fact{Content: "a fact", Subject: "S", Category: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.EmbedFacts(ctx, 10); err != nil {
		t.Fatal(err)
	}

	_, err := memstore.NewSQLiteStore(db, &mockEmbedder{dim: 4, model: "embeddinggemma"}, "test")
	if err == nil {
		t.Fatal("a model change was accepted; stored vectors from another model would be served")
	}
}

// openSharedTestDB returns a database that survives being reopened by several
// stores, so a fingerprint written by one is seen by the next.
func openSharedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func newStoreOnDB(t *testing.T, db *sql.DB, e embedding.Embedder) *memstore.SQLiteStore {
	t.Helper()
	s, err := memstore.NewSQLiteStore(db, e, "test")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s
}
