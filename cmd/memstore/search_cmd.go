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
	dbPath := fs.String("db", defaultDBPath(), "path to memstore database")
	namespace := fs.String("namespace", "default", "namespace")
	format := fs.String("format", "text", "output format: text|json")
	query := fs.String("query", "", "FTS search query (required)")
	subject := fs.String("subject", "", "filter by subject")
	category := fs.String("category", "", "filter by category")
	limit := fs.Int("limit", 5, "max results")
	onlyActive := fs.Bool("active", true, "exclude superseded facts")
	fs.Parse(args)

	if *query == "" {
		fmt.Fprintln(os.Stderr, "search: --query is required")
		os.Exit(1)
	}

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		return // DB not initialized yet; exit 0 silently
	}
	defer closeStore()

	results, err := store.SearchFTS(context.Background(), *query, memstore.SearchOpts{
		MaxResults: *limit,
		Subject:    *subject,
		Category:   *category,
		OnlyActive: *onlyActive,
	})
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
