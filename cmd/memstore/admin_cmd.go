package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore/pgstore"
)

// runAdmin dispatches `memstore admin <subcommand>`. All admin commands
// connect directly to PostgreSQL -- they're intended to be run on the daemon
// host, not against the HTTP API.
func runAdmin(args []string) {
	if len(args) == 0 {
		printAdminUsage(os.Stderr)
		os.Exit(1)
	}
	switch args[0] {
	case "tier3-init":
		runTier3Init(args[1:], os.Stdout)
	case "issue-token":
		runIssueToken(args[1:], os.Stdout)
	case "list-tokens":
		runListTokens(args[1:], os.Stdout)
	case "revoke-token":
		runRevokeToken(args[1:], os.Stdout)
	case "rotate-token":
		runRotateToken(args[1:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "admin: unknown subcommand %q\n", args[0])
		printAdminUsage(os.Stderr)
		os.Exit(1)
	}
}

func printAdminUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: memstore admin <subcommand> [flags]

Subcommands:
  tier3-init              Seed the identity schema for a namespace (creates default user, records meta).
  issue-token <name>      Mint a new bearer token. Prints the token ONCE; not retrievable later.
  list-tokens             List all active tokens (name, scopes, created, last used). Token values are not stored.
  revoke-token <name>     Revoke all active tokens with the given name.
  rotate-token <name>     Issue a new token preserving name + scopes; revoke the old one.

All admin commands connect directly to PostgreSQL. Set --pg or MEMSTORE_PG.`)
}

func openPool(pgFlag string) (*pgxpool.Pool, func(), error) {
	dsn := pgFlag
	if dsn == "" {
		dsn = cliConfig.PG
	}
	if dsn == "" {
		return nil, nil, fmt.Errorf("admin: PostgreSQL DSN required (--pg or MEMSTORE_PG)")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	return pool, func() { pool.Close() }, nil
}

func openTokenStore(pgFlag string) (*pgstore.TokenStore, func(), error) {
	pool, closePool, err := openPool(pgFlag)
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	ts, err := pgstore.NewTokenStore(ctx, pool)
	if err != nil {
		closePool()
		return nil, nil, err
	}
	return ts, closePool, nil
}

// --- tier3-init ---

func runTier3Init(args []string, out io.Writer) {
	fs := flag.NewFlagSet("tier3-init", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG / config)")
	defaultUser := fs.String("default-user", "", "Name to seed as the default user (required)")
	namespace := fs.String("namespace", "", "Namespace to initialize (default: empty string for single-tenant)")
	fs.Parse(args)

	if *defaultUser == "" {
		fmt.Fprintln(os.Stderr, "tier3-init: --default-user is required")
		os.Exit(1)
	}

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	if err := pgstore.InitIdentity(context.Background(), pool, *namespace, *defaultUser); err != nil {
		fail(err)
	}
	fmt.Fprintf(out, "Identity initialized: namespace=%q default-user=%q\n", *namespace, *defaultUser)
}

// --- issue-token ---

func runIssueToken(args []string, out io.Writer) {
	fs := flag.NewFlagSet("issue-token", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG / config)")
	scopes := fs.String("scopes", "", "comma-separated scopes (e.g. read,write,admin)")
	expires := fs.Duration("expires", 0, "token lifetime, e.g. 90d, 720h. 0 = no expiry")
	userName := fs.String("user", "", "user name to bind the token to (required; must exist in memstore_users)")
	namespace := fs.String("namespace", "", "namespace to look up the user in (default: empty string for single-tenant)")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "issue-token: expected exactly one positional argument <name> (format: <user>@<host>)")
		os.Exit(1)
	}
	name := fs.Arg(0)

	if *userName == "" {
		fmt.Fprintln(os.Stderr, "issue-token: --user is required")
		os.Exit(1)
	}

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	ctx := context.Background()

	// Resolve user_id from memstore_users.
	var userID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM memstore_users WHERE namespace = $1 AND name = $2`,
		*namespace, *userName,
	).Scan(&userID); err != nil {
		fail(fmt.Errorf("user %q not found in namespace %q: %w\nRun 'memstore admin tier3-init --default-user %s' to create it", *userName, *namespace, err, *userName))
	}

	ts, closeStore, err := openTokenStore(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closeStore()

	tok, err := ts.Issue(ctx, name, pgstore.IssueOpts{
		UserID:  userID,
		Scopes:  splitCSV(*scopes),
		Expires: *expires,
	})
	if err != nil {
		fail(err)
	}

	fmt.Fprintln(out, "Token issued. Capture it now -- it cannot be retrieved later.")
	fmt.Fprintf(out, "  name:  %s\n", name)
	fmt.Fprintf(out, "  user:  %s\n", *userName)
	if *scopes != "" {
		fmt.Fprintf(out, "  scopes: %s\n", *scopes)
	}
	if *expires > 0 {
		fmt.Fprintf(out, "  expires: %s (%s)\n", time.Now().Add(*expires).Format(time.RFC3339), *expires)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, tok)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Add it to the holder's config:\n  api_key = %q\n", tok)
}

// --- list-tokens ---

func runListTokens(args []string, out io.Writer) {
	fs := flag.NewFlagSet("list-tokens", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN")
	fs.Parse(args)

	ts, closeStore, err := openTokenStore(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closeStore()

	infos, err := ts.List(context.Background())
	if err != nil {
		fail(err)
	}
	if len(infos) == 0 {
		fmt.Fprintln(out, "No active tokens.")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCOPES\tCREATED\tLAST USED\tEXPIRES")
	for _, t := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.Name,
			strings.Join(t.Scopes, ","),
			t.CreatedAt.Format("2006-01-02"),
			fmtNullableTime(t.LastUsedAt, "never"),
			fmtNullableTime(t.ExpiresAt, "never"),
		)
	}
	tw.Flush()
}

// --- revoke-token ---

func runRevokeToken(args []string, out io.Writer) {
	fs := flag.NewFlagSet("revoke-token", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "revoke-token: expected exactly one positional argument <name>")
		os.Exit(1)
	}
	name := fs.Arg(0)

	ts, closeStore, err := openTokenStore(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closeStore()

	n, err := ts.Revoke(context.Background(), name)
	if err != nil {
		fail(err)
	}
	if n == 0 {
		fmt.Fprintf(out, "No active tokens named %q.\n", name)
		os.Exit(1)
	}
	fmt.Fprintf(out, "Revoked %d token(s) named %q.\n", n, name)
}

// --- rotate-token ---

func runRotateToken(args []string, out io.Writer) {
	fs := flag.NewFlagSet("rotate-token", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "rotate-token: expected exactly one positional argument <name>")
		os.Exit(1)
	}
	name := fs.Arg(0)

	ts, closeStore, err := openTokenStore(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closeStore()

	tok, err := ts.Rotate(context.Background(), name)
	if err != nil {
		fail(err)
	}

	fmt.Fprintln(out, "Token rotated. Old token revoked, new one printed below.")
	fmt.Fprintf(out, "  name: %s\n\n%s\n\n", name, tok)
	fmt.Fprintf(out, "Update the holder's config:\n  api_key = %q\n", tok)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fmtNullableTime(t *time.Time, zero string) string {
	if t == nil {
		return zero
	}
	return t.Format("2006-01-02")
}
