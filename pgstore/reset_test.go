package pgstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// namedEmbedder is a mockEmbedder that reports a chosen model name, so a
// test can open the same database under two different models.
type namedEmbedder struct {
	mockEmbedder
	name string
}

func (e *namedEmbedder) Model() string { return e.name }

func (e *namedEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: e.name, Dim: e.dim}
}

// Switching embedding models is refused at open, because the stored vectors
// would be incomparable with new ones. ResetEmbeddings is the deliberate
// operator step that clears them and forgets the old fingerprint, after which
// the open under the new model succeeds and the backfill re-embeds.
func TestResetEmbeddings_AllowsModelChange(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// The pgstore tests share one database; start from nothing, as the
	// package helpers do, so another test's fingerprint cannot leak in.
	for _, tbl := range []string{"api_tokens", "memstore_links", "memstore_facts", "memstore_meta", "memstore_version", "memstore_document_chunks", "memstore_documents", "memstore_users"} {
		pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}

	open := func(model string) (*pgstore.PostgresStore, error) {
		return pgstore.New(ctx, pool, &namedEmbedder{mockEmbedder{dim: 4}, model}, "test", 4, 16)
	}
	if _, err := open("alpha"); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("schema init: %v", err)
	}
	if err := pgstore.InitIdentity(ctx, pool, "test", "testuser"); err != nil {
		t.Fatal(err)
	}
	alpha, err := open("alpha")
	if err != nil {
		t.Fatalf("open under alpha: %v", err)
	}
	id, err := alpha.Insert(ctx, memstore.Fact{Content: "embedded under alpha", Subject: "s", Category: "c"})
	if err != nil {
		t.Fatal(err)
	}
	// Insert queues embedding; store the vector the way the embed queue would.
	if err := alpha.SetEmbedding(ctx, id, []float32{1, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var embedded int
	if err := pool.QueryRow(ctx, `SELECT count(embedding) FROM memstore_facts`).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if embedded != 1 {
		t.Fatalf("expected 1 embedded fact under alpha, got %d", embedded)
	}

	if _, err := open("beta"); !errors.Is(err, embedding.ErrModelChanged) {
		t.Fatalf("open under beta before reset: want ErrModelChanged, got %v", err)
	}

	cleared, err := pgstore.ResetEmbeddings(ctx, pool)
	if err != nil {
		t.Fatalf("ResetEmbeddings: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("expected 1 vector cleared, got %d", cleared)
	}

	if _, err := open("beta"); err != nil {
		t.Fatalf("open under beta after reset: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(embedding) FROM memstore_facts`).Scan(&embedded); err != nil {
		t.Fatal(err)
	}
	if embedded != 0 {
		t.Fatalf("expected no vectors after reset, got %d", embedded)
	}
	var model string
	if err := pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "beta" {
		t.Fatalf("stored model after reopen: want beta, got %q", model)
	}
}
