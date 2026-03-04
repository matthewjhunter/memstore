package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
)

func runCheckDrift(args []string) {
	fs := flag.NewFlagSet("check-drift", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "path to memstore database")
	namespace := fs.String("namespace", "default", "namespace")
	repoPath := fs.String("repo", "", "path to git repository root (default: positional arg)")
	subject := fs.String("subject", "", "scope to a single subject")
	sinceDays := fs.Int("since-days", 7, "only report facts stale due to changes in the last N days (0 = no limit)")
	fs.Parse(args)

	// Accept repo path as positional argument if --repo not provided.
	if *repoPath == "" && fs.NArg() > 0 {
		*repoPath = fs.Arg(0)
	}
	if *repoPath == "" {
		fmt.Fprintln(os.Stderr, "check-drift: --repo or positional path argument is required")
		os.Exit(1)
	}

	absRepo, err := resolveAbsPath(*repoPath)
	if err != nil {
		log.Fatalf("check-drift: resolve repo path: %v", err)
	}

	// Default subject to the directory name of the resolved repo path.
	if *subject == "" {
		*subject = filepath.Base(absRepo)
	}

	store, closeStore, err := openStore(*dbPath, *namespace)
	if err != nil {
		log.Fatalf("check-drift: open store: %v", err)
	}
	if store == nil {
		// DB not initialized — nothing to check.
		return
	}
	defer closeStore()

	ctx := context.Background()

	// List active facts, optionally scoped to subject.
	facts, err := store.List(ctx, memstore.QueryOpts{
		Subject:    *subject,
		OnlyActive: true,
	})
	if err != nil {
		log.Fatalf("check-drift: list facts: %v", err)
	}

	// Separate facts with source_files from those without.
	var withSource []memstore.Fact
	var noSource int
	for _, f := range facts {
		var meta map[string]any
		if len(f.Metadata) > 0 {
			_ = json.Unmarshal(f.Metadata, &meta)
		}
		if sf, ok := meta["source_files"]; ok && len(toStringSlice(sf)) > 0 {
			withSource = append(withSource, f)
		} else {
			noSource++
		}
	}

	if len(withSource) == 0 {
		// No facts with source_files — nothing to report.
		return
	}

	// Check each fact for staleness against git history.
	var sinceCutoff time.Time
	if *sinceDays > 0 {
		sinceCutoff = time.Now().AddDate(0, 0, -*sinceDays)
	}

	type staleResult struct {
		fact        memstore.Fact
		changedFile string
		fileChanged time.Time
	}

	var stale []staleResult
	for _, f := range withSource {
		var meta map[string]any
		_ = json.Unmarshal(f.Metadata, &meta)
		sourceFiles := toStringSlice(meta["source_files"])

		factTime := f.CreatedAt
		if f.LastConfirmedAt != nil && f.LastConfirmedAt.After(factTime) {
			factTime = *f.LastConfirmedAt
		}

		for _, file := range sourceFiles {
			modified, ok := gitFileModTime(ctx, absRepo, file)
			if !ok {
				continue
			}
			if modified.After(factTime) && (sinceCutoff.IsZero() || modified.After(sinceCutoff)) {
				stale = append(stale, staleResult{fact: f, changedFile: file, fileChanged: modified})
				break
			}
		}
	}

	currentCount := len(withSource) - len(stale)

	// Silent success — no stale facts means no output.
	if len(stale) == 0 {
		return
	}

	// Output drift report.
	fmt.Printf("[MEMSTORE - Drift Report]\n")
	fmt.Printf("\u26a0 %d stale facts (source files changed in last %d days):\n\n", len(stale), *sinceDays)

	for _, s := range stale {
		fmt.Printf("[id=%d] %s | %s", s.fact.ID, s.fact.Subject, s.fact.Category)
		if s.fact.Kind != "" {
			fmt.Printf(" | kind=%s", s.fact.Kind)
		}
		if s.fact.Subsystem != "" {
			fmt.Printf(" | subsystem=%s", s.fact.Subsystem)
		}
		fmt.Printf("\n  changed: %s (%s)\n  %s\n\n",
			s.changedFile, s.fileChanged.Format("2006-01-02"), truncate(s.fact.Content, 200))
	}

	fmt.Printf("\u2713 %d current", currentCount)
	if noSource > 0 {
		fmt.Printf(", %d unchecked (no source_files)", noSource)
	}
	fmt.Println()
}

// gitFileModTime returns the last commit time for a file in the given repo.
func gitFileModTime(ctx context.Context, repoPath, filePath string) (time.Time, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "log", "--format=%at", "-1", "--", filePath).Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return time.Time{}, false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(ts, 0), true
}

// toStringSlice converts an interface{} that may be []interface{} or []string to []string.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// truncate shortens s to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
