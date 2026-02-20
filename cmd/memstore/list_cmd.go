package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/matthewjhunter/memstore"
)

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "path to memstore database")
	namespace := fs.String("namespace", "default", "namespace")
	format := fs.String("format", "text", "output format: text|json")
	subject := fs.String("subject", "", "filter by subject")
	category := fs.String("category", "", "filter by category")
	metadataStr := fs.String("metadata", "", `JSON object of equality filters (e.g. '{"surface":"startup"}')`)
	limit := fs.Int("limit", 0, "max results (0 = no limit)")
	onlyActive := fs.Bool("active", true, "exclude superseded facts")
	fs.Parse(args)

	var filters []memstore.MetadataFilter
	if *metadataStr != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(*metadataStr), &m); err != nil {
			log.Fatalf("list: invalid --metadata JSON: %v", err)
		}
		for k, v := range m {
			filters = append(filters, memstore.MetadataFilter{Key: k, Op: "=", Value: v})
		}
	}

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		return // DB not initialized yet; exit 0 silently
	}
	defer closeStore()

	facts, err := store.List(context.Background(), memstore.QueryOpts{
		Subject:         *subject,
		Category:        *category,
		OnlyActive:      *onlyActive,
		MetadataFilters: filters,
		Limit:           *limit,
	})
	if err != nil {
		log.Fatalf("list: %v", err)
	}

	switch *format {
	case "json":
		if err := writeJSON(os.Stdout, facts); err != nil {
			log.Fatalf("list: %v", err)
		}
	default:
		writeFactsText(os.Stdout, facts)
	}
}

// writeFactsText writes facts in a human-readable format.
func writeFactsText(w io.Writer, facts []memstore.Fact) {
	for _, f := range facts {
		fmt.Fprintf(w, "[id=%d] %s | %s | %s\n  %s\n\n",
			f.ID, f.Subject, f.Category, f.CreatedAt.Format("2006-01-02"),
			f.Content)
	}
}

// writeJSON encodes v as indented JSON to w.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
