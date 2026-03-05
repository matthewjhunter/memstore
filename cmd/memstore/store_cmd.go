package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/matthewjhunter/memstore"
)

func runStore(args []string) {
	fs := flag.NewFlagSet("store", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	subject := fs.String("subject", "", "entity this fact is about (required)")
	content := fs.String("content", "", "the factual claim to store (required)")
	category := fs.String("category", "note", "fact category")
	kind := fs.String("kind", "", "structural type (convention, failure_mode, invariant, pattern, decision, trigger)")
	subsystem := fs.String("subsystem", "", "project subsystem (e.g. feeds, auth)")
	metadataStr := fs.String("metadata", "", `JSON metadata object (e.g. '{"key":"val"}')`)
	var supersedes int64
	fs.Int64Var(&supersedes, "supersedes", 0, "ID of the fact this replaces")
	fs.Parse(args)

	if *subject == "" || *content == "" {
		fmt.Fprintln(os.Stderr, "store: --subject and --content are required")
		fs.Usage()
		os.Exit(1)
	}

	var meta map[string]any
	if *metadataStr != "" {
		if err := json.Unmarshal([]byte(*metadataStr), &meta); err != nil {
			log.Fatalf("store: invalid --metadata JSON: %v", err)
		}
	}

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		log.Fatalf("store: database not found at %s (run memstore-mcp first to initialize)", *dbPath)
	}
	defer closeStore()

	f := memstore.Fact{
		Subject:   *subject,
		Content:   *content,
		Category:  *category,
		Kind:      *kind,
		Subsystem: *subsystem,
	}
	if len(meta) > 0 {
		raw, _ := json.Marshal(meta)
		f.Metadata = raw
	}

	id, err := store.Insert(context.Background(), f)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	if supersedes > 0 {
		if err := store.Supersede(context.Background(), supersedes, id); err != nil {
			log.Fatalf("store: supersede: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Stored (id=%d, supersedes=%d)\n", id, supersedes)
	} else {
		fmt.Fprintf(os.Stderr, "Stored (id=%d)\n", id)
	}
}
