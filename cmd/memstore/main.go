// Command memstore provides CLI access to a memstore database.
//
// Usage:
//
//	memstore export --db path/to/db.sqlite [--output=path]
//	memstore import --db path/to/db.sqlite [--skip-duplicates] file.json
//	memstore tasks [--surface startup] [--status pending] [--scope claude] [--format text|json]
//	memstore backfill-feedback
//	memstore store --subject <s> --content <c> [--category note] [--kind <k>] [--subsystem <ss>] [--metadata '{}'] [--supersedes id]
//	memstore list [--subject <s>] [--category <c>] [--metadata '{}'] [--format text|json]
//	memstore search --query <q> [--subject <s>] [--category <c>] [--limit 5] [--format text|json]
//	memstore scan [--subject <s>] [--model] [--threat 6] [--top 15] [--format text|json]
//	memstore hook [--remote url] [--transcript path]
//	memstore mcp-headers
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
	_ "modernc.org/sqlite"
)

// cliConfig holds the loaded config, set once in main() and used by all subcommands.
var cliConfig memstore.AppConfig

func main() {
	log.SetFlags(0)
	cliConfig = memstore.LoadConfig()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "export":
		runExport(os.Args[2:])
	case "import":
		runImport(os.Args[2:])
	case "tasks":
		runTasks(os.Args[2:])
	case "store":
		runStore(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "search":
		runSearch(os.Args[2:])
	case "mcp-headers":
		runMCPHeaders(os.Args[2:])
	case "hook":
		runHook(os.Args[2:])
	case "eval-triggers":
		runEvalTriggers(os.Args[2:])
	case "setup":
		runSetup(os.Args[2:])
	case "tls":
		runTLS(os.Args[2:])
	case "admin":
		runAdmin(os.Args[2:])
	case "backfill-feedback":
		runBackfillFeedback(os.Args[2:])
	case "ingest":
		runIngest(os.Args[2:])
	case "scan":
		runScan(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: memstore <command> [flags]

Commands:
  export    Export all facts to JSON
  import    Import facts from a JSON export
  tasks     List tasks (filter by surface, status, scope, project)
  store     Store a new fact
  list      List facts (filter by subject, category, metadata)
  search    FTS search facts by query text
  scan      Screen the corpus for prompt injection and report what would be blocked
  eval-triggers  Evaluate trigger facts against a file path and load context
  hook               Handle a Claude Code Stop hook event (reads the payload on stdin)
  mcp-headers        Print MCP auth headers as JSON (for Claude Code's headersHelper)
  setup              Install hooks, register MCP server, and configure memstore
  tls                Generate a self-signed CA + server cert, or issue client certs
  admin              Manage api_tokens (issue / list / revoke / rotate). Requires --pg.
  backfill-feedback  Auto-rate all historical fact injections (requires remote)
  ingest             Ingest a file or repo tree into the document corpus (requires
                     remote and the dedicated ingest_token credential)`)
}

// openStore opens the daemon the CLI is configured against. There is no
// local mode: every command that reads or writes facts talks to memstored,
// which owns the namespace and the user the token resolves to.
func openStore() (memstore.Store, func(), error) {
	if cliConfig.Remote == "" {
		return nil, nil, fmt.Errorf("no daemon configured: set remote in config.toml or MEMSTORE_REMOTE (run 'memstore setup')")
	}
	client, err := newRemoteClient()
	if err != nil {
		return nil, nil, err
	}
	return client, func() {}, nil
}

// newRemoteClient builds an httpclient.Client against cliConfig.Remote with
// any TLS knobs from the config applied.
func newRemoteClient() (*httpclient.Client, error) {
	return httpclient.NewWithOptions(
		cliConfig.Remote,
		cliConfig.APIKey,
		httpclient.ClientOptionsFromConfig(cliConfig),
	)
}

// openDB opens a SQLite file read-only for `export --db`. It is the last
// piece of the SQLite backend: the export reader stays one release beyond
// the store it reads so a 0.4.x database can still be carried into a daemon.
func openDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("--db is required")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("database not found: %s", path)
	}
	return sql.Open("sqlite", path+"?mode=ro&_pragma=busy_timeout(5000)")
}

func runExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	output := fs.String("output", "", "write to file instead of stdout")
	fs.Parse(args)

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	data, err := memstore.Export(context.Background(), db)
	if err != nil {
		log.Fatalf("export: %v", err)
	}

	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}

	if *output != "" {
		if err := os.WriteFile(*output, buf, 0600); err != nil {
			log.Fatalf("write: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Exported %d facts and %d links to %s\n", len(data.Facts), len(data.Links), *output)
	} else {
		os.Stdout.Write(buf)
		os.Stdout.Write([]byte("\n"))
		fmt.Fprintf(os.Stderr, "Exported %d facts and %d links\n", len(data.Facts), len(data.Links))
	}
}

// linksSkippedNote names the edges an import could not place because an
// endpoint was skipped as a duplicate; empty when every link landed.
func linksSkippedNote(r *memstore.ImportResult) string {
	if r.LinksSkipped == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d links skipped: an endpoint was a duplicate)", r.LinksSkipped)
}

func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	remote := fs.String("remote", cliConfig.Remote, "memstored URL to import into (default: remote from config.toml)")
	skipDuplicates := fs.Bool("skip-duplicates", false, "skip facts that already exist")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: memstore import [--remote url] [--skip-duplicates] file.json")
		os.Exit(1)
	}
	cliConfig.Remote = *remote

	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		log.Fatalf("read: %v", err)
	}

	var data memstore.ExportData
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Fatalf("parse: %v", err)
	}

	opts := memstore.ImportOpts{SkipDuplicates: *skipDuplicates}

	// Into a daemon, through the Store interface: facts, metadata,
	// created_at, supersession, and links travel; use and confirm counters do
	// not. The daemon has one namespace and the token has one user, so every
	// exported namespace lands in that one.
	if n := exportNamespaces(&data); len(n) > 1 {
		fmt.Fprintf(os.Stderr, "note: export spans namespaces %v; all land in the daemon's namespace\n", n)
	}
	store, cleanup, err := openStore()
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer cleanup()

	result, err := memstore.StoreImport(context.Background(), store, &data, opts)
	if err != nil {
		log.Fatalf("import: %v", err)
	}
	fmt.Printf("Imported %d facts and %d links into %s, skipped %d duplicates%s.\n",
		result.Imported, result.Links, cliConfig.Remote, result.Skipped, linksSkippedNote(result))
}

// exportNamespaces lists the distinct namespaces in an export, in order of
// first appearance.
func exportNamespaces(data *memstore.ExportData) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range data.Facts {
		if !seen[f.Namespace] {
			seen[f.Namespace] = true
			out = append(out, f.Namespace)
		}
	}
	return out
}
