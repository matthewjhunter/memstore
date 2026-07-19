package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
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
	case "user-add":
		runUserAdd(args[1:], os.Stdout)
	case "list-users":
		runListUsers(args[1:], os.Stdout)
	case "disable-user":
		runDisableUser(args[1:], os.Stdout)
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
  user-add <name>         Create a user in the namespace (idempotent). Prints the user id.
  list-users              List users in the namespace (name, id).
  disable-user <name>     Revoke all of a user's tokens. With no active token the user cannot authenticate.
  issue-token <name>      Mint a new bearer token. Prints the token ONCE; not retrievable later.
  list-tokens             List all active tokens (name, scopes, created, last used). Token values are not stored.
  revoke-token <name>     Revoke all active tokens with the given name.
  rotate-token <name>     Issue a new token preserving name + scopes; revoke the old one.

Flags may appear before or after the positional argument.

All admin commands connect directly to PostgreSQL, so they must run on the
daemon host (or anywhere with a route to it). Set --pg or MEMSTORE_PG.

Commands act on the namespace from --namespace, defaulting to the one the
daemon uses (config file / MEMSTORE_NAMESPACE, built-in default "default").`)
}

// parseAdminArgs parses an admin subcommand's flags and returns its positional
// arguments, accepting flags before, after, or interleaved with them.
//
// A bare fs.Parse cannot: Go's flag package stops at the first non-flag
// argument, so `issue-token matthew@kraken --user matthew` silently drops
// --user and the command then complains about the positional argument -- the
// one thing the operator got right. Anything after a bare "--" stays
// positional.
func parseAdminArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			tail = args[i+1:]
			args = args[:i]
			break
		}
	}

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		// rest[0] is a non-flag argument: set it aside and keep parsing what
		// follows, which flag would otherwise have abandoned.
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	return append(positional, tail...), nil
}

// defaultAdminNamespace is the namespace admin commands target when the
// operator does not pass --namespace: the same one the daemon uses. It is
// never the empty string -- admin defaulting to "" while the daemon wrote its
// rows under "default" made list-users report an empty database on a live
// deployment.
func defaultAdminNamespace() string {
	if cliConfig.Namespace != "" {
		return cliConfig.Namespace
	}
	return memstore.DefaultConfig().Namespace
}

const namespaceFlagUsage = "namespace to act on (defaults to the daemon's namespace: config file / MEMSTORE_NAMESPACE)"

// exactlyOneArg enforces a single positional argument, naming what was expected.
func exactlyOneArg(cmd string, positional []string, what string) string {
	if len(positional) != 1 {
		fmt.Fprintf(os.Stderr, "%s: expected exactly one positional argument %s, got %d\n", cmd, what, len(positional))
		os.Exit(1)
	}
	return positional[0]
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
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}

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

// --- user-add ---

func runUserAdd(args []string, out io.Writer) {
	fs := flag.NewFlagSet("user-add", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG / config)")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	positional, err := parseAdminArgs(fs, args)
	if err != nil {
		fail(err)
	}
	name := exactlyOneArg("user-add", positional, "<name>")

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	id, err := pgstore.EnsureUser(context.Background(), pool, *namespace, name)
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(out, "User %q ready in namespace %q (id=%d).\n", name, *namespace, id)
}

// --- list-users ---

func runListUsers(args []string, out io.Writer) {
	fs := flag.NewFlagSet("list-users", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	rows, err := pool.Query(context.Background(),
		`SELECT name, id FROM memstore_users WHERE namespace = $1 ORDER BY name`,
		*namespace)
	if err != nil {
		fail(err)
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID")
	n := 0
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			fail(err)
		}
		fmt.Fprintf(tw, "%s\t%d\n", name, id)
		n++
	}
	if err := rows.Err(); err != nil {
		fail(err)
	}
	tw.Flush()
	if n == 0 {
		fmt.Fprintf(out, "No users in namespace %q.\n", *namespace)
		// An empty namespace usually means the wrong --namespace, not an empty
		// database. Say where the users actually are.
		if elsewhere := populatedNamespaces(context.Background(), pool, *namespace); len(elsewhere) > 0 {
			fmt.Fprintf(out, "Users exist in namespace(s) %s -- pass --namespace to list them.\n",
				strings.Join(elsewhere, ", "))
		}
	}
}

// populatedNamespaces returns every namespace other than exclude that holds at
// least one user, quoted for display (a namespace can be the empty string).
func populatedNamespaces(ctx context.Context, pool *pgxpool.Pool, exclude string) []string {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT namespace FROM memstore_users WHERE namespace <> $1 ORDER BY namespace`,
		exclude)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil
		}
		out = append(out, fmt.Sprintf("%q", ns))
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// --- disable-user ---

func runDisableUser(args []string, out io.Writer) {
	fs := flag.NewFlagSet("disable-user", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	positional, err := parseAdminArgs(fs, args)
	if err != nil {
		fail(err)
	}
	name := exactlyOneArg("disable-user", positional, "<name>")

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	ctx := context.Background()
	userID, err := pgstore.LookupUserID(ctx, pool, *namespace, name)
	if err != nil {
		fail(err)
	}

	ts, closeStore, err := openTokenStore(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closeStore()

	n, err := ts.RevokeByUser(ctx, userID)
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(out, "Disabled user %q: revoked %d token(s). The user can no longer authenticate.\n", name, n)
}

// --- issue-token ---

func runIssueToken(args []string, out io.Writer) {
	fs := flag.NewFlagSet("issue-token", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG / config)")
	scopes := fs.String("scopes", "", "comma-separated scopes: read, write, admin, ingest. admin implies read+write; ingest is never implied and must be granted explicitly")
	expires := fs.Duration("expires", 0, "token lifetime, e.g. 90d, 720h. 0 = no expiry")
	userName := fs.String("user", "", "user name to bind the token to (required; must exist in memstore_users)")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	positional, err := parseAdminArgs(fs, args)
	if err != nil {
		fail(err)
	}
	name := exactlyOneArg("issue-token", positional, "<name> (format: <user>@<host>)")

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

	// Resolve user_id from memstore_users. Only suggest creating the user when
	// it exists nowhere: if it merely lives in another namespace, user-add
	// would mint a duplicate instead of fixing the mismatch, so let
	// LookupUserID's own message (which names that namespace) stand.
	userID, err := pgstore.LookupUserID(ctx, pool, *namespace, *userName)
	if errors.Is(err, pgstore.ErrUserNotFound) {
		fail(fmt.Errorf("%w\nRun 'memstore admin user-add %s' to create it", err, *userName))
	}
	if err != nil {
		fail(err)
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
	fmt.Fprintf(out, "  user:  %s (namespace %q)\n", *userName, *namespace)
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
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}

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
	positional, err := parseAdminArgs(fs, args)
	if err != nil {
		fail(err)
	}
	name := exactlyOneArg("revoke-token", positional, "<name>")

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
	positional, err := parseAdminArgs(fs, args)
	if err != nil {
		fail(err)
	}
	name := exactlyOneArg("rotate-token", positional, "<name>")

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
