package main

import (
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// The report has to keep count and sample distinct. Showing ten findings and
// letting the reader infer that is the total is how a 3,600-item backlog
// stays invisible, which is the failure this command exists to expose.
func TestLintReport_CountsAndSampleAreDistinct(t *testing.T) {
	rep := memstore.LintReport{
		Active: 4846,
		Counts: map[memstore.LintKind]int{memstore.LintNeverSurfaced: 3598},
		Findings: []memstore.LintFinding{
			{Kind: memstore.LintNeverSurfaced, FactID: 1, Subject: "a", Content: "x", CreatedAt: time.Now()},
			{Kind: memstore.LintNeverSurfaced, FactID: 2, Subject: "b", Content: "y", CreatedAt: time.Now()},
		},
	}
	out := lintReport(rep, memstore.LintOpts{SampleLimit: 2})
	for _, want := range []string{
		"4846 active facts",
		"never-surfaced    3598",
		"and 3596 more",
		"never edits or deletes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// A clean corpus should say so plainly rather than printing empty headings.
func TestLintReport_Clean(t *testing.T) {
	out := lintReport(memstore.LintReport{Active: 12, Counts: map[memstore.LintKind]int{
		memstore.LintOrphan: 0,
	}}, memstore.LintOpts{})
	if !strings.Contains(out, "nothing flagged") {
		t.Errorf("report = %q", out)
	}
}

// A check that did not run must not be reported as a zero: "orphan 0" when
// orphan was filtered out reads as "no orphans", which is a different claim.
func TestLintReport_OmitsChecksThatDidNotRun(t *testing.T) {
	rep := memstore.LintReport{
		Active: 10,
		Counts: map[memstore.LintKind]int{memstore.LintDuplicate: 2},
		Findings: []memstore.LintFinding{
			{Kind: memstore.LintDuplicate, FactID: 5, Subject: "s", Content: "dupe", CreatedAt: time.Now()},
		},
	}
	out := lintReport(rep, memstore.LintOpts{Kinds: []memstore.LintKind{memstore.LintDuplicate}})
	if strings.Contains(out, "orphan") || strings.Contains(out, "never-surfaced") {
		t.Errorf("report names checks that did not run:\n%s", out)
	}
	if !strings.Contains(out, "duplicate") {
		t.Errorf("report omits the check that did run:\n%s", out)
	}
}
