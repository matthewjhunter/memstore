// memstored is the memstore network daemon. It serves the memstore HTTP API
// and processes embeddings in the background.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/pgstore"
	_ "modernc.org/sqlite"
)

func main() {
	cfg := memstore.LoadConfig()

	defaultAddr := cfg.Addr
	if defaultAddr == "" {
		defaultAddr = ":8230"
	}
	addr := flag.String("addr", defaultAddr, "listen address")
	dbPath := flag.String("db", cfg.DB, "path to SQLite database")
	pgDSN := flag.String("pg", cfg.PG, "PostgreSQL connection string (overrides --db)")
	vecDim := flag.Int("vec-dim", cfg.VecDim, "embedding vector dimension for Postgres (e.g. 768)")
	namespace := flag.String("namespace", cfg.Namespace, "namespace")
	ollamaURL := flag.String("ollama", cfg.Ollama, "LLM API base URL (Ollama, LiteLLM, or OpenAI-compatible)")
	model := flag.String("model", cfg.Model, "embedding model name")
	apiKey := flag.String("api-key", cfg.APIKey, "API key for authentication (empty = disabled)")
	llmAPIKey := flag.String("llm-api-key", cfg.LLMAPIKey, "API key for the LLM provider (empty = no auth)")
	genModel := flag.String("gen-model", cfg.GenModel, "LLM model for generation (enables /v1/generate)")
	embedInterval := flag.Duration("embed-interval", 2*time.Second, "embed queue poll interval")
	embedBatch := flag.Int("embed-batch", 32, "embed queue batch size")
	flag.Parse()

	embedder := memstore.NewOpenAIEmbedder(*ollamaURL, *llmAPIKey, *model)

	var store memstore.Store
	var pgPool *pgxpool.Pool
	if *pgDSN != "" {
		pool, err := pgxpool.New(context.Background(), *pgDSN)
		if err != nil {
			log.Fatalf("connect to postgres: %v", err)
		}
		defer pool.Close()
		pgPool = pool

		pgStore, err := pgstore.New(context.Background(), pool, embedder, *namespace, *vecDim)
		if err != nil {
			log.Fatalf("init postgres store: %v", err)
		}
		store = pgStore
		log.Printf("using PostgreSQL store (dim=%d)", *vecDim)
	} else {
		db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			log.Fatalf("open db: %v", err)
		}
		defer db.Close()

		sqliteStore, err := memstore.NewSQLiteStore(db, embedder, *namespace)
		if err != nil {
			log.Fatalf("init store: %v", err)
		}
		store = sqliteStore
		log.Printf("using SQLite store (db=%s)", *dbPath)
	}

	sessCtx := httpapi.NewSessionContext()
	defer sessCtx.Stop()

	learnSessions := httpapi.NewLearnSessionStore()
	defer learnSessions.Stop()

	handlerOpts := []httpapi.HandlerOpt{
		httpapi.WithSessionContext(sessCtx),
		httpapi.WithLearnSessions(learnSessions),
	}
	var sessionStore *pgstore.SessionStore
	if pgPool != nil {
		if ss, err := pgstore.NewSessionStore(context.Background(), pgPool); err == nil {
			sessionStore = ss
			handlerOpts = append(handlerOpts, httpapi.WithSessionStore(ss))
			log.Printf("session store enabled")
		} else {
			log.Printf("session store init failed: %v", err)
		}
	}
	var xq *httpapi.ExtractQueue
	if *genModel != "" {
		gen := memstore.NewOpenAIGenerator(*ollamaURL, *llmAPIKey, *genModel)
		handlerOpts = append(handlerOpts, httpapi.WithGenerator(gen))
		log.Printf("generation enabled (model=%s)", *genModel)
		if sessionStore != nil {
			xq = httpapi.NewExtractQueue(store, embedder, gen, sessionStore)
			xq.Start()
			handlerOpts = append(handlerOpts, httpapi.WithExtractQueue(xq))
			log.Printf("extract queue enabled with hint generation (gen-model=%s)", *genModel)
		} else {
			log.Printf("extract queue disabled: requires PostgreSQL session store (--pg)")
		}
	}
	if xq != nil {
		defer xq.Stop()
		// Backfill feedback scores for historical sessions on startup.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			result, err := xq.BackfillFeedback(ctx, func(done, total int) {
				log.Printf("backfill-feedback: %d/%d sessions", done, total)
			})
			if err != nil {
				log.Printf("backfill-feedback: %v", err)
				return
			}
			if result.Sessions > 0 {
				log.Printf("backfill-feedback: done — %d sessions, %d ratings, %d errors",
					result.Sessions, result.Rated, result.Errors)
			}
		}()
	}
	handler := httpapi.New(store, embedder, *apiKey, handlerOpts...)

	eq := httpapi.NewEmbedQueue(store, embedder, *embedInterval, *embedBatch)
	eq.Start()
	defer eq.Stop()

	srv := &http.Server{
		Addr:         *addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down...")
		srv.Close()
	}()

	log.Printf("memstored listening on %s (namespace=%s, model=%s)", *addr, *namespace, *model)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
