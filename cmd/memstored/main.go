// memstored is the memstore network daemon. It serves the memstore HTTP API
// and processes embeddings in the background.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run executes the memstored daemon with the given arguments. It returns when
// ctx is cancelled or the server exits with an error. Extracted from main so
// tests can drive the lifecycle directly.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	cfg := memstore.LoadConfig()

	defaultAddr := cfg.Addr
	if defaultAddr == "" {
		defaultAddr = ":8230"
	}

	fs := flag.NewFlagSet("memstored", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultAddr, "listen address")
	dbPath := fs.String("db", cfg.DB, "path to SQLite database")
	pgDSN := fs.String("pg", cfg.PG, "PostgreSQL connection string (overrides --db)")
	vecDim := fs.Int("vec-dim", cfg.VecDim, "embedding vector dimension for Postgres (e.g. 768)")
	namespace := fs.String("namespace", cfg.Namespace, "namespace")
	ollamaURL := fs.String("ollama", cfg.Ollama, "LLM API base URL (Ollama, LiteLLM, or OpenAI-compatible)")
	model := fs.String("model", cfg.Model, "embedding model name")
	apiKey := fs.String("api-key", cfg.APIKey, "API key for authentication (empty = disabled)")
	llmAPIKey := fs.String("llm-api-key", cfg.LLMAPIKey, "API key for the LLM provider (empty = no auth)")
	genModel := fs.String("gen-model", cfg.GenModel, "LLM model for generation (enables /v1/generate)")
	genURL := fs.String("gen-url", cfg.GenURL, "separate LLM URL for generation (defaults to --ollama)")
	embedInterval := fs.Duration("embed-interval", 2*time.Second, "embed queue poll interval")
	embedBatch := fs.Int("embed-batch", 32, "embed queue batch size")
	if err := fs.Parse(args); err != nil {
		return err
	}

	embedder := memstore.NewOpenAIEmbedder(*ollamaURL, *llmAPIKey, *model)

	var store memstore.Store
	var pgPool *pgxpool.Pool
	if *pgDSN != "" {
		pool, err := pgxpool.New(ctx, *pgDSN)
		if err != nil {
			return fmt.Errorf("connect to postgres: %w", err)
		}
		defer pool.Close()
		pgPool = pool

		pgStore, err := pgstore.New(ctx, pool, embedder, *namespace, *vecDim)
		if err != nil {
			return fmt.Errorf("init postgres store: %w", err)
		}
		store = pgStore
		log.Printf("using PostgreSQL store (dim=%d)", *vecDim)
	} else {
		db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer db.Close()

		sqliteStore, err := memstore.NewSQLiteStore(db, embedder, *namespace)
		if err != nil {
			return fmt.Errorf("init store: %w", err)
		}
		store = sqliteStore
		log.Printf("using SQLite store (db=%s)", *dbPath)
	}

	sessCtx := httpapi.NewSessionContext()
	defer sessCtx.Stop()

	handlerOpts := []httpapi.HandlerOpt{
		httpapi.WithSessionContext(sessCtx),
	}
	var sessionStore *pgstore.SessionStore
	if pgPool != nil {
		if ss, err := pgstore.NewSessionStore(ctx, pgPool); err == nil {
			sessionStore = ss
			handlerOpts = append(handlerOpts, httpapi.WithSessionStore(ss))
			log.Printf("session store enabled")
		} else {
			log.Printf("session store init failed: %v", err)
		}
	}
	var xq *httpapi.ExtractQueue
	if *genModel != "" {
		genBaseURL := *ollamaURL
		if *genURL != "" {
			genBaseURL = *genURL
		}
		gen := memstore.NewOpenAIGenerator(genBaseURL, *llmAPIKey, *genModel)
		handlerOpts = append(handlerOpts, httpapi.WithGenerator(gen))
		log.Printf("generation enabled (model=%s, url=%s)", *genModel, genBaseURL)
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
		// Budget ~3s per fact × ~40 facts/session × ~60 sessions ≈ 2h.
		go func() {
			bfCtx, cancel := context.WithTimeout(ctx, 4*time.Hour)
			defer cancel()
			result, err := xq.BackfillFeedback(bfCtx, func(done, total int) {
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

	// Cancel-on-ctx: close the server when the parent context fires.
	go func() {
		<-ctx.Done()
		log.Println("shutting down...")
		srv.Close()
	}()

	log.Printf("memstored listening on %s (namespace=%s, model=%s)", *addr, *namespace, *model)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}
