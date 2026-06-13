package pgstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/conformance"
	"github.com/matthewjhunter/memstore/pgstore"
)

// newTestSessionStore creates a SessionStore against the test database, with
// the facts-layer schema migrated first (SessionStore depends on
// memstore_users + memstore_meta from that migration).
func newTestSessionStore(t *testing.T) (*pgstore.SessionStore, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	// Drop all session tables so each test starts clean.
	for _, tbl := range []string{
		"context_feedback",
		"context_injections",
		"context_hints",
		"session_hooks",
		"session_turns",
	} {
		pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}

	// Ensure the facts-layer schema exists (memstore_users, memstore_meta).
	// Use the same bootstrap pattern as newTestStoreNS.
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
	pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)

	embedder := &mockEmbedder{dim: 4}
	if _, err := pgstore.New(ctx, pool, embedder, "test", 4, 0); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("facts schema init: %v", err)
	}
	if err := pgstore.InitIdentity(ctx, pool, "test", "testuser"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
	if _, err := pgstore.New(ctx, pool, embedder, "test", 4, 0); err != nil {
		t.Fatalf("second pgstore.New: %v", err)
	}

	ss, err := pgstore.NewSessionStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return ss, pool
}

// TestSessionForUser_InvalidID checks that ForUser rejects non-positive IDs.
func TestSessionForUser_InvalidID(t *testing.T) {
	ss, _ := newTestSessionStore(t)
	for _, id := range []int64{0, -1, -999} {
		if _, err := ss.ForUser(id); err == nil {
			t.Errorf("ForUser(%d) should have returned an error", id)
		}
	}
}

// TestSessionMigrate_UserIDColumns verifies that after NewSessionStore all five
// session tables have a user_id column that is NOT NULL and has the FK to
// memstore_users.
func TestSessionMigrate_UserIDColumns(t *testing.T) {
	_, pool := newTestSessionStore(t)
	ctx := context.Background()

	tables := []string{
		"session_turns",
		"session_hooks",
		"context_hints",
		"context_injections",
		"context_feedback",
	}
	for _, tbl := range tables {
		t.Run(tbl, func(t *testing.T) {
			// Check column exists and is NOT NULL.
			var isNullable string
			err := pool.QueryRow(ctx, `
				SELECT is_nullable FROM information_schema.columns
				WHERE table_name = $1 AND column_name = 'user_id'
			`, tbl).Scan(&isNullable)
			if err != nil {
				t.Fatalf("querying user_id column for %s: %v", tbl, err)
			}
			if isNullable != "NO" {
				t.Errorf("%s.user_id is nullable (expected NOT NULL)", tbl)
			}

			// Check FK exists.
			var fkCount int
			err = pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM information_schema.referential_constraints rc
				JOIN information_schema.key_column_usage kcu
				  ON kcu.constraint_name = rc.constraint_name
				WHERE kcu.table_name = $1 AND kcu.column_name = 'user_id'
			`, tbl).Scan(&fkCount)
			if err != nil {
				t.Fatalf("querying FK for %s: %v", tbl, err)
			}
			if fkCount == 0 {
				t.Errorf("%s.user_id has no FK to memstore_users", tbl)
			}
		})
	}
}

// TestSessionStore_DefaultScope exercises basic write+read through the
// default-scoped SessionStore (the single-user daemon path).
func TestSessionStore_DefaultScope(t *testing.T) {
	ss, _ := newTestSessionStore(t)
	ctx := context.Background()

	sid := "default-scope-session"
	turns := []memstore.SessionTurn{
		{
			SessionID: sid,
			UUID:      "uuid-default-1",
			TurnIndex: 0,
			Role:      "user",
			Content:   "hello default",
			CWD:       "/tmp",
			CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
		},
	}
	if err := ss.SaveTurns(ctx, sid, turns); err != nil {
		t.Fatalf("SaveTurns: %v", err)
	}

	got, err := ss.GetSessionTurns(ctx, sid)
	if err != nil {
		t.Fatalf("GetSessionTurns: %v", err)
	}
	if len(got) != 1 || got[0].Content != "hello default" {
		t.Errorf("unexpected turns: %v", got)
	}
}

// TestSessionConformance_SessionIsolation wires the conformance session
// isolation battery against pgstore, using two ForUser-scoped stores.
func TestSessionConformance_SessionIsolation(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)

	conformance.Run(t, conformance.Options{
		// NewStore is required by conformance.Run; use a minimal stub that
		// satisfies the interface (the session isolation subtests are all we care
		// about here -- the full facts battery runs in store_test.go).
		NewStore: func(t *testing.T) memstore.Store {
			t.Helper()
			return newTestStore(t)
		},
		NewTwoUserSessionStores: func(t *testing.T) (memstore.SessionStore, memstore.SessionStore) {
			t.Helper()
			pool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatalf("connecting to postgres: %v", err)
			}
			t.Cleanup(pool.Close)

			// Clean session tables.
			for _, tbl := range []string{
				"context_feedback",
				"context_injections",
				"context_hints",
				"session_hooks",
				"session_turns",
			} {
				pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
			}

			// Rebuild facts schema with two users.
			pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_links CASCADE`)
			pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_facts CASCADE`)
			pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_meta CASCADE`)
			pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_version CASCADE`)
			pool.Exec(ctx, `DROP TABLE IF EXISTS memstore_users CASCADE`)

			embedder := &mockEmbedder{dim: 4}
			if _, err := pgstore.New(ctx, pool, embedder, "test", 4, 0); err != nil && !strings.Contains(err.Error(), "tier3-init") {
				t.Fatalf("facts schema init: %v", err)
			}
			if err := pgstore.InitIdentity(ctx, pool, "test", "user-a"); err != nil {
				t.Fatalf("InitIdentity user-a: %v", err)
			}
			// user-b is a second user in the same namespace.
			uidB, err := pgstore.EnsureUser(ctx, pool, "test", "user-b")
			if err != nil {
				t.Fatalf("EnsureUser user-b: %v", err)
			}

			ss, err := pgstore.NewSessionStore(ctx, pool)
			if err != nil {
				t.Fatalf("NewSessionStore: %v", err)
			}

			storeA, err := ss.ForUser(ss.UserID())
			if err != nil {
				t.Fatalf("ForUser(user-a): %v", err)
			}
			storeB, err := ss.ForUser(uidB)
			if err != nil {
				t.Fatalf("ForUser(user-b): %v", err)
			}
			return storeA, storeB
		},
	})
}
