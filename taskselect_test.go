package memstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

func task(id int64, content, project, priority, status string, age time.Duration) memstore.Fact {
	meta, _ := json.Marshal(map[string]string{"kind": "task", "project": project, "priority": priority, "status": status})
	return memstore.Fact{ID: id, Content: content, Metadata: meta, CreatedAt: time.Now().Add(-age)}
}

func ids(fs []memstore.Fact) []int64 {
	out := make([]int64, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}

func TestHeuristicSelector_BucketsThenRecency(t *testing.T) {
	tasks := []memstore.Fact{
		task(1, "other repo, high", "herald", "high", "pending", time.Hour),
		task(2, "this repo, low", "memstore", "low", "pending", time.Hour),
		task(3, "this repo, normal, older", "memstore", "normal", "pending", 48*time.Hour),
		task(4, "this repo, normal, newer", "memstore", "normal", "pending", time.Hour),
		task(5, "this repo, in progress, low", "memstore", "low", "in_progress", 72*time.Hour),
		task(6, "no project, high", "", "high", "pending", time.Minute),
		task(7, "this repo, high", "memstore", "high", "pending", 24*time.Hour),
	}
	got, err := memstore.HeuristicSelector{}.SelectTasks(context.Background(), tasks, memstore.TaskContext{Project: "memstore"})
	if err != nil {
		t.Fatal(err)
	}
	// repo match first; inside it in_progress, then high, then normal by
	// recency, then low; then the rest by priority, newest first.
	want := []int64{5, 7, 4, 3, 2, 6, 1}
	if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", ids(got), want)
	}
}

func TestHeuristicSelector_LimitAndNoProject(t *testing.T) {
	tasks := []memstore.Fact{
		task(1, "a", "x", "normal", "pending", time.Hour),
		task(2, "b", "y", "high", "pending", time.Hour),
		task(3, "c", "z", "low", "pending", time.Hour),
	}
	got, _ := memstore.HeuristicSelector{}.SelectTasks(context.Background(), tasks, memstore.TaskContext{Limit: 2})
	if fmt.Sprint(ids(got)) != "[2 1]" {
		t.Errorf("no-project limit 2 = %v, want [2 1]", ids(got))
	}
	all, _ := memstore.HeuristicSelector{}.SelectTasks(context.Background(), tasks, memstore.TaskContext{})
	if len(all) != 3 {
		t.Errorf("limit 0 returned %d, want all 3", len(all))
	}
}

// scriptedTaskReranker scores a document by keyword.
type scriptedTaskReranker struct {
	scores map[string]float64
	query  string
	calls  int
	err    error
}

func (r *scriptedTaskReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	r.calls++
	r.query = req.Query
	if r.err != nil {
		return nil, r.err
	}
	out := make([]embedding.RerankResult, len(req.Documents))
	for i, d := range req.Documents {
		var s float64
		for k, v := range r.scores {
			if strings.Contains(d, k) && v > s {
				s = v
			}
		}
		out[i] = embedding.RerankResult{Index: i, Score: s}
	}
	return out, nil
}
func (r *scriptedTaskReranker) Model() string { return "scripted" }

func TestRerankSelector_OrdersWithinBucketsOnly(t *testing.T) {
	// Two repo tasks of equal priority: the reranker prefers the older one.
	// One other-repo task the reranker loves: it must stay below both.
	tasks := []memstore.Fact{
		task(1, "memstore: tidy the changelog", "memstore", "normal", "pending", time.Hour),
		task(2, "memstore: fix the recall pipeline", "memstore", "normal", "pending", 48*time.Hour),
		task(3, "herald: recall pipeline rewrite", "herald", "high", "pending", time.Minute),
	}
	rr := &scriptedTaskReranker{scores: map[string]float64{"recall pipeline": 0.9}}
	got, err := memstore.RerankSelector{Reranker: rr}.SelectTasks(context.Background(), tasks, memstore.TaskContext{Project: "memstore", CWD: "/r/memstore"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids(got)) != "[2 1 3]" {
		t.Errorf("order = %v, want [2 1 3]: rerank reorders inside the repo bucket, never across it", ids(got))
	}
	if !strings.Contains(rr.query, "memstore") || !strings.Contains(rr.query, "/r/memstore") {
		t.Errorf("rerank query = %q, want it to name the project and cwd", rr.query)
	}
}

func TestRerankSelector_DefersWithoutProjectAndDegradesOnOutage(t *testing.T) {
	tasks := []memstore.Fact{task(1, "a", "x", "normal", "pending", time.Hour)}
	rr := &scriptedTaskReranker{}
	if _, err := (memstore.RerankSelector{Reranker: rr}).SelectTasks(context.Background(), tasks, memstore.TaskContext{}); err != nil || rr.calls != 0 {
		t.Errorf("without a project the reranker was called %d times (err %v), want 0", rr.calls, err)
	}
	down := &scriptedTaskReranker{err: embedding.ErrRerankUnavailable}
	got, err := (memstore.RerankSelector{Reranker: down}).SelectTasks(context.Background(), tasks, memstore.TaskContext{Project: "x"})
	if err != nil || len(got) != 1 {
		t.Errorf("outage should degrade to the heuristic: got %v err %v", ids(got), err)
	}
	bug := &scriptedTaskReranker{err: &embedding.PermanentError{Err: errors.New("unknown model")}}
	if _, err := (memstore.RerankSelector{Reranker: bug}).SelectTasks(context.Background(), tasks, memstore.TaskContext{Project: "x"}); err == nil {
		t.Error("a caller-bug rerank error must surface")
	}
}

func TestTaskSelectorFromEnv(t *testing.T) {
	rr := &scriptedTaskReranker{}
	cases := []struct {
		env, want string
		rr        embedding.Reranker
		wantErr   bool
	}{
		{"", memstore.TaskSelectorHeuristic, nil, false},
		{"heuristic", memstore.TaskSelectorHeuristic, nil, false},
		{"rerank", memstore.TaskSelectorRerank, rr, false},
		{"rerank", "", nil, true},
		{"llm", "", nil, true},
		{"sideways", "", nil, true},
	}
	for _, c := range cases {
		t.Setenv("MEMSTORE_TASK_SELECTOR", c.env)
		_, name, err := memstore.TaskSelectorFromEnv("MEMSTORE", c.rr, 0)
		if (err != nil) != c.wantErr || name != c.want {
			t.Errorf("env %q rr=%v: name %q err %v, want %q wantErr %v", c.env, c.rr != nil, name, err, c.want, c.wantErr)
		}
	}
}

func TestTaskSelectRequest_Filters(t *testing.T) {
	f := memstore.TaskSelectRequest{Surface: "startup", Status: "pending"}.TaskFilters()
	if len(f) != 3 || f[0].Key != "kind" {
		t.Errorf("filters = %+v", f)
	}
}

// A task list is a list of work still to do. An unset status must therefore
// mean "open" -- pending, in progress, or never given a status -- and not
// "every task ever filed", which is how completed work reached the top of a
// session's five.
func TestTaskSelectRequest_DefaultStatusIsOpen(t *testing.T) {
	f := memstore.TaskSelectRequest{}.TaskFilters()
	if len(f) != 3 {
		t.Fatalf("default filters = %+v, want kind plus two status exclusions", f)
	}
	excluded := map[string]bool{}
	for _, mf := range f[1:] {
		if mf.Key != "status" || mf.Op != "!=" {
			t.Errorf("filter %+v, want a status != clause", mf)
		}
		if !mf.IncludeNull {
			t.Errorf("filter %+v must IncludeNull: a task with no status is open, not hidden", mf)
		}
		excluded[fmt.Sprint(mf.Value)] = true
	}
	if !excluded["completed"] || !excluded["cancelled"] {
		t.Errorf("excluded = %v, want completed and cancelled", excluded)
	}
}

// "all" is the escape hatch: it is how a caller asks for closed tasks too,
// and it is the only way to get no status predicate at all.
func TestTaskSelectRequest_StatusAll(t *testing.T) {
	f := memstore.TaskSelectRequest{Status: memstore.TaskStatusAll}.TaskFilters()
	if len(f) != 1 || f[0].Key != "kind" {
		t.Errorf("status=all filters = %+v, want kind only", f)
	}
}
