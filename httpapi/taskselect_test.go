package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/matthewjhunter/memstore"
)

func seedTask(t *testing.T, store interface {
	Insert(context.Context, memstore.Fact) (int64, error)
}, content, project, priority, status string) int64 {
	t.Helper()
	meta, _ := json.Marshal(map[string]string{"kind": "task", "project": project, "priority": priority, "status": status, "surface": "startup", "scope": "claude"})
	id, err := store.Insert(context.Background(), memstore.Fact{Content: content, Subject: "todo", Category: "note", Kind: "task", Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestTaskSelect_TopNForThisRepo: the endpoint returns the few tasks a
// session should see first -- this repo's, then by priority -- and says how
// many were eligible, so five are never mistaken for the whole backlog.
func TestTaskSelect_TopNForThisRepo(t *testing.T) {
	h, store := newTestHandler(t)
	other := seedTask(t, store, "herald: rotate the feed key", "herald", "high", "pending")
	low := seedTask(t, store, "memstore: tidy the changelog", "memstore", "low", "pending")
	high := seedTask(t, store, "memstore: fix recall auth", "memstore", "high", "pending")
	prog := seedTask(t, store, "memstore: finish the deletion", "memstore", "normal", "in_progress")
	_ = other
	_ = low

	var got memstore.TaskSelectResponse
	resp := doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"cwd": "/home/m/git/memstore", "limit": 2, "surface": "startup"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	decodeJSON(t, resp, &got)
	if got.Total != 4 {
		t.Errorf("total = %d, want 4 eligible", got.Total)
	}
	if len(got.Tasks) != 2 || got.Tasks[0].ID != prog || got.Tasks[1].ID != high {
		t.Errorf("tasks = %v, want [%d %d] (in progress, then high, both this repo)", taskIDs(got.Tasks), prog, high)
	}
	if got.Selector != memstore.TaskSelectorHeuristic {
		t.Errorf("selector = %q", got.Selector)
	}

	// A negative limit is a caller error; a zero limit is the whole list in
	// selection order.
	if r := doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"limit": -1}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("negative limit: status %d, want 400", r.StatusCode)
	}
	resp = doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"project": "memstore"})
	decodeJSON(t, resp, &got)
	if len(got.Tasks) != 4 || got.Tasks[3].ID != other {
		t.Errorf("limit 0 = %v, want all four with the other repo's task last", taskIDs(got.Tasks))
	}
}

func taskIDs(fs []memstore.Fact) []int64 {
	out := make([]int64, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}

// TestTaskSelect_ExcludesClosedByDefault: a request with no status must not
// see completed or cancelled work. The selector only ever *boosts*
// in_progress, so before this a finished high-priority task outranked every
// real one and owned the top of the session's list.
func TestTaskSelect_ExcludesClosedByDefault(t *testing.T) {
	h, store := newTestHandler(t)
	done := seedTask(t, store, "memstore: ship the hook auth fix", "memstore", "high", "completed")
	killed := seedTask(t, store, "memstore: rewrite it in rust", "memstore", "high", "cancelled")
	open := seedTask(t, store, "memstore: retune the link gate", "memstore", "normal", "pending")

	var got memstore.TaskSelectResponse
	resp := doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"project": "memstore"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	decodeJSON(t, resp, &got)
	if got.Total != 1 || len(got.Tasks) != 1 || got.Tasks[0].ID != open {
		t.Errorf("default = %v (total %d), want only the open task %d, not %d/%d",
			taskIDs(got.Tasks), got.Total, open, done, killed)
	}

	// Asking for a closed status still works, and "all" gets everything.
	resp = doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"status": "completed"})
	decodeJSON(t, resp, &got)
	if len(got.Tasks) != 1 || got.Tasks[0].ID != done {
		t.Errorf("status=completed = %v, want [%d]", taskIDs(got.Tasks), done)
	}
	resp = doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"status": memstore.TaskStatusAll})
	decodeJSON(t, resp, &got)
	if got.Total != 3 {
		t.Errorf("status=all total = %d, want 3", got.Total)
	}
}

// A task carrying no status at all is unstarted work, not closed work: it
// must survive the default filter rather than disappear from every list.
func TestTaskSelect_KeepsTasksWithNoStatus(t *testing.T) {
	h, store := newTestHandler(t)
	bare := seedTask(t, store, "memstore: the one nobody set a status on", "memstore", "normal", "")

	var got memstore.TaskSelectResponse
	decodeJSON(t, doJSON(t, h, "POST", "/v1/tasks/select", map[string]any{"project": "memstore"}), &got)
	if len(got.Tasks) != 1 || got.Tasks[0].ID != bare {
		t.Errorf("tasks = %v, want the status-less task %d", taskIDs(got.Tasks), bare)
	}
}
