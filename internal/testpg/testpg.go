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
	"strings"
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

// connString renders a key=value DSN for the new database. Built by hand
// rather than via ConnString(), which returns the string originally parsed and
// would ignore the database swap. sslmode follows whether the admin config
// negotiated TLS.
func connString(cfg *pgx.ConnConfig) string {
	sslmode := "disable"
	if cfg.TLSConfig != nil {
		sslmode = "require"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "host=%s port=%d dbname=%s sslmode=%s", cfg.Host, cfg.Port, cfg.Database, sslmode)
	if cfg.User != "" {
		fmt.Fprintf(&b, " user=%s", cfg.User)
	}
	if cfg.Password != "" {
		fmt.Fprintf(&b, " password=%s", cfg.Password)
	}
	return b.String()
}
