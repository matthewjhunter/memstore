package memstore_test

import (
	"regexp"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// Whatever SlugifySubject returns must satisfy SubjectPattern, or normalizing
// would hand the corpus a subject the linter turns around and flags.
func TestSlugifySubject(t *testing.T) {
	valid := regexp.MustCompile(memstore.SubjectPattern)
	cases := []struct{ in, want string }{
		{"Falkenstein Castle", "falkenstein-castle"},
		{"Version control action", "version-control-action"},
		{"AI Agents", "ai-agents"},
		{"BIORce Role", "biorce-role"},
		{"OSG Session Notes Skill", "osg-session-notes-skill"},
		{"`extractLinkPostURL` function", "extractlinkposturl-function"},
		{"  padded  ", "padded"},
		{"Multiple   Spaces", "multiple-spaces"},
		{"Trailing -- dashes --", "trailing-dashes"},

		// Already conventional subjects are returned untouched, so a
		// normalize pass is a no-op on the part of the corpus that is fine.
		{"memstore", "memstore"},
		{"jane-austen", "jane-austen"},
		{"speculativefiction.org", "speculativefiction.org"},
		{"infodancer/oidclient", "infodancer/oidclient"},
		{"gemma3:12b", "gemma3:12b"},
		{"/proc", "/proc"},
		{".litcoffee", ".litcoffee"},

		// Nothing usable left: the caller must leave these alone rather than
		// store an empty subject.
		{"", ""},
		{"---", ""},
		{"!!!", ""},
	}
	for _, c := range cases {
		got := memstore.SlugifySubject(c.in)
		if got != c.want {
			t.Errorf("SlugifySubject(%q) = %q, want %q", c.in, got, c.want)
		}
		if got != "" && !valid.MatchString(got) {
			t.Errorf("SlugifySubject(%q) = %q, which SubjectPattern rejects", c.in, got)
		}
	}
}

// Slugifying is idempotent: running normalize twice must not keep changing
// subjects, or every run would look like it had work to do.
func TestSlugifySubject_Idempotent(t *testing.T) {
	for _, s := range []string{"Falkenstein Castle", "AI Agents", "/etc/fstab", "gemma3:12b", "`x` function"} {
		once := memstore.SlugifySubject(s)
		if twice := memstore.SlugifySubject(once); twice != once {
			t.Errorf("SlugifySubject(%q) = %q, then %q -- not idempotent", s, once, twice)
		}
	}
}

// Paths and dotfiles are real entity names. Flagging /proc and .litcoffee as
// malformed sent the linter after five facts that were correctly subjected.
func TestSubjectPattern_AllowsPathsAndDotfiles(t *testing.T) {
	valid := regexp.MustCompile(memstore.SubjectPattern)
	for _, s := range []string{"/proc", "/etc/fstab", ".litcoffee", ".bashrc"} {
		if !valid.MatchString(s) {
			t.Errorf("SubjectPattern rejects %q, which is a legitimate entity name", s)
		}
	}
	for _, s := range []string{"PR branch", "Candidate", "-leading-dash", "has space"} {
		if valid.MatchString(s) {
			t.Errorf("SubjectPattern accepts %q", s)
		}
	}
}

// The store normalizes on write, so the invariant is "every stored subject
// follows the convention". That only works if lookups normalize too --
// otherwise storing "Matthew" and then querying subject=Matthew silently
// returns nothing, which is worse than the malformed subject was.
func TestNormalizeStoredSubject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Matthew", "matthew"},
		{"Falkenstein Castle", "falkenstein-castle"},
		{"memstore", "memstore"},
		{"/etc/fstab", "/etc/fstab"},

		// Empty stays empty: that is LintMissingSubject's business, and
		// inventing a subject here would hide it.
		{"", ""},

		// Nothing salvageable is stored unchanged rather than blanked --
		// trading a malformed subject for a missing one is not an
		// improvement, and lint still surfaces it.
		{"!!!", "!!!"},
		{"---", "---"},
	}
	for _, c := range cases {
		if got := memstore.NormalizeStoredSubject(c.in); got != c.want {
			t.Errorf("NormalizeStoredSubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
