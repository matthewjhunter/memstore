package memstore

import (
	"regexp"
	"strings"
)

// SubjectPattern is the convention memory_store documents: a lowercase
// singular entity name. Dots, slashes and colons are allowed because real
// subjects carry them -- "speculativefiction.org", "infodancer/oidclient",
// "gemma3:12b" -- and a leading dot or slash because paths and dotfiles are
// entity names too ("/etc/fstab", ".litcoffee"). What it catches is capitals
// and spaces, which is what the extraction artifacts had.
const SubjectPattern = `^[a-z0-9./][a-z0-9._/:-]*$`

var (
	subjectValid   = regexp.MustCompile(SubjectPattern)
	subjectStrip   = regexp.MustCompile(`[^a-z0-9._/:-]+`)
	subjectDashRun = regexp.MustCompile(`-{2,}`)
)

// ValidSubject reports whether s already follows the convention. An empty
// subject is not valid, but it is a different defect -- see LintMissingSubject.
func ValidSubject(s string) bool { return subjectValid.MatchString(s) }

// SlugifySubject rewrites a subject into the convention: lowercased, with any
// run of characters the convention does not allow collapsed to a single
// hyphen, and hyphens trimmed from both ends. It returns "" when nothing
// usable is left, which the caller must treat as "leave this one alone"
// rather than storing an empty subject.
//
// It is deliberately mechanical and idempotent. It fixes the *shape* of a
// subject and nothing else: "Version control action" becomes
// "version-control-action", which is well-formed and still not an entity
// name. Deciding what a fact is actually about is a separate problem and not
// one a regexp gets to answer.
func SlugifySubject(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	s = subjectStrip.ReplaceAllString(s, "-")
	s = subjectDashRun.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" || !subjectValid.MatchString(s) {
		return ""
	}
	return s
}

// NormalizeStoredSubject is what the store applies to every subject on the
// way in, making "every stored subject follows the convention" an invariant
// rather than an aspiration -- which is what lets the odd-subject lint check
// go from empty to structurally impossible.
//
// It is deliberately forgiving rather than strict. A subject that cannot be
// slugified into anything (punctuation only) is stored unchanged instead of
// blanked: trading a malformed subject for a missing one is not an
// improvement, and LintOddSubject will still surface it. An empty subject
// stays empty, which is a different defect with its own check.
//
// Normalizing here rather than at each caller means it also covers import.
// That is intended: a fact arriving from an older store came out of the same
// pipeline that produced the mess, and the destination store's invariant is
// the one that should hold.
func NormalizeStoredSubject(s string) string {
	if s == "" || ValidSubject(s) {
		return s
	}
	if slug := SlugifySubject(s); slug != "" {
		return slug
	}
	return s
}
