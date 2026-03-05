// memstored is the memstore network daemon. It serves the memstore HTTP API
// and processes embeddings in the background.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	_ "modernc.org/sqlite"
)

func main() {
	cfg := memstore.LoadConfig()

	addr := flag.String("addr", ":8230", "listen address")
	dbPath := flag.String("db", cfg.DB, "path to SQLite database")
	namespace := flag.String("namespace", cfg.Namespace, "namespace")
	ollamaURL := flag.String("ollama", cfg.Ollama, "Ollama base URL")
	model := flag.String("model", cfg.Model, "embedding model name")
	apiKey := flag.String("api-key", "", "API key for authentication (empty = disabled)")
	embedInterval := flag.Duration("embed-interval", 2*time.Second, "embed queue poll interval")
	embedBatch := flag.Int("embed-batch", 32, "embed queue batch size")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	embedder := memstore.NewOllamaEmbedder(*ollamaURL, *model)

	store, err := memstore.NewSQLiteStore(db, embedder, *namespace)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	handler := httpapi.New(store, embedder, *apiKey)

	eq := httpapi.NewEmbedQueue(store, embedder, *embedInterval, *embedBatch)
	eq.Start()
	defer eq.Stop()

	srv := &http.Server{
		Addr:         *addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down...")
		srv.Close()
	}()

	log.Printf("memstored listening on %s (db=%s, namespace=%s, model=%s)", *addr, *dbPath, *namespace, *model)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
