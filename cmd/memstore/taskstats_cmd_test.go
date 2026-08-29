package main

import (
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

func TestTaskStatsReport(t *testing.T) {
	from := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	t.Run("empty window says so rather than dividing by zero", func(t *testing.T) {
		out := taskStatsReport(memstore.TaskSelectionStats{}, from)
		if !strings.Contains(out, "no selections in the window") {
			t.Errorf("report = %q", out)
		}
	})

	t.Run("selections that returned nothing are distinguished from no selections", func(t *testing.T) {
		out := taskStatsReport(memstore.TaskSelectionStats{Selections: 4}, from)
		if !strings.Contains(out, "every selection came back empty") {
			t.Errorf("report = %q", out)
		}
	})

	t.Run("reports turnover", func(t *testing.T) {
		out := taskStatsReport(memstore.TaskSelectionStats{
			Selections: 20, Slots: 100, DistinctTasks: 12, TopShare: 0.85,
			Top: []memstore.TaskSelectionCount{
				{TaskID: 4098, Times: 20, Share: 0.2},
				{TaskID: 8358, Times: 19, Share: 0.19},
			},
		}, from)
		for _, want := range []string{
			"task selections since 2026-08-15: 20",
			"slots filled: 100 (5.0 per selection)",
			"distinct tasks shown: 12",
			"top 5 tasks hold 85.0% of all slots",
			"id=4098   shown  20  (20.0% of slots)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("report missing %q:\n%s", want, out)
			}
		}
	})
}
