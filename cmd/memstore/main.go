// Command memstore provides CLI access to a memstore database.
//
// Usage:
//
//	memstore export --db path/to/db.sqlite [--output=path]
//	memstore import --db path/to/db.sqlite [--skip-duplicates] file.json
//	memstore tasks [--surface startup] [--status pending] [--scope claude] [--format text|json]
//	memstore store --subject <s> --content <c> [--category note] [--metadata '{}'] [--supersedes id]
//	memstore list [--subject <s>] [--category <c>] [--metadata '{}'] [--format text|json]
//	memstore search --query <q> [--subject <s>] [--category <c>] [--limit 5] [--format text|json]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

func main() {
	log.SetFlags(0)

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
	case "list-project":
		runListProject(os.Args[2:])
	case "list-file":
		runListFile(os.Args[2:])
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
  search    FTS search facts by query text`)
}

// defaultDBPath returns the default database location, matching the MCP server default.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "memstore", "memory.db")
}

// openStore opens the database and returns a Store with a nil embedder.
// Embedding is deferred — the MCP server embeds lazily via NeedingEmbedding.
// Returns (nil, nil, nil) if the database file does not exist.
func openStore(dbPath, namespace string) (memstore.Store, func(), error) {
	return openStoreWithEmbedder(dbPath, namespace, nil)
}

// openStoreWithEmbedder is like openStore but wires in an embedder for hybrid search.
func openStoreWithEmbedder(dbPath, namespace string, embedder memstore.Embedder) (memstore.Store, func(), error) {
	if dbPath == "" {
		return nil, nil, fmt.Errorf("could not determine database path; use --db")
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, nil // DB not yet initialized — callers treat as empty
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := memstore.NewSQLiteStore(db, embedder, namespace)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return store, func() { db.Close() }, nil
}

func openDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("--db is required")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("database not found: %s", path)
	}
	return sql.Open("sqlite", path)
}

func runExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to memstore database (required)")
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
		fmt.Fprintf(os.Stderr, "Exported %d facts to %s\n", len(data.Facts), *output)
	} else {
		os.Stdout.Write(buf)
		os.Stdout.Write([]byte("\n"))
		fmt.Fprintf(os.Stderr, "Exported %d facts\n", len(data.Facts))
	}
}

func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to memstore database (required)")
	skipDuplicates := fs.Bool("skip-duplicates", false, "skip facts that already exist")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Usage: memstore import --db path/to/db.sqlite [--skip-duplicates] file.json")
		os.Exit(1)
	}

	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		log.Fatalf("read: %v", err)
	}

	var data memstore.ExportData
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Fatalf("parse: %v", err)
	}

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	result, err := memstore.Import(context.Background(), db, &data, memstore.ImportOpts{
		SkipDuplicates: *skipDuplicates,
	})
	if err != nil {
		log.Fatalf("import: %v", err)
	}

	fmt.Printf("Imported %d facts, skipped %d duplicates.\n", result.Imported, result.Skipped)
}
