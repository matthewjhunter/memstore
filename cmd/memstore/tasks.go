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
	format := fs.String("format", "text", "output format: text|json")
	surface := fs.String("surface", "", "filter by surface (e.g. startup)")
	status := fs.String("status", "", "filter by status (pending|in_progress|completed|cancelled)")
	scope := fs.String("scope", "", "filter by scope (matthew|claude|collaborative)")
	project := fs.String("project", "", "filter by project name")
	limit := fs.Int("limit", 0, "show only the top N tasks for this session, chosen by the daemon's task selector (0 = every matching task)")
	cwd := fs.String("cwd", "", "working directory the session is in; its repo's tasks rank first (used with --limit)")
	fs.Parse(args)

	if *limit > 0 || *cwd != "" {
		runTasksSelect(*format, *surface, *status, *scope, *cwd, *limit)
		return
	}

	store, closeStore, err := openStore()
	if err != nil {
		log.Fatal(err)
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

// runTasksSelect is the --limit/--cwd path: the daemon chooses, this side
// only resolves the repo name (it has the filesystem; the daemon does not)
// and renders.
func runTasksSelect(format, surface, status, scope, cwd string, limit int) {
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	client, err := newRemoteClient()
	if err != nil || cliConfig.Remote == "" {
		log.Fatalf("tasks: --limit and --cwd need a daemon (set remote in config.toml or MEMSTORE_REMOTE)")
	}
	resp, err := client.SelectTasks(context.Background(), memstore.TaskSelectRequest{
		CWD: cwd, Project: memstore.ProjectNameFromCWD(cwd), Limit: limit,
		Surface: surface, Status: status, Scope: scope,
	})
	if err != nil {
		log.Fatalf("tasks: %v", err)
	}
	switch format {
	case "json":
		if err := writeJSON(os.Stdout, resp); err != nil {
			log.Fatalf("tasks: %v", err)
		}
	default:
		writeTasksTextSelected(os.Stdout, resp.Tasks, resp.Total)
	}
}

// writeTasksText writes a hook-injectable plain-text task list.
func writeTasksText(w io.Writer, facts []memstore.Fact) {
	writeTasksTextSelected(w, facts, len(facts))
}

// writeTasksTextSelected is writeTasksText with a header that says when the
// list is a selection: five tasks shown as if they were all of them would
// have a session believe the backlog is five long.
func writeTasksTextSelected(w io.Writer, facts []memstore.Fact, total int) {
	if len(facts) == 0 {
		return
	}
	if total > len(facts) {
		fmt.Fprintf(w, "[MEMSTORE - Pending Tasks] (top %d of %d for this session; `memstore tasks` lists all)\n", len(facts), total)
	} else {
		fmt.Fprintln(w, "[MEMSTORE - Pending Tasks]")
	}
	for _, f := range facts {
		var meta map[string]any
		if len(f.Metadata) > 0 {
			json.Unmarshal(f.Metadata, &meta) //nolint:errcheck // malformed metadata just leaves meta nil; fields below degrade gracefully
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
