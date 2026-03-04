package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/matthewjhunter/memstore"
)

func runListFile(args []string) {
	fs := flag.NewFlagSet("list-file", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "path to memstore database")
	namespace := fs.String("namespace", "default", "namespace")
	filePath := fs.String("file", "", "absolute file path (required)")
	symbolName := fs.String("symbol", "", "symbol name to narrow symbol-surface results")
	fs.Parse(args)

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "list-file: --file is required")
		os.Exit(1)
	}

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		return
	}
	defer closeStore()

	ctx := context.Background()

	// Query surface=file (exact file path match).
	fileFacts, err := store.List(ctx, memstore.QueryOpts{
		OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "surface", Op: "=", Value: "file"},
			{Key: "file_path", Op: "=", Value: *filePath},
		},
	})
	if err != nil {
		log.Fatalf("list-file: %v", err)
	}

	// Query surface=symbol (same file path).
	symbolFacts, err := store.List(ctx, memstore.QueryOpts{
		OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "surface", Op: "=", Value: "symbol"},
			{Key: "file_path", Op: "=", Value: *filePath},
		},
	})
	if err != nil {
		log.Fatalf("list-file: %v", err)
	}

	// Narrow by symbol name if requested.
	if *symbolName != "" {
		lower := strings.ToLower(*symbolName)
		var filtered []memstore.Fact
		for _, f := range symbolFacts {
			var meta map[string]any
			if len(f.Metadata) > 0 {
				_ = json.Unmarshal(f.Metadata, &meta)
			}
			if sn, _ := meta["symbol_name"].(string); strings.ToLower(sn) == lower {
				filtered = append(filtered, f)
			}
		}
		symbolFacts = filtered
	}

	if len(fileFacts)+len(symbolFacts) == 0 {
		// No output — hook will see empty stdout and inject nothing.
		return
	}

	fmt.Printf("[file context for: %s]\n\n", *filePath)

	if len(fileFacts) > 0 {
		fmt.Println("--- file ---")
		for _, f := range fileFacts {
			printContextFact(f)
		}
		fmt.Println()
	}

	if len(symbolFacts) > 0 {
		if *symbolName != "" {
			fmt.Printf("--- symbol: %s ---\n", *symbolName)
		} else {
			fmt.Println("--- symbols ---")
		}
		for _, f := range symbolFacts {
			printContextFact(f)
		}
	}
}

func printContextFact(f memstore.Fact) {
	fmt.Printf("[id=%d] %s | %s", f.ID, f.Subject, f.Category)
	if f.Kind != "" {
		fmt.Printf(" | kind=%s", f.Kind)
	}
	if f.Subsystem != "" {
		fmt.Printf(" | subsystem=%s", f.Subsystem)
	}
	fmt.Println()
	fmt.Printf("  %s\n", f.Content)
	fmt.Println()
}
