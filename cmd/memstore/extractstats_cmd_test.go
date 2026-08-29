package main

import (
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

func TestExtractStatsReport(t *testing.T) {
	from := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	got := extractStatsReport(memstore.ExtractRunStats{Runs: 4, Inserted: 10, Superseded: 2, Duplicates: 4, Linked: 6, Errors: 1}, from)
	for _, want := range []string{"since 2026-08-15: 4", "duplicates 4", "duplicates per run: 1.00", "share of facts produced: 25.0%"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	empty := extractStatsReport(memstore.ExtractRunStats{}, from)
	if !strings.Contains(empty, "no runs in the window") {
		t.Errorf("empty window report = %q", empty)
	}
}
