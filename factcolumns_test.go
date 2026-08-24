package memstore

import (
	"strings"
	"testing"
)

// countingScanner records how many destinations a scan function asks for,
// without needing a database.
type countingScanner struct{ n int }

func (c *countingScanner) Scan(dest ...any) error {
	c.n = len(dest)
	return nil
}

// scanFact and factColumns must agree on how many columns exist. This is the
// invariant CLAUDE.md names, and it is checked here rather than only by a live
// query because the live queries that would catch it are backend-specific: the
// pgstore ones need a Postgres service, so on a machine without one they skip
// and the mismatch ships.
//
// That is not hypothetical. Adding inject_count/last_injected_at (#158) passed
// a full local `go test ./...` and failed in CI with "number of field
// descriptions must equal number of destinations, got 20 and 18", because
// pgstore's vector search selected prefixedFactColumns and scanned into a
// hand-written list. This test fails on any machine, with no service
// container.
func TestFactColumnsMatchesScanFactArity(t *testing.T) {
	want := len(strings.Split(factColumns, ", "))

	var c countingScanner
	if _, err := scanFact(&c); err != nil {
		t.Fatalf("scanFact against a no-op scanner: %v", err)
	}

	if c.n != want {
		t.Errorf("scanFact asks for %d destinations, factColumns lists %d columns.\n"+
			"Adding a column means updating BOTH, plus pgstore's copies and ExportedFact.\ncolumns: %s",
			c.n, want, factColumns)
	}
}

// The prefixed form is what the join queries select, so it must stay a
// faithful rendering of the same list -- same count, every entry aliased.
func TestPrefixedFactColumnsMatchesFactColumns(t *testing.T) {
	plain := strings.Split(factColumns, ", ")
	prefixed := strings.Split(prefixedFactColumns("f."), ", ")

	if len(prefixed) != len(plain) {
		t.Fatalf("prefixedFactColumns has %d columns, factColumns has %d", len(prefixed), len(plain))
	}
	for i, col := range prefixed {
		if !strings.HasPrefix(strings.TrimSpace(col), "f.") {
			t.Errorf("column %d (%q) is not aliased; an unaliased column resolves by luck and breaks when a joined table gains the same name", i, col)
		}
		if strings.TrimSpace(col) != "f."+strings.TrimSpace(plain[i]) {
			t.Errorf("column %d: prefixed %q does not correspond to plain %q", i, col, plain[i])
		}
	}
}
