package memstore_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/matthewjhunter/memstore"
)

func TestTaskTitle(t *testing.T) {
	long := strings.Repeat("word ", 60)
	cases := []struct {
		name, in, want string
	}{
		{"cut at double dash",
			"memstore wiki-organizer: subject assignment -- the real fix for subject proliferation, which normalization did not touch.",
			"memstore wiki-organizer: subject assignment"},
		{"cut at em dash",
			"Evaluate whether memstore should use a network database (e.g. PostgreSQL) instead of SQLite \u2014 consider multi-machine access",
			"Evaluate whether memstore should use a network database (e.g. PostgreSQL) instead of SQLite"},
		{"first sentence after a parenthesis",
			"Finish the OSS polish pass on memstore (narrowed 2026-08-29; supersedes 4098). Verified already in place: LICENSE.",
			"Finish the OSS polish pass on memstore (narrowed 2026-08-29; supersedes 4098)."},
		{"first sentence",
			"Extract a shared web-extraction module so herald and memstore stop reinventing it. Proposed 2026-08-30.",
			"Extract a shared web-extraction module so herald and memstore stop reinventing it."},
		{"abbreviation is not a sentence end",
			"Pick a store (e.g. Postgres) for the daemon",
			"Pick a store (e.g. Postgres) for the daemon"},
		{"first line", "  line one\nline two", "line one"},
		{"empty", "   ", ""},
	}
	for _, c := range cases {
		if got := memstore.TaskTitle(c.in); got != c.want {
			t.Errorf("%s: TaskTitle = %q, want %q", c.name, got, c.want)
		}
	}

	got := memstore.TaskTitle(long)
	if n := utf8.RuneCountInString(got); n > memstore.TaskTitleMax+3 {
		t.Errorf("long title is %d runes, want at most %d", n, memstore.TaskTitleMax+3)
	}
	if !strings.HasSuffix(got, "word...") {
		t.Errorf("long title should end on a whole word plus an ellipsis: %q", got)
	}
}

func TestProjectAliasesFromCWD(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "git", "old-school-gamers", "osg")
	sub := filepath.Join(repo, "cmd", "tool")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := memstore.ProjectAliasesFromCWD(sub); len(got) != 1 || got[0] != "old-school-gamers" {
		t.Errorf("aliases inside a repo = %v, want [old-school-gamers]", got)
	}
	// Outside a repo there is no account directory to speak of: the parent of
	// a working area like ~/job-search is the home directory.
	plain := filepath.Join(root, "job-search")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := memstore.ProjectAliasesFromCWD(plain); len(got) != 0 {
		t.Errorf("aliases outside a repo = %v, want none", got)
	}
	if got := memstore.ProjectAliasesFromCWD(""); len(got) != 0 {
		t.Errorf("aliases for empty cwd = %v, want none", got)
	}
}

func TestTaskContextMatchesProject(t *testing.T) {
	tc := memstore.TaskContext{Project: "osg", Aliases: []string{"old-school-gamers"}}
	for p, want := range map[string]bool{
		"osg":               true,
		"OSG":               true,
		"oldschoolgamers":   true,
		"old_school_gamers": true,
		"Old-School-Gamers": true,
		"herald":            false,
		"":                  false,
	} {
		if got := tc.MatchesProject(p); got != want {
			t.Errorf("MatchesProject(%q) = %v, want %v", p, got, want)
		}
	}
	if (memstore.TaskContext{}).MatchesProject("anything") {
		t.Error("a context with no project matched a task")
	}
}

// The alias has to reach the ranking too, or a project-only list would hold
// the right tasks in the wrong order.
func TestHeuristicScoreUsesAliases(t *testing.T) {
	tc := memstore.TaskContext{Project: "osg", Aliases: []string{"old-school-gamers"}}
	meta, _ := json.Marshal(map[string]string{"project": "oldschoolgamers", "priority": "normal"})
	aliased := memstore.HeuristicScore(memstore.ParseTaskMeta(memstore.Fact{Metadata: meta}), tc)
	meta, _ = json.Marshal(map[string]string{"project": "herald", "priority": "high"})
	foreign := memstore.HeuristicScore(memstore.ParseTaskMeta(memstore.Fact{Metadata: meta}), tc)
	if aliased <= foreign {
		t.Errorf("aliased-project task scored %d, foreign high-priority task %d", aliased, foreign)
	}
}
