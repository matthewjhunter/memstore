package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/matthewjhunter/memstore"
)

func runSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	format := fs.String("format", "text", "output format: text|json")
	query := fs.String("query", "", "search query (required)")
	subject := fs.String("subject", "", "filter by subject")
	category := fs.String("category", "", "filter by category")
	limit := fs.Int("limit", 5, "max results")
	onlyActive := fs.Bool("active", true, "exclude superseded facts")
	hybrid := fs.Bool("hybrid", false, "use hybrid FTS+vector search (requires Ollama)")
	ollamaURL := fs.String("ollama-url", cliConfig.Ollama, "Ollama base URL (for --hybrid)")
	model := fs.String("model", cliConfig.Model, "embedding model name (for --hybrid)")
	fs.Parse(args)

	if *query == "" {
		fmt.Fprintln(os.Stderr, "search: --query is required")
		os.Exit(1)
	}

	opts := memstore.SearchOpts{
		MaxResults: *limit,
		Subject:    *subject,
		Category:   *category,
		OnlyActive: *onlyActive,
	}

	var store memstore.Store
	var closeStore func()
	var err error

	if *hybrid {
		embedder := memstore.NewOllamaEmbedder(*ollamaURL, *model)
		store, closeStore, err = openStoreWithEmbedder(*dbPath, *namespace, embedder)
	} else {
		store, closeStore, err = openStore(*dbPath, *namespace)
	}
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		return // DB not initialized yet; exit 0 silently
	}
	defer closeStore()

	var results []memstore.SearchResult
	if *hybrid {
		results, err = store.Search(context.Background(), *query, opts)
		if err != nil {
			// Ollama may be unavailable; fall back to FTS and warn.
			fmt.Fprintf(os.Stderr, "search: hybrid search failed (%v), falling back to FTS\n", err)
			results, err = store.SearchFTS(context.Background(), *query, opts)
		}
	} else {
		results, err = store.SearchFTS(context.Background(), *query, opts)
	}
	if err != nil {
		log.Fatalf("search: %v", err)
	}

	switch *format {
	case "json":
		facts := make([]memstore.Fact, len(results))
		for i, r := range results {
			facts[i] = r.Fact
		}
		if err := writeJSON(os.Stdout, facts); err != nil {
			log.Fatalf("search: %v", err)
		}
	default:
		writeSearchText(os.Stdout, results)
	}
}

func writeSearchText(w io.Writer, results []memstore.SearchResult) {
	for _, r := range results {
		f := r.Fact
		fmt.Fprintf(w, "[id=%d] %s | %s | %s\n  %s\n\n",
			f.ID, f.Subject, f.Category, f.CreatedAt.Format("2006-01-02"),
			f.Content)
	}
}
