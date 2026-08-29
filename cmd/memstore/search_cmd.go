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
	format := fs.String("format", "text", "output format: text|json")
	query := fs.String("query", "", "search query (required)")
	subject := fs.String("subject", "", "filter by subject")
	category := fs.String("category", "", "filter by category")
	limit := fs.Int("limit", 5, "max results")
	onlyActive := fs.Bool("active", true, "exclude superseded facts")
	searchMode := fs.String("search", modeAuto, searchModeUsage)
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

	// The daemon runs the vector arm server-side; the only local decision is
	// whether to ask for hybrid or FTS-only.
	useHybrid, err := resolveSearchMode(*searchMode)
	if err != nil {
		log.Fatalf("search: %v", err)
	}

	store, closeStore, err := openStore()
	if err != nil {
		log.Fatal(err)
	}
	defer closeStore()

	var results []memstore.SearchResult
	if useHybrid {
		results, err = store.Search(context.Background(), *query, opts)
		// A runtime failure (embedding endpoint down, daemon unreachable) is
		// treated the same way as an unavailable embedder above: auto degrades
		// with a warning, an explicit --search hybrid does not, because the
		// caller asked for semantic search and thin results would look like a
		// thin corpus.
		if err != nil && *searchMode == modeAuto {
			fmt.Fprintf(os.Stderr, "search: hybrid search failed (%v), falling back to %s\n", err, modeFTS)
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
