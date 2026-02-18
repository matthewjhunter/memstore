// Command memstore-mcp is an MCP server that gives Claude (or any MCP client)
// persistent, searchable memory backed by SQLite with hybrid FTS5 + vector search.
//
// Usage:
//
//	memstore-mcp [flags]
//
// Flags:
//
//	--db         Path to SQLite database (default: ~/.local/share/memstore/memory.db)
//	--namespace  Namespace for fact isolation (default: "default")
//	--ollama     Ollama base URL (default: http://localhost:11434)
//	--model      Embedding model name (default: nomic-embed-text)
//
// The server communicates over stdio using newline-delimited JSON-RPC
// (the MCP stdio transport). Register it with Claude Code via:
//
//	claude mcp add memstore -s user -- /path/to/memstore-mcp [flags]
//
// This stores the config in ~/.claude.json at user scope so it is
// available in all projects.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", defaultDBPath(), "path to SQLite database")
	namespace := flag.String("namespace", "default", "namespace for fact isolation")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama base URL")
	model := flag.String("model", "embeddinggemma", "embedding model name")
	flag.Parse()

	// Log to stderr to keep stdout clean for MCP JSON-RPC.
	log.SetOutput(os.Stderr)

	// Ensure the database directory exists.
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
		log.Fatalf("creating db directory: %v", err)
	}

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer db.Close()

	// Single connection for WAL mode correctness with memstore's mutex.
	db.SetMaxOpenConns(1)

	embedder := memstore.NewOllamaEmbedder(*ollamaURL, *model)

	store, err := memstore.NewSQLiteStore(db, embedder, *namespace)
	if err != nil {
		log.Fatalf("initializing store: %v", err)
	}

	memorySrv := mcpserver.NewMemoryServer(store, embedder)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "memstore",
		Version: "0.1.0",
	}, nil)

	memorySrv.Register(server)

	log.Printf("memstore-mcp starting (db=%s, namespace=%s, model=%s)", *dbPath, *namespace, *model)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// defaultDBPath returns ~/.local/share/memstore/memory.db, following the
// XDG Base Directory Specification for user data.
func defaultDBPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "memstore", "memory.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot determine home directory: %v\n", err)
		return "memory.db"
	}
	return filepath.Join(home, ".local", "share", "memstore", "memory.db")
}
