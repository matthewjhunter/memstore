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
	ollamaURL := flag.String("ollama", cfg.Ollama, "Ollama base URL")
	model := flag.String("model", cfg.Model, "embedding model name")
	apiKey := flag.String("api-key", cfg.APIKey, "API key for authentication (empty = disabled)")
	genModel := flag.String("gen-model", cfg.GenModel, "LLM model for generation (enables /v1/generate)")
	embedInterval := flag.Duration("embed-interval", 2*time.Second, "embed queue poll interval")
	embedBatch := flag.Int("embed-batch", 32, "embed queue batch size")
	flag.Parse()

	embedder := memstore.NewOllamaEmbedder(*ollamaURL, *model)

	var store memstore.Store
	if *pgDSN != "" {
		pool, err := pgxpool.New(context.Background(), *pgDSN)
		if err != nil {
			log.Fatalf("connect to postgres: %v", err)
		}
		defer pool.Close()

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

	handlerOpts := []httpapi.HandlerOpt{httpapi.WithSessionContext(sessCtx)}
	if *genModel != "" {
		gen := memstore.NewOllamaGenerator(*ollamaURL, *genModel)
		handlerOpts = append(handlerOpts, httpapi.WithGenerator(gen))
		log.Printf("generation enabled (model=%s)", *genModel)
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
