// Package teststore opens a store for a test on whichever backend
// MEMSTORE_TEST_BACKEND names, so the same test file runs against SQLite and
// PostgreSQL without knowing which it got.
//
//	MEMSTORE_TEST_BACKEND=sqlite   in-memory SQLite (the default)
//	MEMSTORE_TEST_BACKEND=pg       a private PostgreSQL database per test,
//	                               skipped when MEMSTORE_TEST_PG is unset
//
// The daemon runs on PostgreSQL and only on PostgreSQL, so the pg run is the
// one that says whether the HTTP and MCP layers work; the sqlite run is kept
// while that backend is still shipped. CI runs both.
package teststore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/testpg"
	"github.com/matthewjhunter/memstore/pgstore"

	_ "modernc.org/sqlite"
)

// Store is what a test may hold: the full store union, the scoper that hands
// out capability-typed handles, and the two tuning entry points tests reach
// for that live on both backends but on no capability interface.
type Store interface {
	memstore.Store
	memstore.StoreScoper
	SetReranker(rr embedding.Reranker)
	TermDocCounts(ctx context.Context, terms []string) (map[string]int, int, error)
}

// DefaultUser is the owner recorded on a PostgreSQL test database. SQLite has
// no users, so tests that need to name one should use this constant rather
// than assume.
const DefaultUser = "testuser"

// Backend returns the selected backend name, "sqlite" or "pg".
func Backend() string {
	switch b := strings.ToLower(os.Getenv("MEMSTORE_TEST_BACKEND")); b {
	case "", "sqlite":
		return "sqlite"
	case "pg", "postgres", "postgresql":
		return "pg"
	default:
		panic("MEMSTORE_TEST_BACKEND=" + b + ": want sqlite or pg")
	}
}

// IsPG reports whether the selected backend is PostgreSQL. For the few tests
// whose expectations legitimately differ by backend; most should not ask.
func IsPG() bool { return Backend() == "pg" }

// VecDim is the embedding dimension the store is opened with. The mock
// embedders in the test packages all produce 4-wide vectors.
const VecDim = 4

// New opens a store on the selected backend. embedder may be nil for a store
// that cannot embed (the FTS-only fallback tests need one).
func New(t testing.TB, embedder embedding.Embedder, namespace string) Store {
	t.Helper()
	switch Backend() {
	case "pg":
		return newPG(t, embedder, namespace)
	default:
		return newSQLite(t, embedder, namespace)
	}
}

func newSQLite(t testing.TB, embedder embedding.Embedder, namespace string) Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("teststore: open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := memstore.NewSQLiteStore(db, embedder, namespace)
	if err != nil {
		t.Fatalf("teststore: NewSQLiteStore: %v", err)
	}
	return s
}

func newPG(t testing.TB, embedder embedding.Embedder, namespace string) Store {
	t.Helper()
	ctx := context.Background()
	pool := testpg.Pool(t)
	// First open lays the schema down and refuses for want of an owner; record
	// one and open again. Same sequence memstored runs with --default-user.
	s, err := pgstore.New(ctx, pool, embedder, namespace, VecDim, 512)
	if errors.Is(err, pgstore.ErrNoDefaultUser) {
		if err := pgstore.InitIdentity(ctx, pool, namespace, DefaultUser); err != nil {
			t.Fatalf("teststore: InitIdentity: %v", err)
		}
		s, err = pgstore.New(ctx, pool, embedder, namespace, VecDim, 512)
	}
	if err != nil {
		t.Fatalf("teststore: pgstore.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
