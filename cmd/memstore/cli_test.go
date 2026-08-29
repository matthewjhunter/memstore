package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

func TestWriteTasksText_empty(t *testing.T) {
	var buf bytes.Buffer
	writeTasksText(&buf, nil, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for nil facts, got %q", buf.String())
	}

	buf.Reset()
	writeTasksText(&buf, []memstore.Fact{}, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty facts, got %q", buf.String())
	}
}

func TestWriteTasksText_tasks(t *testing.T) {
	meta := map[string]any{"priority": "high", "project": "myproject"}
	raw, _ := json.Marshal(meta)

	facts := []memstore.Fact{
		{ID: 1, Content: "Fix the login bug", Metadata: raw},
	}

	var buf bytes.Buffer
	writeTasksText(&buf, facts, "")
	out := buf.String()

	if !strings.Contains(out, "[MEMSTORE - Pending Tasks]") {
		t.Errorf("missing header in output: %q", out)
	}
	if !strings.Contains(out, "[high]") {
		t.Errorf("expected [high] prefix for high priority task: %q", out)
	}
	if !strings.Contains(out, "Fix the login bug") {
		t.Errorf("expected content in output: %q", out)
	}
	if !strings.Contains(out, "(project: myproject)") {
		t.Errorf("expected project suffix in output: %q", out)
	}
}

func TestWriteTasksText_normalPriority(t *testing.T) {
	meta := map[string]any{"priority": "normal"}
	raw, _ := json.Marshal(meta)

	facts := []memstore.Fact{
		{ID: 2, Content: "Do something", Metadata: raw},
	}

	var buf bytes.Buffer
	writeTasksText(&buf, facts, "")
	out := buf.String()

	if strings.Contains(out, "[high]") {
		t.Errorf("normal priority should not produce [high] prefix: %q", out)
	}
	if !strings.Contains(out, "Do something") {
		t.Errorf("expected content in output: %q", out)
	}
}

func TestWriteFactsText(t *testing.T) {
	facts := []memstore.Fact{
		{ID: 42, Subject: "matthew", Category: "preference", Content: "prefers dark mode"},
	}

	var buf bytes.Buffer
	writeFactsText(&buf, facts)
	out := buf.String()

	if !strings.Contains(out, "id=42") {
		t.Errorf("expected id in output: %q", out)
	}
	if !strings.Contains(out, "matthew") {
		t.Errorf("expected subject in output: %q", out)
	}
	if !strings.Contains(out, "preference") {
		t.Errorf("expected category in output: %q", out)
	}
	if !strings.Contains(out, "prefers dark mode") {
		t.Errorf("expected content in output: %q", out)
	}
}

func TestWriteJSON(t *testing.T) {
	facts := []memstore.Fact{
		{ID: 1, Subject: "test", Content: "hello"},
	}

	var buf bytes.Buffer
	if err := writeJSON(&buf, facts); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	var out []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, buf.String())
	}
	if len(out) != 1 {
		t.Errorf("expected 1 element, got %d", len(out))
	}
}

// A selection must say it is one: five tasks under a plain header read as
// the whole backlog.
func TestWriteTasksTextSelected_HeaderNamesTheTotal(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"priority": "normal", "project": "memstore"})
	facts := []memstore.Fact{{ID: 1, Content: "one", Metadata: raw}, {ID: 2, Content: "two", Metadata: raw}}

	var buf bytes.Buffer
	writeTasksTextSelected(&buf, facts, 190, "")
	if !strings.Contains(buf.String(), "(top 2 of 190 for this session; `memstore tasks` lists all)") {
		t.Errorf("selection header missing: %q", buf.String())
	}
	buf.Reset()
	writeTasksTextSelected(&buf, facts, 2, "")
	if !strings.HasPrefix(buf.String(), "[MEMSTORE - Pending Tasks]\n") || strings.Contains(buf.String(), "top") {
		t.Errorf("a complete list should use the plain header: %q", buf.String())
	}
}

// The heading has to match what was asked for. Printing "Pending Tasks" over
// a list of completed ones is the same lie the default status filter told.
func TestTasksHeading(t *testing.T) {
	cases := map[string]string{
		"":            "Pending Tasks",
		"pending":     "Pending Tasks",
		"in_progress": "Pending Tasks",
		"all":         "All Tasks",
		"completed":   "Tasks (completed)",
		"cancelled":   "Tasks (cancelled)",
	}
	for status, want := range cases {
		if got := tasksHeading(status); got != want {
			t.Errorf("tasksHeading(%q) = %q, want %q", status, got, want)
		}
	}
}
