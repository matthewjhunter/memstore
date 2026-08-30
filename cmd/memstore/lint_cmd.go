package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// runLint reports the model-free corpus checks (see memstore.LintKind). It
// is the mechanical half of the wiki organizer in docs/document-synthesis.md:
// lint produces a queue for a person to work through and never writes a
// fact, because every check here is a heuristic. A fact nothing has
// retrieved is suspicious, not wrong.
func runLint(args []string, out io.Writer) {
	fs := flag.NewFlagSet("lint", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	kinds := fs.String("kind", "", "comma-separated checks to run (default all): "+kindNames())
	minAge := fs.Duration("min-age", 30*24*time.Hour, "never-surfaced ignores facts younger than this")
	sample := fs.Int("sample", 10, "findings to list per check (0 = all); counts are always complete")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}
	opts := memstore.LintOpts{MinAge: *minAge, SampleLimit: *sample}
	if *kinds != "" {
		for _, k := range strings.Split(*kinds, ",") {
			k = strings.TrimSpace(k)
			if !validLintKind(k) {
				fail(fmt.Errorf("lint: unknown check %q; want one of %s", k, kindNames()))
			}
			opts.Kinds = append(opts.Kinds, memstore.LintKind(k))
		}
	}
	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	rep, err := pgstore.Lint(context.Background(), pool, *namespace, opts)
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, lintReport(rep, opts))
}

func kindNames() string {
	names := make([]string, len(memstore.LintKinds))
	for i, k := range memstore.LintKinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

func validLintKind(s string) bool {
	for _, k := range memstore.LintKinds {
		if string(k) == s {
			return true
		}
	}
	return false
}

// lintDescriptions say what a count means, since "orphan: 412" on its own
// invites the reader to assume the worst about 412 facts.
var lintDescriptions = map[memstore.LintKind]string{
	memstore.LintDuplicate:      "content identical to an earlier live fact",
	memstore.LintOrphan:         "no links in or out; unreachable by traversal",
	memstore.LintOddSubject:     "subject is not a lowercase entity name",
	memstore.LintMissingSubject: "no subject at all (the V4 migration blanked ownership-only subjects)",
	memstore.LintNeverSurfaced:  "never searched, injected, or confirmed",
}

func lintReport(rep memstore.LintReport, opts memstore.LintOpts) string {
	s := fmt.Sprintf("memstore lint: %d active facts\n\n", rep.Active)
	if rep.Total() == 0 {
		return s + "  nothing flagged\n"
	}
	for _, k := range memstore.LintKinds {
		n, ran := rep.Counts[k]
		if !ran {
			continue
		}
		pct := ""
		if rep.Active > 0 {
			pct = fmt.Sprintf(" (%.0f%%)", 100*float64(n)/float64(rep.Active))
		}
		s += fmt.Sprintf("  %-15s %6d%-6s %s\n", k, n, pct, lintDescriptions[k])
	}
	s += "\n"
	for _, k := range memstore.LintKinds {
		shown := findingsOfKind(rep.Findings, k)
		if len(shown) == 0 {
			continue
		}
		s += fmt.Sprintf("%s -- %s\n", k, lintDescriptions[k])
		for _, f := range shown {
			s += fmt.Sprintf("  id=%-6d %s  subject=%q\n    %s\n",
				f.FactID, f.CreatedAt.Format("2006-01-02"), f.Subject, f.Content)
		}
		if n := rep.Counts[k]; n > len(shown) {
			s += fmt.Sprintf("  ... and %d more (--sample 0 for all)\n", n-len(shown))
		}
		s += "\n"
	}
	s += "Lint reports; it never edits or deletes. Every check is a heuristic --\n"
	s += "a fact nothing has retrieved is suspicious, not wrong.\n"
	return s
}

func findingsOfKind(all []memstore.LintFinding, k memstore.LintKind) []memstore.LintFinding {
	var out []memstore.LintFinding
	for _, f := range all {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}
