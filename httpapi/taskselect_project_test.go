package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// TestTaskSelect_ProjectOnly: the startup list is this project's tasks and
// nothing else. Ranking the repo first and filling the remaining slots from
// other projects put foreign work into unrelated sessions, the same failure
// #221 describes for hints.
func TestTaskSelect_ProjectOnly(t *testing.T) {
	h, store := newTestHandler(t)
	seedTask(t, store, "herald: rotate the feed key", "herald", "high", "pending")
	aliased := seedTask(t, store, "osg: write up the session", "oldschoolgamers", "normal", "pending")
	direct := seedTask(t, store, "osg: fix the dice parser", "osg", "high", "pending")

	var got memstore.TaskSelectResponse
	resp := doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{
		"project": "osg", "aliases": []string{"old-school-gamers"},
		"project_only": true, "limit": 5, "surface": "startup",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	decodeJSON(t, resp, &got)
	if got.Total != 2 {
		t.Errorf("total = %d, want 2: only this project's tasks are eligible", got.Total)
	}
	if ids := taskIDs(got.Tasks); len(ids) != 2 || ids[0] != direct || ids[1] != aliased {
		t.Errorf("tasks = %v, want [%d %d] (high first; the alias counts as this project)", ids, direct, aliased)
	}
	// The echo is how a client tells this daemon from one that ignored the
	// field and returned every project's tasks with every project's total.
	if !got.ProjectOnly {
		t.Error("response does not confirm project_only was applied")
	}

	// Without project_only the old behaviour stands: this project first, then
	// everything else.
	got = memstore.TaskSelectResponse{}
	resp = doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{
		"project": "osg", "aliases": []string{"old-school-gamers"}, "limit": 5,
	})
	decodeJSON(t, resp, &got)
	if len(got.Tasks) != 3 {
		t.Errorf("without project_only: %d tasks, want 3", len(got.Tasks))
	}
	if got.ProjectOnly {
		t.Error("response claims project_only when it was not asked for")
	}

	// project_only with nothing to match on is an empty list, not everything.
	resp = doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"project_only": true, "limit": 5})
	decodeJSON(t, resp, &got)
	if len(got.Tasks) != 0 || got.Total != 0 {
		t.Errorf("project_only without a project: %d tasks of %d, want none", len(got.Tasks), got.Total)
	}
}
