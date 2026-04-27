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
// connect directly to PostgreSQL — they're intended to be run on the daemon
// host, not against the HTTP API.
func runAdmin(args []string) {
	if len(args) == 0 {
		printAdminUsage(os.Stderr)
		os.Exit(1)
	}
	switch args[0] {
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
  issue-token <name>      Mint a new bearer token. Prints the token ONCE; not retrievable later.
  list-tokens             List all active tokens (name, scopes, created, last used). Token values are not stored.
  revoke-token <name>     Revoke all active tokens with the given name.
  rotate-token <name>     Issue a new token preserving name + scopes; revoke the old one.

All admin commands connect directly to PostgreSQL. Set --pg or MEMSTORE_PG.`)
}

func openTokenStore(pgFlag string) (*pgstore.TokenStore, func(), error) {
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
	ts, err := pgstore.NewTokenStore(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return ts, func() { pool.Close() }, nil
}

// --- issue-token ---

func runIssueToken(args []string, out io.Writer) {
	fs := flag.NewFlagSet("issue-token", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG / config)")
	scopes := fs.String("scopes", "", "comma-separated scopes (e.g. read,write,admin)")
	expires := fs.Duration("expires", 0, "token lifetime, e.g. 90d, 720h. 0 = no expiry")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "issue-token: expected exactly one positional argument <name>")
		os.Exit(1)
	}
	name := fs.Arg(0)

	ts, closeStore, err := openTokenStore(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closeStore()

	tok, err := ts.Issue(context.Background(), name, pgstore.IssueOpts{
		Scopes:  splitCSV(*scopes),
		Expires: *expires,
	})
	if err != nil {
		fail(err)
	}

	fmt.Fprintln(out, "Token issued. Capture it now — it cannot be retrieved later.")
	fmt.Fprintf(out, "  name:  %s\n", name)
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
