package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore/pgstore"
)

// newTokenStore returns a fresh TokenStore with the api_tokens table cleaned.
// Skips if MEMSTORE_TEST_PG is unset.
func newTokenStore(t *testing.T) *pgstore.TokenStore {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	pool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)

	ts, err := pgstore.NewTokenStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return ts
}

func TestTokenStore_IssueAndVerify(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	tok, err := ts.Issue(ctx, "matthew-laptop", pgstore.IssueOpts{Scopes: []string{"read", "write"}})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !pgstore.IsMemstoreToken(tok) {
		t.Errorf("token missing prefix: %q", tok)
	}

	res, err := ts.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Name != "matthew-laptop" {
		t.Errorf("Name = %q", res.Name)
	}
	if len(res.Scopes) != 2 || res.Scopes[0] != "read" {
		t.Errorf("Scopes = %v", res.Scopes)
	}
}

func TestTokenStore_Verify_Invalid(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	if _, err := ts.Verify(ctx, ""); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("empty token: got %v, want ErrTokenInvalid", err)
	}
	if _, err := ts.Verify(ctx, "mst_doesnotexist"); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("unknown token: got %v, want ErrTokenInvalid", err)
	}
}

func TestTokenStore_Revoke(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	tok, err := ts.Issue(ctx, "alice-laptop", pgstore.IssueOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Verify(ctx, tok); err != nil {
		t.Fatalf("Verify before revoke: %v", err)
	}

	n, err := ts.Revoke(ctx, "alice-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Revoke returned %d, want 1", n)
	}
	if _, err := ts.Verify(ctx, tok); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("Verify after revoke: got %v, want ErrTokenInvalid", err)
	}
}

func TestTokenStore_Rotate(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	old, err := ts.Issue(ctx, "matthew-workstation", pgstore.IssueOpts{Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	newTok, err := ts.Rotate(ctx, "matthew-workstation")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if old == newTok {
		t.Fatal("Rotate returned the same token")
	}

	// Old token rejected.
	if _, err := ts.Verify(ctx, old); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("old token after rotate: got %v", err)
	}
	// New token works and preserves scopes.
	res, err := ts.Verify(ctx, newTok)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scopes) != 1 || res.Scopes[0] != "read" {
		t.Errorf("Scopes after rotate = %v", res.Scopes)
	}
}

func TestTokenStore_Expiry(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	tok, err := ts.Issue(ctx, "ephemeral", pgstore.IssueOpts{Expires: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Immediately valid.
	if _, err := ts.Verify(ctx, tok); err != nil {
		t.Fatalf("Verify immediately: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := ts.Verify(ctx, tok); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("Verify after expiry: got %v, want ErrTokenInvalid", err)
	}
}

func TestTokenStore_EnsureLegacyToken(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	// First call inserts.
	added, err := ts.EnsureLegacyToken(ctx, "old-shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Error("first call should report inserted=true")
	}

	// Verify the legacy key works as a token.
	res, err := ts.Verify(ctx, "old-shared-secret")
	if err != nil {
		t.Fatalf("Verify legacy: %v", err)
	}
	if res.Name != "legacy" {
		t.Errorf("Name = %q, want legacy", res.Name)
	}

	// Second call is a no-op.
	added, err = ts.EnsureLegacyToken(ctx, "old-shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("second call should report inserted=false")
	}

	// Empty key never inserts.
	added, err = ts.EnsureLegacyToken(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("empty key should not insert")
	}
}

func TestTokenStore_List(t *testing.T) {
	ts := newTokenStore(t)
	ctx := context.Background()

	if _, err := ts.Issue(ctx, "a", pgstore.IssueOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Issue(ctx, "b", pgstore.IssueOpts{Scopes: []string{"read"}}); err != nil {
		t.Fatal(err)
	}

	infos, err := ts.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("List = %d items, want 2", len(infos))
	}
	if infos[0].Name != "a" || infos[1].Name != "b" {
		t.Errorf("List order = [%s, %s], want [a, b]", infos[0].Name, infos[1].Name)
	}

	// Revoked tokens drop out of List.
	if _, err := ts.Revoke(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	infos, err = ts.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "b" {
		t.Errorf("after revoke: %v", infos)
	}
}
