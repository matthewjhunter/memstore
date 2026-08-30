package memstore

import "time"

// Corpus lint: the mechanical half of the wiki organizer.
//
// docs/document-synthesis.md gives lint four checks. Two of them need the
// document corpus (derived facts whose citations are gone, subjects with
// documents but no synthesis fact) and wait for it. The checks here need
// nothing but the fact graph, and they are the ones that address what a
// 2026-08-29 audit of the live corpus actually found: 3,600 of 4,846 facts
// had never surfaced by any signal, 2,700 carried a subject that was not an
// entity name, and hand review turned up exact duplicate pairs.
//
// Lint reports and never writes. It produces a queue for a person to work
// through, which is the design's rule and also the safe one: every finding
// here is a heuristic, and a fact that has never been retrieved is not
// thereby wrong.

// LintKind names a check. One fact can appear under several.
type LintKind string

const (
	// LintOrphan: no links in or out. The fact is in the store but not in
	// the graph, so nothing reaches it by traversal.
	LintOrphan LintKind = "orphan"
	// LintNeverSurfaced: never searched, never injected, never confirmed,
	// and old enough that it has had the chance. Suspicion, not a verdict.
	LintNeverSurfaced LintKind = "never-surfaced"
	// LintOddSubject: the subject is not a lowercase entity name. In the
	// audited corpus this reliably marked extraction artifacts whose subject
	// was an event category ("Version control action", "PR branch").
	LintOddSubject LintKind = "odd-subject"
	// LintMissingSubject: no subject at all. Separate from LintOddSubject
	// because the cause is different and so is the fix: the V4 identity
	// migration blanked subjects that only carried ownership, so these are
	// facts waiting for a topic rather than facts with a bad one.
	LintMissingSubject LintKind = "missing-subject"
	// LintDuplicate: content identical to an earlier live fact.
	LintDuplicate LintKind = "duplicate"
)

// LintKinds is every check, in report order: cheapest and most certain
// first, most heuristic last.
var LintKinds = []LintKind{LintDuplicate, LintMissingSubject, LintOrphan, LintOddSubject, LintNeverSurfaced}

// SubjectPattern is the convention memory_store documents: a lowercase
// singular entity name. Dots, slashes and colons are allowed because real
// subjects carry them -- "speculativefiction.org", "infodancer/oidclient",
// "gemma3:12b" -- so what this actually catches is capitals and spaces,
// which is what the artifacts had.
const SubjectPattern = `^[a-z0-9][a-z0-9._/:-]*$`

// LintFinding is one flagged fact.
type LintFinding struct {
	Kind      LintKind
	FactID    int64
	Subject   string
	Detail    string // why it was flagged, when the kind alone does not say
	Content   string // truncated for display
	CreatedAt time.Time
}

// LintOpts tunes the run.
type LintOpts struct {
	// MinAge excludes facts younger than this from LintNeverSurfaced; a
	// fact stored an hour ago has not had its chance yet. Zero means the
	// caller wants no age floor.
	MinAge time.Duration
	// Kinds limits the run; empty means every check.
	Kinds []LintKind
	// SampleLimit caps the findings *listed* per kind. Counts are always
	// complete: a check that matches 3,600 facts must report 3,600 and show
	// ten, not report ten.
	SampleLimit int
}

// LintReport is a run's outcome. Counts are over the whole corpus; Findings
// is the sample.
type LintReport struct {
	Active   int
	Counts   map[LintKind]int
	Findings []LintFinding
}

// Count is Counts[k] with the zero value for a check that did not run.
func (r LintReport) Count(k LintKind) int { return r.Counts[k] }

// Total is every flag raised, which exceeds the number of facts flagged
// when one fact trips several checks.
func (r LintReport) Total() int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
}

// WantKind reports whether opts asks for this check. Exported for the
// store implementations, which run the checks.
func (o LintOpts) WantKind(k LintKind) bool {
	if len(o.Kinds) == 0 {
		return true
	}
	for _, want := range o.Kinds {
		if want == k {
			return true
		}
	}
	return false
}
