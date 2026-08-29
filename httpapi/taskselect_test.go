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
