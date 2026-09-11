package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/fence"
)

// claimTimeout bounds the call that records what a hook is about to show. The
// hooks give the whole command 3-4s; past this the context is shown unrecorded
// rather than lost to the hook's timeout.
const claimTimeout = 1500 * time.Millisecond

func runEvalTriggers(args []string) {
	fs := flag.NewFlagSet("eval-triggers", flag.ExitOnError)
	filePath := fs.String("file", "", "absolute file path to evaluate triggers against (required)")
	session := fs.String("session", "", "session id: record the facts shown against it, and leave out those it was already shown")
	format := fs.String("format", "text", "output format: text|hook (hook: JSON with the block and a notice for the user)")
	fs.Parse(args)

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "eval-triggers: --file is required")
		os.Exit(1)
	}

	store, closeStore, err := openStore()
	if err != nil {
		log.Fatal(err)
	}
	defer closeStore()

	ctx := context.Background()
	groups, err := loadTriggerContext(ctx, store, *filePath)
	if err != nil {
		log.Fatalf("eval-triggers: %v", err)
	}
	claimer, _ := store.(memstore.InjectionClaimer)
	groups = claimTriggerContext(ctx, claimer, *session, groups)
	if *format != "hook" {
		if err := writeTriggerContext(os.Stdout, *filePath, groups); err != nil {
			log.Fatalf("eval-triggers: %v", err)
		}
		return
	}
	var block strings.Builder
	if err := writeTriggerContext(&block, *filePath, groups); err != nil {
		log.Fatalf("eval-triggers: %v", err)
	}
	if err := writeHookOutput(os.Stdout, block.String(), triggerNotice(*filePath, groups), cliConfig.HookNotices); err != nil {
		log.Fatalf("eval-triggers: %v", err)
	}
}

// triggerGroup is one matched trigger and the facts it loads.
type triggerGroup struct {
	trigger memstore.Fact
	facts   []memstore.Fact
}

// triggerLoad is what a matched trigger's metadata asks to load.
type triggerLoad struct {
	subsystem, subject string
	kinds              []string
}

// matchTrigger reports whether a file-pattern trigger matches filePath, and
// what it loads if so.
func matchTrigger(t memstore.Fact, filePath string) (triggerLoad, bool) {
	var meta map[string]any
	if len(t.Metadata) == 0 || json.Unmarshal(t.Metadata, &meta) != nil {
		return triggerLoad{}, false
	}
	signal, _ := meta["signal"].(string)
	if signal == "" || !memstore.MatchFilePattern(signal, filePath) {
		return triggerLoad{}, false
	}
	var l triggerLoad
	l.subsystem, _ = meta["load_subsystem"].(string)
	l.subject, _ = meta["load_subject"].(string)
	if raw, ok := meta["load_kinds"].([]any); ok {
		for _, k := range raw {
			if s, ok := k.(string); ok {
				l.kinds = append(l.kinds, s)
			}
		}
	}
	return l, true
}

// loadTriggerContext finds the file-pattern triggers matching filePath and
// loads each one's facts. A fact appears once, under the first trigger that
// loads it, and a matched trigger is never listed as a fact: a trigger for a
// subsystem is itself a fact in that subsystem.
func loadTriggerContext(ctx context.Context, store memstore.Store, filePath string) ([]triggerGroup, error) {
	triggers, err := store.List(ctx, memstore.QueryOpts{
		Kind:       "trigger",
		OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "signal_type", Op: "=", Value: "file_pattern"},
		},
	})
	if err != nil {
		return nil, err
	}

	var groups []triggerGroup
	var loads []triggerLoad
	seen := make(map[int64]bool)
	for _, t := range triggers {
		if l, ok := matchTrigger(t, filePath); ok {
			groups = append(groups, triggerGroup{trigger: t})
			loads = append(loads, l)
			seen[t.ID] = true
		}
	}

	for i, l := range loads {
		if l.subsystem == "" && l.subject == "" {
			continue
		}
		// QueryOpts takes one Kind, so each requested kind is its own query;
		// no kinds means one query with no kind filter.
		kinds := l.kinds
		if len(kinds) == 0 {
			kinds = []string{""}
		}
		for _, kind := range kinds {
			facts, err := store.List(ctx, memstore.QueryOpts{
				Subsystem:  l.subsystem,
				Subject:    l.subject,
				Kind:       kind,
				OnlyActive: true,
			})
			if err != nil {
				continue
			}
			for _, f := range facts {
				if !seen[f.ID] {
					seen[f.ID] = true
					groups[i].facts = append(groups[i].facts, f)
				}
			}
		}
	}
	return groups, nil
}

// claimTriggerContext records what the file context is about to show against
// the session, on the file-trigger channel, and keeps only what no channel has
// shown the session yet. With no session or claimer, or when the claim fails
// (a daemon older than the endpoint, a timeout), everything is shown and
// nothing recorded, so a failure repeats facts instead of dropping them.
func claimTriggerContext(ctx context.Context, c memstore.InjectionClaimer, session string, groups []triggerGroup) []triggerGroup {
	if c == nil || session == "" || len(groups) == 0 {
		return groups
	}
	var ids []string
	for _, g := range groups {
		ids = append(ids, strconv.FormatInt(g.trigger.ID, 10))
		for _, f := range g.facts {
			ids = append(ids, strconv.FormatInt(f.ID, 10))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()
	fresh, err := c.ClaimInjections(ctx, session, memstore.ChannelFileTrigger, memstore.RefTypeFact, ids)
	if err != nil {
		return groups
	}
	isNew := make(map[int64]bool, len(fresh))
	for _, s := range fresh {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			isNew[id] = true
		}
	}
	return keepNew(groups, isNew)
}

// keepNew keeps a trigger while it or any fact it loads is new, and under it
// only the new facts. The trigger is repeated with a new fact so that no fact
// arrives without the rule that loaded it.
func keepNew(groups []triggerGroup, isNew map[int64]bool) []triggerGroup {
	var out []triggerGroup
	for _, g := range groups {
		var facts []memstore.Fact
		for _, f := range g.facts {
			if isNew[f.ID] {
				facts = append(facts, f)
			}
		}
		if len(facts) == 0 && !isNew[g.trigger.ID] {
			continue
		}
		out = append(out, triggerGroup{trigger: g.trigger, facts: facts})
	}
	return out
}

// writeTriggerContext renders the file context the Read and Edit hooks inject:
// memstore's heading and the fence preamble, then each trigger and its facts.
// The ids stay outside the fence, where the citation convention looks for
// them. Every stored value -- trigger text, fact bodies, the fields on each
// label line -- is fenced or neutralized: this reaches the model on every Read
// and Edit of a matching file, the same exposure #219 closed for hints.
func writeTriggerContext(w io.Writer, filePath string, groups []triggerGroup) error {
	if len(groups) == 0 {
		return nil
	}
	fnc, err := fence.New()
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[trigger context for: %s]\n", fnc.Inline(filePath))
	b.WriteString(fnc.Preamble())
	for _, g := range groups {
		fmt.Fprintf(&b, "\n--- trigger [id=%d] ---\n%s\n", g.trigger.ID, fnc.Indent(g.trigger.Content, "  "))
		for _, f := range g.facts {
			fmt.Fprintf(&b, "\n[id=%d] %s | %s", f.ID, fnc.Inline(f.Subject), fnc.Inline(f.Category))
			if f.Kind != "" {
				fmt.Fprintf(&b, " | kind=%s", fnc.Inline(f.Kind))
			}
			if f.Subsystem != "" {
				fmt.Fprintf(&b, " | subsystem=%s", fnc.Inline(f.Subsystem))
			}
			fmt.Fprintf(&b, "\n%s\n", fnc.Indent(f.Content, "  "))
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}
