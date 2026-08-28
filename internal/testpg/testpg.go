// Package testpg hands tests a private PostgreSQL database each.
//
// MEMSTORE_TEST_PG names an admin DSN on a server with pgvector. Every call
// creates a database of its own there and drops it when the test ends, so
// migrations never race across tests (a UNIQUE index creation colliding on its
// name under `go test ./...` parallelism was the original reason for this) and
// no test sees another's default user, tokens, or facts. Without the variable
// the test is skipped, which is what a laptop with no Postgres should see; CI
// sets it.
package testpg

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const envVar = "MEMSTORE_TEST_PG"

var counter atomic.Int64

// Available reports whether MEMSTORE_TEST_PG is set, for callers that want to
// decide something before skipping.
func Available() bool { return os.Getenv(envVar) != "" }

// DSN creates a fresh database and returns a DSN targeting it. The database is
// dropped with FORCE on cleanup, best effort.
func DSN(t testing.TB) string {
	t.Helper()
	cfg := adminConfig(t)
	name := create(t, cfg)
	cfg.Database = name
	return connString(cfg)
}

// Pool creates a fresh database and returns a pool connected to it. The pool
// is closed and the database dropped on cleanup.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	cfg := adminConfig(t)
	name := create(t, cfg)

	poolCfg, err := pgxpool.ParseConfig(os.Getenv(envVar))
	if err != nil {
		t.Fatalf("testpg: parse pool config: %v", err)
	}
	poolCfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("testpg: connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func adminConfig(t testing.TB) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv(envVar)
	if dsn == "" {
		t.Skipf("%s not set; skipping (requires PostgreSQL with pgvector)", envVar)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("testpg: parse %s: %v", envVar, err)
	}
	return cfg
}

// create makes a uniquely named database from an admin connection and
// registers its removal. The identifier is process-derived, never user input,
// which is why it is formatted straight into the DDL: CREATE DATABASE cannot
// take a parameter.
func create(t testing.TB, admin *pgx.ConnConfig) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("memstore_test_%d_%d", os.Getpid(), counter.Add(1))

	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		t.Fatalf("testpg: connect to admin database: %v", err)
	}
	_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		conn.Close(ctx)
		t.Fatalf("testpg: create database %s: %v", name, err)
	}
	conn.Close(ctx)

	t.Cleanup(func() {
		ctx := context.Background()
		conn, err := pgx.ConnectConfig(ctx, admin)
		if err != nil {
			t.Logf("testpg: cleanup connect to drop %s: %v", name, err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
			t.Logf("testpg: drop %s: %v", name, err)
		}
	})
	return name
}

func connString(cfg *pgx.ConnConfig) string {
	u := fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Host, cfg.Port, cfg.Database)
	if cfg.Password != "" {
		u = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	}
	return u
}
