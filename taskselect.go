package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/matthewjhunter/go-embedding"
)

// Task selection: which few pending tasks a session should see first.
//
// The startup hook used to inject every pending task -- 190 of them, 157 KB,
// over the hook's context cap -- so what actually reached the model was a
// truncated preview of whatever happened to sort first. A session can act on
// a handful, and the handful should be the ones that matter here and now:
// this repo's tasks, in-progress work, the high-priority ones. Everything
// below is the policy for choosing them; the full list is still one command
// away.

// TaskContext describes the session the selection is for.
type TaskContext struct {
	// CWD is the working directory the session started in.
	CWD string
	// Project is the repo name CWD maps to (ProjectNameFromCWD on the
	// client, which has the filesystem; the daemon does not). Empty means
	// no project affinity.
	Project string
	// Limit caps the result; zero means all, in selection order.
	Limit int
}

// TaskSelector orders pending tasks for a session and truncates to the
// context's limit. Implementations must be deterministic for a given input.
type TaskSelector interface {
	SelectTasks(ctx context.Context, tasks []Fact, tc TaskContext) ([]Fact, error)
}

// TaskMeta is the task-shaped subset of a fact's metadata.
type TaskMeta struct {
	Project  string
	Priority string
	Status   string
	Scope    string
}

// ParseTaskMeta reads the task fields from a fact's metadata; malformed or
// absent metadata yields zero values, which rank last.
func ParseTaskMeta(f Fact) TaskMeta {
	var m map[string]any
	if len(f.Metadata) > 0 {
		_ = json.Unmarshal(f.Metadata, &m)
	}
	get := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	return TaskMeta{Project: get("project"), Priority: get("priority"), Status: get("status"), Scope: get("scope")}
}

// Heuristic weights. Buckets, not a blend: a task for this repo outranks any
// task for another, whatever its priority; within a repo, work already in
// progress comes before work not started; within that, priority order.
// Recency breaks the remaining ties, newest first.
const (
	taskScoreProject    = 1000
	taskScoreInProgress = 100
	taskScoreHigh       = 20
	taskScoreNormal     = 10
)

// HeuristicScore is the bucket score the heuristic selector sorts by,
// exported so a wrapping selector can keep the buckets and reorder inside.
func HeuristicScore(m TaskMeta, tc TaskContext) int {
	s := 0
	if tc.Project != "" && strings.EqualFold(m.Project, tc.Project) {
		s += taskScoreProject
	}
	if m.Status == "in_progress" {
		s += taskScoreInProgress
	}
	switch m.Priority {
	case "high":
		s += taskScoreHigh
	case "normal", "":
		s += taskScoreNormal
	}
	return s
}

// HeuristicSelector ranks by HeuristicScore, then newest first.
type HeuristicSelector struct{}

func (HeuristicSelector) SelectTasks(_ context.Context, tasks []Fact, tc TaskContext) ([]Fact, error) {
	out := append([]Fact(nil), tasks...)
	scores := make(map[int64]int, len(out))
	for _, f := range out {
		scores[f.ID] = HeuristicScore(ParseTaskMeta(f), tc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if scores[out[i].ID] != scores[out[j].ID] {
			return scores[out[i].ID] > scores[out[j].ID]
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return truncateTasks(out, tc.Limit), nil
}

// RerankSelector keeps the heuristic buckets and lets a cross-encoder order
// the tasks inside each one by relevance to the session's project. The
// reranker never promotes a task across a bucket: it is good at "which of
// these is about the thing I am working on" and has no opinion about
// priority, so the buckets stay authoritative and it only breaks ties that
// recency would otherwise break arbitrarily. With no project in the context
// there is nothing to be relevant to and it defers to the heuristic.
type RerankSelector struct {
	Reranker embedding.Reranker
	// DocBytes truncates each task before scoring; zero uses the reranker's
	// default.
	DocBytes int
}

func (r RerankSelector) SelectTasks(ctx context.Context, tasks []Fact, tc TaskContext) ([]Fact, error) {
	if r.Reranker == nil || tc.Project == "" || len(tasks) == 0 {
		return HeuristicSelector{}.SelectTasks(ctx, tasks, tc)
	}
	out := append([]Fact(nil), tasks...)
	docs := make([]string, len(out))
	for i, f := range out {
		docs[i] = f.Content
	}
	res, err := RerankShrinking(ctx, r.Reranker, embedding.RerankRequest{
		Query:            taskRerankQuery(tc),
		Documents:        docs,
		MaxDocumentBytes: r.DocBytes,
	})
	if err != nil {
		if IsRerankDegradation(err) {
			LogRerankDegraded("tasks", err)
			return HeuristicSelector{}.SelectTasks(ctx, tasks, tc)
		}
		return nil, fmt.Errorf("memstore: task rerank: %w", err)
	}
	rerank := make(map[int64]float64, len(out))
	for _, rr := range res {
		if rr.Index < 0 || rr.Index >= len(out) {
			continue
		}
		rerank[out[rr.Index].ID] = rr.Score
	}
	scores := make(map[int64]int, len(out))
	for _, f := range out {
		scores[f.ID] = HeuristicScore(ParseTaskMeta(f), tc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if scores[out[i].ID] != scores[out[j].ID] {
			return scores[out[i].ID] > scores[out[j].ID]
		}
		if rerank[out[i].ID] != rerank[out[j].ID] {
			return rerank[out[i].ID] > rerank[out[j].ID]
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return truncateTasks(out, tc.Limit), nil
}

// taskRerankQuery is what the cross-encoder scores tasks against. It names
// the project and the directory so a task that mentions either reads as
// relevant; a cross-encoder is a relevance judge, not a planner, and the
// query stays a description of the situation rather than an instruction.
func taskRerankQuery(tc TaskContext) string {
	q := "pending work on the " + tc.Project + " project"
	if tc.CWD != "" {
		q += " in " + tc.CWD
	}
	return q
}

func truncateTasks(tasks []Fact, limit int) []Fact {
	if limit > 0 && len(tasks) > limit {
		return tasks[:limit]
	}
	return tasks
}

// Task selector strategies, chosen by {prefix}_TASK_SELECTOR.
const (
	TaskSelectorHeuristic = "heuristic"
	TaskSelectorRerank    = "rerank"
	// TaskSelectorLLM is reserved: a generator-backed selector that reads the
	// whole list and picks. Not built yet; naming it refuses to start rather
	// than silently falling back, so a configuration that asks for it is told.
	TaskSelectorLLM = "llm"
)

// TaskSelectorFromEnv builds the daemon's task selector from
// {prefix}_TASK_SELECTOR (default heuristic). rerank needs a configured
// reranker; without one it is an error rather than a quiet downgrade.
func TaskSelectorFromEnv(prefix string, rr embedding.Reranker, docBytes int) (TaskSelector, string, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(prefix + "_TASK_SELECTOR")))
	switch mode {
	case "", TaskSelectorHeuristic:
		return HeuristicSelector{}, TaskSelectorHeuristic, nil
	case TaskSelectorRerank:
		if rr == nil {
			return nil, "", fmt.Errorf("%s_TASK_SELECTOR=rerank needs a reranker (%s_RERANK_BASE_URL and %s_RERANK_MODEL)", prefix, prefix, prefix)
		}
		return RerankSelector{Reranker: rr, DocBytes: docBytes}, TaskSelectorRerank, nil
	case TaskSelectorLLM:
		return nil, "", fmt.Errorf("%s_TASK_SELECTOR=llm is reserved and not implemented yet", prefix)
	default:
		return nil, "", fmt.Errorf("%s_TASK_SELECTOR=%q: want %s, %s, or %s", prefix, mode, TaskSelectorHeuristic, TaskSelectorRerank, TaskSelectorLLM)
	}
}

// TaskSelectRequest is the body of POST /v1/tasks/select.
type TaskSelectRequest struct {
	CWD     string `json:"cwd,omitempty"`
	Project string `json:"project,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Surface string `json:"surface,omitempty"`
	Status  string `json:"status,omitempty"`
	Scope   string `json:"scope,omitempty"`
}

// TaskSelectResponse carries the chosen tasks and how many were eligible,
// so a caller can say "top 5 of 190" rather than present five as the whole.
type TaskSelectResponse struct {
	Tasks    []Fact `json:"tasks"`
	Total    int    `json:"total"`
	Selector string `json:"selector"`
}

// TaskFilters renders a select request's filters as the metadata filters
// List takes. kind=task is always applied; the request's fields narrow it.
func (r TaskSelectRequest) TaskFilters() []MetadataFilter {
	filters := []MetadataFilter{{Key: "kind", Op: "=", Value: "task"}}
	for k, v := range map[string]string{"surface": r.Surface, "status": r.Status, "scope": r.Scope} {
		if v != "" {
			filters = append(filters, MetadataFilter{Key: k, Op: "=", Value: v})
		}
	}
	return filters
}
