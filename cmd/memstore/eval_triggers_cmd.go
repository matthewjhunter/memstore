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
		if !memstore.MatchFilePattern(signal, *filePath) {
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
