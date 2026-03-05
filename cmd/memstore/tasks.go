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

func runTasks(args []string) {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	format := fs.String("format", "text", "output format: text|json")
	surface := fs.String("surface", "", "filter by surface (e.g. startup)")
	status := fs.String("status", "", "filter by status (pending|in_progress|completed|cancelled)")
	scope := fs.String("scope", "", "filter by scope (matthew|claude|collaborative)")
	project := fs.String("project", "", "filter by project name")
	fs.Parse(args)

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatal(err)
	}
	if store == nil {
		return // DB not initialized yet; exit 0 silently
	}
	defer closeStore()

	filters := []memstore.MetadataFilter{
		{Key: "kind", Op: "=", Value: "task"},
	}
	if *surface != "" {
		filters = append(filters, memstore.MetadataFilter{Key: "surface", Op: "=", Value: *surface})
	}
	if *status != "" {
		filters = append(filters, memstore.MetadataFilter{Key: "status", Op: "=", Value: *status})
	}
	if *scope != "" {
		filters = append(filters, memstore.MetadataFilter{Key: "scope", Op: "=", Value: *scope})
	}
	if *project != "" {
		filters = append(filters, memstore.MetadataFilter{Key: "project", Op: "=", Value: *project})
	}

	facts, err := store.List(context.Background(), memstore.QueryOpts{
		OnlyActive:      true,
		MetadataFilters: filters,
	})
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}

	switch *format {
	case "json":
		if err := writeJSON(os.Stdout, facts); err != nil {
			log.Fatalf("tasks: %v", err)
		}
	default:
		writeTasksText(os.Stdout, facts)
	}
}

// writeTasksText writes a hook-injectable plain-text task list.
func writeTasksText(w io.Writer, facts []memstore.Fact) {
	if len(facts) == 0 {
		return
	}
	fmt.Fprintln(w, "[MEMSTORE - Pending Tasks]")
	for _, f := range facts {
		var meta map[string]any
		if len(f.Metadata) > 0 {
			json.Unmarshal(f.Metadata, &meta) //nolint:errcheck
		}
		prefix := ""
		if p, _ := meta["priority"].(string); p == "high" {
			prefix = "[high] "
		}
		suffix := ""
		if p, _ := meta["project"].(string); p != "" {
			suffix = fmt.Sprintf(" (project: %s)", p)
		}
		fmt.Fprintf(w, "• %s%s%s\n", prefix, f.Content, suffix)
	}
}
