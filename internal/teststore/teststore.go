// Package teststore opens a memstore.Store on PostgreSQL for the HTTP, MCP,
// and client test suites. Every test gets a private database on the server
// MEMSTORE_TEST_PG names; without it the suites skip. The SQLite backend and
// its in-memory run were removed in 0.6.0.
package teststore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/screening"
	"github.com/matthewjhunter/memstore/internal/testpg"
	"github.com/matthewjhunter/memstore/pgstore"
)

// Store is what the suites need from a backend: the full Store contract plus
// the scoper and the two test-only hooks.
type Store interface {
	memstore.Store
	memstore.StoreScoper
	SetReranker(rr embedding.Reranker)
	TermDocCounts(ctx context.Context, terms []string) (map[string]int, int, error)
	// Screening knobs the store tests turn.
	SetScreenMode(m memstore.ScreenMode)
	SetInlineRejectScore(score int)
	SetDetectModes(write, read memstore.ScreenDetectMode)
	SetDetectReadScore(n int)
	SetDetectScoreForTest(ctx context.Context, id int64, score int) error
	DetectScore(ctx context.Context, id int64) (int, error)
	DetectWithheldCount(ctx context.Context) (int, error)
	// Screening queue and review, as screening.PendingStore and the admin
	// surface see them.
	PendingFacts(ctx context.Context, limit int) ([]screening.PendingFact, error)
	Resolve(ctx context.Context, id int64, d screening.Decision) error
	Defer(ctx context.Context, id int64, reason string) error
	Abandon(ctx context.Context, id int64, reason string) error
	ScreenCounts(ctx context.Context) (map[memstore.ScreenState]int, error)
	BlockedFacts(ctx context.Context, limit int) ([]memstore.BlockedFact, error)
	ReleaseFact(ctx context.Context, id int64) error
}

// DefaultUser is the owner recorded on a test database.
const DefaultUser = "testuser"

// VecDim is the embedding dimension the store is opened with. The mock
// embedders in the test packages all produce 4-wide vectors.
const VecDim = 4

// New opens a store on a fresh PostgreSQL database. embedder may be nil for a
// store that cannot embed (the FTS-only fallback tests need one).
func New(t testing.TB, embedder embedding.Embedder, namespace string) Store {
	t.Helper()
	return NewOn(t, testpg.Pool(t), embedder, namespace)
}

// Pool returns a fresh database for tests that open several stores on one
// database -- namespace isolation, for instance -- through NewOn.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return testpg.Pool(t)
}

// NewOn opens a store on pool, laying the schema down and recording the
// default user on first use. Repeated calls on one pool share the database
// and differ only by namespace.
func NewOn(t testing.TB, pool *pgxpool.Pool, embedder embedding.Embedder, namespace string) Store {
	t.Helper()
	ctx := context.Background()
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
