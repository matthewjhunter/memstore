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

	// Drop all session tables so each test starts clean. api_tokens is included
	// so default-user inference can't read a token left behind by another
	// package on the shared CI Postgres -- keeps the bootstrap order-independent.
	for _, tbl := range []string{
		"context_feedback",
		"context_injections",
		"context_hints",
		"session_hooks",
		"session_turns",
		"api_tokens",
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

// TestSessionMigrate_DataWithoutDefaultUser verifies the fail-loud gate:
// when session rows exist but no default_user is recorded, NewSessionStore
// returns the tier3-init error AND leaves user_id nullable (the failure is at
// resolution, not a masked half-migrate that silently sets NOT NULL).
func TestSessionMigrate_DataWithoutDefaultUser(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	// Clean both layers. api_tokens is dropped too: default-user inference
	// reads it, so a token left behind by another package on the shared CI
	// Postgres would let the migration infer a default user and mask the
	// tier3-init failure this test asserts. Dropping it keeps the
	// "no way to infer a default user" precondition order-independent.
	for _, tbl := range []string{
		"context_feedback", "context_injections", "context_hints",
		"session_hooks", "session_turns",
		"api_tokens",
		"memstore_links", "memstore_facts", "memstore_meta",
		"memstore_version", "memstore_users",
	} {
		pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
	}

	// Build the facts-layer schema so memstore_meta + memstore_users exist,
	// but deliberately do NOT seed default_user (no InitIdentity call). The
	// first pgstore.New migrates the schema and fails at user resolution;
	// that failure is expected and benign here.
	embedder := &mockEmbedder{dim: 4}
	if _, err := pgstore.New(ctx, pool, embedder, "test", 4, 0); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("facts schema init: %v", err)
	}

	// Pre-create session_turns with a NULLABLE user_id and seed one row, so the
	// migration's fail-loud check has data present but no default user to stamp.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE session_turns (
			id         BIGSERIAL PRIMARY KEY,
			session_id TEXT NOT NULL,
			uuid       TEXT NOT NULL,
			turn_index INT NOT NULL,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			cwd        TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			user_id    BIGINT,
			UNIQUE(session_id, uuid)
		)`); err != nil {
		t.Fatalf("creating pre-migration session_turns: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_turns(session_id, uuid, turn_index, role, content)
		VALUES ('orphan-sid', 'orphan-uuid', 0, 'user', 'pre-existing row')
	`); err != nil {
		t.Fatalf("seeding orphan turn: %v", err)
	}

	// NewSessionStore must fail loudly with the tier3-init instruction.
	_, err = pgstore.NewSessionStore(ctx, pool)
	if err == nil {
		t.Fatal("NewSessionStore should have failed (session data, no default user)")
		return // SA5011: newer staticcheck misses that Fatal terminates
	}
	if !strings.Contains(err.Error(), "tier3-init") {
		t.Errorf("expected tier3-init error, got: %v", err)
	}

	// The column must remain nullable -- the failure was at resolution, not a
	// masked half-migrate that silently applied SET NOT NULL.
	var isNullable string
	if qerr := pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_name = 'session_turns' AND column_name = 'user_id'
	`).Scan(&isNullable); qerr != nil {
		t.Fatalf("querying user_id nullability: %v", qerr)
	}
	if isNullable != "YES" {
		t.Errorf("session_turns.user_id was set NOT NULL despite failed resolution (half-migrate); is_nullable=%q", isNullable)
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

// TestSessionMigrate_Idempotent verifies that running the session-store
// migration a second time (i.e. every daemon restart after the first) is a
// clean no-op. Regression for a production bug where the widened-unique
// ADD CONSTRAINT blocks caught only duplicate_object (42710); adding a UNIQUE
// constraint creates a backing index, so a re-run raised duplicate_table
// (42P07), which aborted Migrate and disabled the session store + extract
// queue on every restart. The test also reproduces the prod state where the
// old narrow constraint uq_context_feedback_ref_session was left behind, and
// confirms the re-run cleans it up.
func TestSessionMigrate_Idempotent(t *testing.T) {
	_, pool := newTestSessionStore(t)
	ctx := context.Background()

	// Simulate the production database: the old narrow constraint survived a
	// prior half-completed migration and coexists with the wide one.
	if _, err := pool.Exec(ctx, `
		ALTER TABLE context_feedback
		ADD CONSTRAINT uq_context_feedback_ref_session
		UNIQUE (ref_id, ref_type, session_id)`); err != nil {
		t.Fatalf("seeding leftover narrow constraint: %v", err)
	}

	// A second (and third) migrate must succeed -- this is what a daemon
	// restart does. Before the fix, the first re-run errored with 42P07.
	for i := range 2 {
		if _, err := pgstore.NewSessionStore(ctx, pool); err != nil {
			t.Fatalf("re-run %d of NewSessionStore failed (migration not idempotent): %v", i+1, err)
		}
	}

	// The leftover narrow constraint must be gone, leaving only the wide one.
	var narrow, wide int
	if err := pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE conname = 'uq_context_feedback_ref_session'),
			count(*) FILTER (WHERE conname = 'uq_context_feedback_user_ref_session')
		FROM pg_constraint
		WHERE conrelid = 'context_feedback'::regclass AND contype = 'u'`).Scan(&narrow, &wide); err != nil {
		t.Fatalf("inspecting context_feedback constraints: %v", err)
	}
	if narrow != 0 {
		t.Errorf("old narrow constraint uq_context_feedback_ref_session still present after re-migrate")
	}
	if wide != 1 {
		t.Errorf("wide constraint uq_context_feedback_user_ref_session count = %d, want 1", wide)
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

			// Clean session tables. api_tokens is included so default-user
			// inference can't read a token left behind by another package on
			// the shared CI Postgres -- keeps the rebuild order-independent.
			for _, tbl := range []string{
				"context_feedback",
				"context_injections",
				"context_hints",
				"session_hooks",
				"session_turns",
				"api_tokens",
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
