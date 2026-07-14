package pgstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore/pgstore"
)

// newTokenStore returns a fresh TokenStore with a clean schema including the
// memstore_users and memstore_meta tables that token migration requires.
// Skips if MEMSTORE_TEST_PG is unset.
func newTokenStore(t *testing.T) (*pgstore.TokenStore, int64) {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	// Drop in reverse dependency order.
	pool.Exec(ctx, `DROP TABLE IF EXISTS api_tokens`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)

	// Seed the tables token migration depends on.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS memstore_users (
		id         BIGSERIAL   PRIMARY KEY,
		namespace  TEXT        NOT NULL,
		name       TEXT        NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (namespace, name)
	)`); err != nil {
		t.Fatalf("create memstore_users: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS memstore_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create memstore_meta: %v", err)
	}

	// Insert a default user and record it in meta.
	var defaultUID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO memstore_users (namespace, name) VALUES ('', 'testuser') RETURNING id`,
	).Scan(&defaultUID); err != nil {
		t.Fatalf("insert default user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memstore_meta (key, value) VALUES ('default_user', 'testuser')`,
	); err != nil {
		t.Fatalf("insert default_user meta: %v", err)
	}

	ts, err := pgstore.NewTokenStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	return ts, defaultUID
}

func TestTokenStore_IssueAndVerify(t *testing.T) {
	ts, uid := newTokenStore(t)
	ctx := context.Background()

	tok, err := ts.Issue(ctx, "matthew@laptop", pgstore.IssueOpts{UserID: uid, Scopes: []string{"read", "write"}})
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
	if res.Name != "matthew@laptop" {
		t.Errorf("Name = %q", res.Name)
	}
	if len(res.Scopes) != 2 || res.Scopes[0] != "read" {
		t.Errorf("Scopes = %v", res.Scopes)
	}
	if res.UserID != uid {
		t.Errorf("UserID = %d, want %d", res.UserID, uid)
	}
}

func TestTokenStore_Verify_Invalid(t *testing.T) {
	ts, _ := newTokenStore(t)
	ctx := context.Background()

	if _, err := ts.Verify(ctx, ""); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("empty token: got %v, want ErrTokenInvalid", err)
	}
	if _, err := ts.Verify(ctx, "mst_doesnotexist"); !errors.Is(err, pgstore.ErrTokenInvalid) {
		t.Errorf("unknown token: got %v, want ErrTokenInvalid", err)
	}
}

func TestTokenStore_Revoke(t *testing.T) {
	ts, uid := newTokenStore(t)
	ctx := context.Background()

	tok, err := ts.Issue(ctx, "alice@laptop", pgstore.IssueOpts{UserID: uid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Verify(ctx, tok); err != nil {
		t.Fatalf("Verify before revoke: %v", err)
	}

	n, err := ts.Revoke(ctx, "alice@laptop")
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
	ts, uid := newTokenStore(t)
	ctx := context.Background()

	old, err := ts.Issue(ctx, "matthew@workstation", pgstore.IssueOpts{UserID: uid, Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	newTok, err := ts.Rotate(ctx, "matthew@workstation")
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
	// New token works and preserves scopes and owner.
	res, err := ts.Verify(ctx, newTok)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scopes) != 1 || res.Scopes[0] != "read" {
		t.Errorf("Scopes after rotate = %v", res.Scopes)
	}
	if res.UserID != uid {
		t.Errorf("UserID after rotate = %d, want %d", res.UserID, uid)
	}

	// The rotated row keeps the original user_id (assert via List too).
	infos, err := ts.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range infos {
		if info.Name == "matthew@workstation" && info.RevokedAt == nil {
			if info.UserID != uid {
				t.Errorf("List: rotated token UserID = %d, want %d", info.UserID, uid)
			}
		}
	}
}

func TestTokenStore_Expiry(t *testing.T) {
	ts, uid := newTokenStore(t)
	ctx := context.Background()

	tok, err := ts.Issue(ctx, "ephemeral@test", pgstore.IssueOpts{UserID: uid, Expires: 50 * time.Millisecond})
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
	ts, _ := newTokenStore(t)
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
	ts, uid := newTokenStore(t)
	ctx := context.Background()

	if _, err := ts.Issue(ctx, "a@host", pgstore.IssueOpts{UserID: uid}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Issue(ctx, "b@host", pgstore.IssueOpts{UserID: uid, Scopes: []string{"read"}}); err != nil {
		t.Fatal(err)
	}

	infos, err := ts.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("List = %d items, want 2", len(infos))
	}
	if infos[0].Name != "a@host" || infos[1].Name != "b@host" {
		t.Errorf("List order = [%s, %s], want [a@host, b@host]", infos[0].Name, infos[1].Name)
	}
	if infos[0].UserID != uid {
		t.Errorf("infos[0].UserID = %d, want %d", infos[0].UserID, uid)
	}

	// Revoked tokens drop out of List.
	if _, err := ts.Revoke(ctx, "a@host"); err != nil {
		t.Fatal(err)
	}
	infos, err = ts.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "b@host" {
		t.Errorf("after revoke: %v", infos)
	}
}

func TestTokenStore_RevokeByUser(t *testing.T) {
	ts, uid := newTokenStore(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	// A second user, to prove RevokeByUser is scoped by user_id.
	otherUID, err := pgstore.EnsureUser(ctx, pool, "", "other")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	// Two active tokens for the default user, one for the other user.
	if _, err := ts.Issue(ctx, "testuser@a", pgstore.IssueOpts{UserID: uid}); err != nil {
		t.Fatalf("issue testuser@a: %v", err)
	}
	if _, err := ts.Issue(ctx, "testuser@b", pgstore.IssueOpts{UserID: uid}); err != nil {
		t.Fatalf("issue testuser@b: %v", err)
	}
	otherTok, err := ts.Issue(ctx, "other@c", pgstore.IssueOpts{UserID: otherUID})
	if err != nil {
		t.Fatalf("issue other@c: %v", err)
	}

	n, err := ts.RevokeByUser(ctx, uid)
	if err != nil {
		t.Fatalf("RevokeByUser: %v", err)
	}
	if n != 2 {
		t.Errorf("RevokeByUser revoked %d, want 2", n)
	}

	// The other user's token is untouched.
	if _, err := ts.Verify(ctx, otherTok); err != nil {
		t.Errorf("other user's token should still verify after RevokeByUser: %v", err)
	}

	// A second call revokes nothing (already revoked).
	n, err = ts.RevokeByUser(ctx, uid)
	if err != nil {
		t.Fatalf("RevokeByUser (second call): %v", err)
	}
	if n != 0 {
		t.Errorf("second RevokeByUser revoked %d, want 0", n)
	}
}

func TestLookupUserID(t *testing.T) {
	_, uid := newTokenStore(t) // seeds memstore_users with the default "testuser"
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	got, err := pgstore.LookupUserID(ctx, pool, "", "testuser")
	if err != nil {
		t.Fatalf("LookupUserID(existing): %v", err)
	}
	if got != uid {
		t.Errorf("LookupUserID = %d, want %d", got, uid)
	}

	if _, err := pgstore.LookupUserID(ctx, pool, "", "nonexistent"); err == nil {
		t.Error("LookupUserID(missing): expected a not-found error, got nil")
	}
}

// A user that exists under a different namespace must be reported as such.
// Reporting it as a plain not-found sent operators to 'user-add', which would
// create a second user of the same name in the wrong namespace instead of
// surfacing the mismatch.
func TestLookupUserID_wrongNamespace(t *testing.T) {
	newTokenStore(t) // seeds "testuser" in namespace ""
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, err = pgstore.LookupUserID(ctx, pool, "default", "testuser")
	if err == nil {
		t.Fatal("LookupUserID in the wrong namespace: expected an error, got nil")
	}
	if !errors.Is(err, pgstore.ErrUserWrongNamespace) {
		t.Errorf("error = %v, want ErrUserWrongNamespace", err)
	}
	if errors.Is(err, pgstore.ErrUserNotFound) {
		t.Errorf("error = %v, must not also be ErrUserNotFound (that is what sends operators to user-add)", err)
	}
	// The message has to name the namespace the user actually lives in, or the
	// operator has no way to know which --namespace to pass.
	if !strings.Contains(err.Error(), `""`) {
		t.Errorf("error %q does not name the namespace the user was found in", err)
	}

	// A name that exists nowhere is still a plain not-found.
	_, err = pgstore.LookupUserID(ctx, pool, "default", "nobody")
	if !errors.Is(err, pgstore.ErrUserNotFound) {
		t.Errorf("error for a wholly unknown user = %v, want ErrUserNotFound", err)
	}
	if errors.Is(err, pgstore.ErrUserWrongNamespace) {
		t.Errorf("error for a wholly unknown user = %v, must not be ErrUserWrongNamespace", err)
	}
}
