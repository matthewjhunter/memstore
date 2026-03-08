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
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

func main() {
	cfg := memstore.LoadConfig()
	remote := flag.String("remote", cfg.Remote, "memstored URL for daemon mode (empty = local SQLite)")
	apiKey := flag.String("api-key", cfg.APIKey, "API key for memstored auth")
	dbPath := flag.String("db", cfg.DB, "path to SQLite database (local mode only)")
	namespace := flag.String("namespace", cfg.Namespace, "namespace for fact isolation (local mode only)")
	ollamaURL := flag.String("ollama", cfg.Ollama, "Ollama base URL (local mode only)")
	model := flag.String("model", cfg.Model, "embedding model name (local mode only)")
	genModel := flag.String("gen-model", cfg.GenModel, "LLM model for generation (e.g. qwen2.5:7b); enables memory_learn")
	hookMode := flag.Bool("hook", false, "read Stop hook JSON from stdin, POST to memstored, exit")
	transcriptPath := flag.String("transcript", "", "read JSONL transcript from path, POST to memstored, exit")
	flag.Parse()

	// Hook capture modes — run without starting the MCP server.
	if *hookMode {
		runHookCapture(*remote, *apiKey)
		return
	}
	if *transcriptPath != "" {
		runTranscriptCapture(*remote, *apiKey, *transcriptPath)
		return
	}

	// Log to stderr to keep stdout clean for MCP JSON-RPC.
	log.SetOutput(os.Stderr)

	var store memstore.Store
	var embedder memstore.Embedder

	if *remote != "" {
		// Daemon mode: talk to memstored over HTTP.
		store = httpclient.New(*remote, *apiKey)
		log.Printf("memstore-mcp starting in daemon mode (remote=%s)", *remote)
	} else {
		// Local mode: open SQLite directly.
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

		embedder = memstore.NewOllamaEmbedder(*ollamaURL, *model)

		sqlStore, err := memstore.NewSQLiteStore(db, embedder, *namespace)
		if err != nil {
			log.Fatalf("initializing store: %v", err)
		}
		store = sqlStore
		log.Printf("memstore-mcp starting in local mode (db=%s, namespace=%s, model=%s)", *dbPath, *namespace, *model)
	}

	srvCfg := mcpserver.Config{}
	if *remote != "" {
		// Daemon mode: generation and learning go through memstored.
		rc := httpclient.New(*remote, *apiKey)
		srvCfg.Generator = httpclient.NewHTTPGenerator(*remote, *apiKey)
		srvCfg.Learner = rc
		srvCfg.SessionStore = rc // enables memory_rate_context
	} else if *genModel != "" {
		// Local mode: talk to Ollama directly.
		srvCfg.Generator = memstore.NewOllamaGenerator(*ollamaURL, *genModel)
		// Learner auto-created from store+embedder+generator in NewMemoryServerWithConfig.
	}

	memorySrv := mcpserver.NewMemoryServerWithConfig(store, embedder, srvCfg)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "memstore",
		Version: "0.1.0",
	}, nil)

	memorySrv.Register(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("server error: %v", err)
	}

	// Session ended — upload transcript once if a state file exists.
	if *remote != "" {
		uploadTranscriptOnShutdown(*remote, *apiKey)
	}
}

// sessionStateFile returns the path to the current-session state file written by the Stop hook.
func sessionStateFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "memstore", "current-session.json")
}

// uploadTranscriptOnShutdown reads the session state file written by the Stop hook,
// uploads the JSONL transcript to memstored, and removes the state file.
func uploadTranscriptOnShutdown(remote, apiKey string) {
	stateData, err := os.ReadFile(sessionStateFile())
	if err != nil {
		return // no state file — nothing to do
	}
	var state struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		CWD            string `json:"cwd"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil || state.SessionID == "" {
		return
	}
	content, err := os.ReadFile(state.TranscriptPath)
	if err != nil {
		log.Printf("memstore-mcp: read transcript %s: %v", state.TranscriptPath, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := httpclient.New(remote, apiKey)
	if err := c.PostSessionTranscript(ctx, state.SessionID, state.CWD, string(content)); err != nil {
		log.Printf("memstore-mcp: upload transcript: %v", err)
		return
	}
	os.Remove(sessionStateFile())
	log.Printf("memstore-mcp: uploaded transcript for session %s", state.SessionID)
}

// runHookCapture reads a Claude Code Stop hook payload from stdin and POSTs it to memstored.
func runHookCapture(remote, apiKey string) {
	if remote == "" {
		return // no remote configured — silently skip
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("memstore-mcp --hook: read stdin: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpclient.New(remote, apiKey).PostSessionHook(ctx, json.RawMessage(data)); err != nil {
		log.Fatalf("memstore-mcp --hook: post: %v", err)
	}
}

// runTranscriptCapture reads a JSONL transcript file and POSTs it to memstored.
// Session metadata is read from the state file written by the Stop hook.
func runTranscriptCapture(remote, apiKey, path string) {
	if remote == "" {
		return
	}
	var state struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	if stateData, err := os.ReadFile(sessionStateFile()); err == nil {
		json.Unmarshal(stateData, &state)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("memstore-mcp --transcript: read file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpclient.New(remote, apiKey).PostSessionTranscript(ctx, state.SessionID, state.CWD, string(content)); err != nil {
		log.Fatalf("memstore-mcp --transcript: post: %v", err)
	}
}
