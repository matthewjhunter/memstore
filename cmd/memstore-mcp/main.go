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

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/internal/hookcapture"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

// deprecationNotice is printed on every start. The binary and local SQLite
// mode go away in the release after next; the HTTP transport served by
// memstored is the supported runtime, and `memstore setup` registers it.
const deprecationNotice = "memstore-mcp: DEPRECATED -- this stdio binary and local SQLite mode are removed in the release after 0.5.x. " +
	"Run `memstore setup --remote <daemon-url>` to register MCP over HTTP; see MIGRATING.md for the export path if you use local mode."

func main() {
	cfg := memstore.LoadConfig()
	remote := flag.String("remote", cfg.Remote, "memstored URL for daemon mode (empty = local SQLite)")
	// Secrets are not flag defaults: flag prints defaults in --help output, which
	// would echo the configured key to the terminal. Resolved from cfg after Parse.
	apiKey := flag.String("api-key", "", "API key for memstored auth (default: from config file or MEMSTORE_API_KEY)")
	dbPath := flag.String("db", cfg.DB, "path to SQLite database (local mode only)")
	namespace := flag.String("namespace", cfg.Namespace, "namespace for fact isolation (local mode only)")
	ollamaURL := flag.String("ollama", cfg.Ollama, "LLM API base URL for chat generation (local mode only)")
	llmAPIKey := flag.String("llm-api-key", "", "API key for the chat LLM provider (default: from config file or MEMSTORE_LLM_API_KEY; empty = no auth)")
	genModel := flag.String("gen-model", cfg.GenModel, "LLM model for generation")
	noEmbeddings := flag.Bool("no-embeddings", false, "run without an embedding endpoint; search degrades to FTS5-only (local mode)")
	readOnly := flag.Bool("read-only", false, "register only retrieval tools; the store-mutating tools are not advertised (for RAG consumers). Pair with a token issued --scopes read so the daemon enforces it too")
	// Deprecated: the Stop hook now runs as `memstore hook`, so the hook no
	// longer requires this binary to be installed. These remain so a machine
	// whose hook script and whose binaries are updated in either order keeps
	// working; they go when this command does.
	hookMode := flag.Bool("hook", false, "deprecated: use `memstore hook`. Read Stop hook JSON from stdin, POST to memstored, exit")
	transcriptPath := flag.String("transcript", "", "deprecated: use `memstore hook --transcript`. Read JSONL transcript from path, POST to memstored, exit")
	flag.Parse()

	// stderr is the MCP client's log, which is where a person configuring a
	// stdio server looks when something is off.
	fmt.Fprintln(os.Stderr, deprecationNotice)

	// Fall back to the configured secrets when the flags are unset.
	if *apiKey == "" {
		*apiKey = cfg.APIKey
	}
	if *llmAPIKey == "" {
		*llmAPIKey = cfg.LLMAPIKey
	}

	tlsOpts := httpclient.ClientOptionsFromConfig(cfg)

	// Hook capture modes — run without starting the MCP server.
	capture := hookcapture.Options{
		Remote:  *remote,
		APIKey:  *apiKey,
		TLS:     tlsOpts,
		Respawn: []string{"--transcript"},
		Tool:    "memstore-mcp --hook",
	}
	if *hookMode {
		capture.Run()
		return
	}
	if *transcriptPath != "" {
		capture.RunTranscript(*transcriptPath)
		return
	}

	// Log to stderr to keep stdout clean for MCP JSON-RPC.
	log.SetOutput(os.Stderr)

	var store memstore.Store
	var embedder embedding.Embedder
	// embedGrant is vector-write authority, granted to the server rather than
	// derived from the store handle it already holds.
	var embedGrant memstore.EmbedStore
	// Hard byte bound on a single embed request, from the configured budget.
	// Only set in local mode; in daemon mode the server owns embedding.
	var embedCeiling int

	if *remote != "" {
		// Daemon mode: talk to memstored over HTTP.
		c, err := httpclient.NewWithOptions(*remote, *apiKey, tlsOpts)
		if err != nil {
			log.Fatalf("memstore-mcp: build remote client: %v", err)
		}
		store = c
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

		var embedDesc string
		if *noEmbeddings {
			embedDesc = "disabled (FTS-only)"
		} else {
			embCfg, err := memstore.EmbedConfigFromEnv()
			if err != nil {
				log.Fatalf("memstore-mcp: embedder config: %v", err)
			}
			embedder, err = embedding.New(embCfg)
			if err != nil {
				log.Fatalf("memstore-mcp: create embedder: %v", err)
			}
			memstore.LogEmbedModel(embCfg)
			embedCeiling = embCfg.Limits().MaxBytes
			embedDesc = embCfg.Model
		}

		sqlStore, err := memstore.NewSQLiteStore(db, embedder, *namespace)
		if err != nil {
			log.Fatalf("initializing store: %v", err)
		}
		if rr, rcfg, err := memstore.RerankerFromEnv("MEMSTORE_RERANK"); err != nil {
			log.Fatalf("memstore-mcp: %v", err)
		} else if rr != nil {
			sqlStore.SetReranker(rr)
			log.Printf("reranker configured (model=%s, normalize=%t)", rcfg.Model, rcfg.NormalizeScores)
			if !rcfg.NormalizeScores {
				log.Printf("WARNING: reranker NormalizeScores is off — a raw-logit backend " +
					"(llama.cpp) needs MEMSTORE_RERANK_NORMALIZE_SCORES=true for fusion.")
			}
		}
		store = sqlStore
		// Local mode is the only configuration that embeds at insert: there is
		// no daemon here, so nothing drains the embed queue. Captured from the
		// branch that has both the embedder and a store able to write vectors,
		// and granted to the server below.
		embedGrant = sqlStore
		log.Printf("memstore-mcp starting in local mode (db=%s, namespace=%s, embed=%s)",
			*dbPath, *namespace, embedDesc)
	}

	srvCfg := mcpserver.Config{Embed: embedGrant}
	// Which server gets built, not a field on it: a retrieval-only session is a
	// *MemoryServer, which holds a read handle and has no write handler to
	// register.
	readOnlySession := *readOnly
	// The rerank policy, from env. It is the whole policy now, not a starting
	// point: memory_rerank_settings reports it and no tool call changes it, so
	// what is configured here is what every caller gets unless it overrides
	// threshold or rerank_mode on the call itself. Applies in both modes -- in
	// remote mode the resolved mode/threshold are sent to the daemon, which owns
	// the reranker.
	if pol, err := memstore.RerankPolicyFromEnv("MEMSTORE_RERANK"); err != nil {
		log.Fatalf("memstore-mcp: rerank policy: %v", err)
	} else {
		srvCfg.RerankMode = pol.Mode
		srvCfg.RerankThreshold = pol.Threshold
		srvCfg.RerankCandidates = pol.Candidates
		srvCfg.RerankRecallCandidates = pol.RecallCandidates
		srvCfg.RerankDocBytes = pol.DocBytes
		srvCfg.RerankRecallDocBytes = pol.RecallDocBytes
	}
	if *remote != "" {
		// Daemon mode: generation and feedback go through memstored.
		rc, err := httpclient.NewWithOptions(*remote, *apiKey, tlsOpts)
		if err != nil {
			log.Fatalf("memstore-mcp: build remote feedback client: %v", err)
		}
		gen, err := httpclient.NewHTTPGeneratorWithOptions(*remote, *apiKey, tlsOpts)
		if err != nil {
			log.Fatalf("memstore-mcp: build remote generator: %v", err)
		}
		srvCfg.Generator = gen
		srvCfg.SessionStore = rc // enables memory_rate_context

		// Ask the daemon what this token may actually do, before the tool list
		// and the instructions are built from it. Advertising writes that the
		// daemon will 403 is the thing this avoids; the flag can only tighten
		// the answer, never loosen it. Local SQLite mode has no token and no
		// scope enforcement, so it keeps the flag value alone.
		readOnlySession = applyTokenScopes(context.Background(), rc, *readOnly)
	} else if *genModel != "" {
		// Local mode: talk to Ollama directly.
		srvCfg.Generator = memstore.NewOpenAIGenerator(*ollamaURL, *llmAPIKey, *genModel)
	}

	// registrar is what both server types have in common. Nothing else here
	// needs to know which one it got: the difference is the tool set each can
	// register, and that is settled by construction.
	type registrar interface {
		Register(*mcp.Server)
		SetEmbedCeiling(int)
	}
	var memorySrv registrar
	if readOnlySession {
		memorySrv = mcpserver.NewMemoryServerWithConfig(store, embedder, srvCfg)
	} else {
		memorySrv = mcpserver.NewWriteServerWithConfig(store, embedder, srvCfg)
	}
	// The configured budget, not the model's registered one: sizing chunks
	// against the registry while requests are clipped to a lower configured
	// budget truncates every chunk's tail silently.
	memorySrv.SetEmbedCeiling(embedCeiling)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "memstore",
		Version: "0.5.0",
	}, &mcp.ServerOptions{
		Instructions: mcpserver.Instructions(readOnlySession),
	})

	memorySrv.Register(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("server error: %v", err)
	}
	// Note: pending uploads are drained gradually by the Stop hook
	// (runHookCapture → drainOnePendingUpload), one per Stop event.
	// We don't drain on MCP shutdown anymore — Claude Code SIGKILLs the
	// server, so any post-Run cleanup wouldn't reliably execute anyway.
}
