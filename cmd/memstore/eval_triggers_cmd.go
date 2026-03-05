package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"strings"

	"github.com/matthewjhunter/memstore"
)

func runEvalTriggers(args []string) {
	fs := flag.NewFlagSet("eval-triggers", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	filePath := fs.String("file", "", "absolute file path to evaluate triggers against (required)")
	fs.Parse(args)

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "eval-triggers: --file is required")
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

	// Find all trigger facts with signal_type=file_pattern.
	triggers, err := store.List(ctx, memstore.QueryOpts{
		Kind:       "trigger",
		OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "signal_type", Op: "=", Value: "file_pattern"},
		},
	})
	if err != nil {
		log.Fatalf("eval-triggers: %v", err)
	}

	if len(triggers) == 0 {
		return
	}

	// Evaluate each trigger against the file path.
	type triggerMatch struct {
		trigger     memstore.Fact
		loadSub     string
		loadKinds   []string
		loadSubject string
	}
	var matches []triggerMatch

	for _, t := range triggers {
		var meta map[string]any
		if len(t.Metadata) == 0 {
			continue
		}
		if err := json.Unmarshal(t.Metadata, &meta); err != nil {
			continue
		}
		signal, _ := meta["signal"].(string)
		if signal == "" {
			continue
		}
		if !matchFilePattern(signal, *filePath) {
			continue
		}
		loadSub, _ := meta["load_subsystem"].(string)
		loadSubject, _ := meta["load_subject"].(string)
		var loadKinds []string
		if rawKinds, ok := meta["load_kinds"].([]any); ok {
			for _, k := range rawKinds {
				if s, ok := k.(string); ok {
					loadKinds = append(loadKinds, s)
				}
			}
		}
		matches = append(matches, triggerMatch{
			trigger:     t,
			loadSub:     loadSub,
			loadKinds:   loadKinds,
			loadSubject: loadSubject,
		})
	}

	if len(matches) == 0 {
		return
	}

	// Load context facts for each matching trigger, deduplicating by fact ID.
	seen := make(map[int64]bool)
	fmt.Printf("[trigger context for: %s]\n", *filePath)

	for _, m := range matches {
		fmt.Printf("\n--- trigger: %s ---\n", m.trigger.Content)

		if m.loadSub == "" && m.loadSubject == "" {
			continue
		}

		if len(m.loadKinds) > 0 {
			// Query each kind separately since QueryOpts only supports a single Kind filter.
			for _, kind := range m.loadKinds {
				opts := memstore.QueryOpts{
					Subsystem:  m.loadSub,
					Subject:    m.loadSubject,
					Kind:       kind,
					OnlyActive: true,
				}
				facts, err := store.List(ctx, opts)
				if err != nil {
					continue
				}
				for _, f := range facts {
					if !seen[f.ID] {
						seen[f.ID] = true
						printContextFact(f)
					}
				}
			}
		} else {
			opts := memstore.QueryOpts{
				Subsystem:  m.loadSub,
				Subject:    m.loadSubject,
				OnlyActive: true,
			}
			facts, err := store.List(ctx, opts)
			if err != nil {
				continue
			}
			for _, f := range facts {
				if !seen[f.ID] {
					seen[f.ID] = true
					printContextFact(f)
				}
			}
		}
	}
}

// matchFilePattern checks if filePath matches a trigger's glob pattern.
// Patterns are relative (e.g. "internal/feeds/**"). The function tries
// matching against successive suffixes of the absolute file path.
// Supports ** as "match any path segments".
func matchFilePattern(pattern, filePath string) bool {
	// Clean both paths.
	pattern = path.Clean(pattern)
	filePath = path.Clean(filePath)

	// Split file path into segments and try matching against each suffix.
	parts := strings.Split(filePath, "/")
	for i := range parts {
		suffix := strings.Join(parts[i:], "/")
		if globMatch(pattern, suffix) {
			return true
		}
	}
	return false
}

// globMatch matches a pattern against a path string, supporting ** for
// matching zero or more directory segments. Single-segment wildcards (*) and
// character matching (?) are handled by path.Match.
func globMatch(pattern, name string) bool {
	// If no ** in pattern, use path.Match directly.
	if !strings.Contains(pattern, "**") {
		matched, _ := path.Match(pattern, name)
		return matched
	}

	// Split pattern on "**" and match prefix/suffix.
	prefix, suffix, _ := strings.Cut(pattern, "**")

	// Remove trailing slash from prefix, leading slash from suffix.
	prefix = strings.TrimSuffix(prefix, "/")
	suffix = strings.TrimPrefix(suffix, "/")

	if prefix == "" && suffix == "" {
		// Pattern is just "**" — matches everything.
		return true
	}

	if prefix == "" {
		// "**/<suffix>" — suffix must match the end of some sub-path.
		parts := strings.Split(name, "/")
		for i := range parts {
			tail := strings.Join(parts[i:], "/")
			if globMatch(suffix, tail) {
				return true
			}
		}
		return false
	}

	if suffix == "" {
		// "<prefix>/**" — prefix must match the start of the path.
		parts := strings.Split(name, "/")
		for i := 1; i <= len(parts); i++ {
			head := strings.Join(parts[:i], "/")
			matched, _ := path.Match(prefix, head)
			if matched {
				return true
			}
		}
		return false
	}

	// "<prefix>/**/<suffix>" — prefix matches start, suffix matches remainder.
	parts := strings.Split(name, "/")
	for i := 1; i <= len(parts); i++ {
		head := strings.Join(parts[:i], "/")
		matched, _ := path.Match(prefix, head)
		if matched {
			// Try suffix against every possible tail from this point.
			for j := i; j <= len(parts); j++ {
				tail := strings.Join(parts[j:], "/")
				if globMatch(suffix, tail) {
					return true
				}
			}
		}
	}
	return false
}
