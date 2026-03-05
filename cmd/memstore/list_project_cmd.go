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

func runListProject(args []string) {
	fs := flag.NewFlagSet("list-project", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	cwd := fs.String("cwd", "", "current working directory (required)")
	fs.Parse(args)

	if *cwd == "" {
		fmt.Fprintln(os.Stderr, "list-project: --cwd is required")
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

	cwdSlash := *cwd
	if !strings.HasSuffix(cwdSlash, "/") {
		cwdSlash += "/"
	}

	var matching []memstore.Fact
	seen := make(map[int64]bool)
	for _, surface := range []string{"project", "package"} {
		facts, err := store.List(context.Background(), memstore.QueryOpts{
			OnlyActive: true,
			MetadataFilters: []memstore.MetadataFilter{
				{Key: "surface", Op: "=", Value: surface},
			},
		})
		if err != nil {
			log.Fatalf("list-project: %v", err)
		}
		for _, f := range facts {
			if !seen[f.ID] && projectFactMatchesCWD(f, *cwd, cwdSlash) {
				seen[f.ID] = true
				matching = append(matching, f)
			}
		}
	}

	for _, f := range matching {
		fmt.Printf("[id=%d] %s | %s | %s\n  %s\n",
			f.ID, f.Subject, f.Category, f.CreatedAt.Format("2006-01-02"), f.Content)
		if q := projectFactQuality(f); q != "" {
			fmt.Printf("  [draft: %s — rewrite with memory_store + supersedes if you have better context]\n", q)
		}
		fmt.Println()
	}
}

// projectFactQuality returns the quality tag if the fact is a local-model draft, or "" otherwise.
func projectFactQuality(f memstore.Fact) string {
	if len(f.Metadata) == 0 {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal(f.Metadata, &meta); err != nil {
		return ""
	}
	if q, _ := meta["quality"].(string); strings.HasPrefix(q, "local") {
		return q
	}
	return ""
}

// projectFactMatchesCWD reports whether a project/package-surface fact applies to cwd.
// Handles project_path and package_path metadata keys (both use prefix matching).
// Note: project_subject resolution is not available in the CLI (server config only).
func projectFactMatchesCWD(f memstore.Fact, cwd, cwdSlash string) bool {
	var meta map[string]any
	if len(f.Metadata) > 0 {
		_ = json.Unmarshal(f.Metadata, &meta)
	}
	for _, key := range []string{"project_path", "package_path"} {
		if pp, _ := meta[key].(string); pp != "" {
			if cwd == pp {
				return true
			}
			ppSlash := pp
			if !strings.HasSuffix(ppSlash, "/") {
				ppSlash += "/"
			}
			if strings.HasPrefix(cwdSlash, ppSlash) {
				return true
			}
		}
	}
	return false
}
