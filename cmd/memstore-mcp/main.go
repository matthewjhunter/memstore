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
	"strings"
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
	ollamaURL := flag.String("ollama", cfg.Ollama, "LLM API base URL (local mode only)")
	model := flag.String("model", cfg.Model, "embedding model name (local mode only)")
	llmAPIKey := flag.String("llm-api-key", cfg.LLMAPIKey, "API key for the LLM provider (empty = no auth)")
	genModel := flag.String("gen-model", cfg.GenModel, "LLM model for generation; enables memory_learn")
	noEmbeddings := flag.Bool("no-embeddings", false, "run without embeddings (FTS-only search, no vector similarity)")
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

		if !*noEmbeddings {
			embedder = memstore.NewOpenAIEmbedder(*ollamaURL, *llmAPIKey, *model)
		}

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
		srvCfg.Generator = memstore.NewOpenAIGenerator(*ollamaURL, *llmAPIKey, *genModel)
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

// sessionsDir returns the directory where per-session state files are written by the Stop hook.
func sessionsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "memstore", "sessions")
}

// uploadTranscriptOnShutdown scans the sessions directory for pending state files,
// atomically claims each one, uploads its transcript to memstored, and removes it.
//
// Per-session files (one per session_id) prevent concurrent sessions from
// overwriting each other's state. The atomic rename-to-.uploading ensures each
// file is uploaded exactly once even if multiple MCP server instances shut down
// simultaneously.
func uploadTranscriptOnShutdown(remote, apiKey string) {
	dir := sessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // directory doesn't exist yet — nothing to do
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		src := filepath.Join(dir, entry.Name())
		dst := src + ".uploading"
		// Atomic claim: only one process wins the rename.
		if err := os.Rename(src, dst); err != nil {
			continue // another process claimed it, or it disappeared
		}
		done := strings.TrimSuffix(src, ".json") + ".done"
		if err := uploadSessionFile(remote, apiKey, dst); err != nil {
			log.Printf("memstore-mcp: upload %s: %v", entry.Name(), err)
			os.Rename(dst, src) // restore for retry on next shutdown
		} else {
			os.Rename(dst, done) // keep as .done — skipped on future scans, useful for auditing
		}
	}
}

// uploadSessionFile reads a session state file, loads the transcript, and posts it to memstored.
func uploadSessionFile(remote, apiKey, statePath string) error {
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}
	var state struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		CWD            string `json:"cwd"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil || state.SessionID == "" {
		return nil // malformed — discard silently
	}
	content, err := os.ReadFile(state.TranscriptPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpclient.New(remote, apiKey).PostSessionTranscript(ctx, state.SessionID, state.CWD, string(content)); err != nil {
		return err
	}
	log.Printf("memstore-mcp: uploaded transcript for session %s", state.SessionID)
	return nil
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
// Session metadata (session_id, cwd) is resolved by scanning the sessions directory
// for a state file whose transcript_path matches the given path.
func runTranscriptCapture(remote, apiKey, path string) {
	if remote == "" {
		return
	}
	var sessionID, cwd string
	if entries, err := os.ReadDir(sessionsDir()); err == nil {
		for _, entry := range entries {
			ext := filepath.Ext(entry.Name())
			if entry.IsDir() || (ext != ".json" && ext != ".done") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sessionsDir(), entry.Name()))
			if err != nil {
				continue
			}
			var state struct {
				SessionID      string `json:"session_id"`
				TranscriptPath string `json:"transcript_path"`
				CWD            string `json:"cwd"`
			}
			if json.Unmarshal(data, &state) == nil && state.TranscriptPath == path {
				sessionID = state.SessionID
				cwd = state.CWD
				break
			}
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("memstore-mcp --transcript: read file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpclient.New(remote, apiKey).PostSessionTranscript(ctx, sessionID, cwd, string(content)); err != nil {
		log.Fatalf("memstore-mcp --transcript: post: %v", err)
	}
}
