package pgstore

import (
	"strings"
	"testing"
)

type countingScanner struct{ n int }

func (c *countingScanner) Scan(dest ...any) error {
	c.n = len(dest)
	return nil
}

// The pgstore mirror of the root-package guard, and the one that would have
// caught #158's CI failure locally.
//
// Every pgstore test that touches a real query is gated on MEMSTORE_TEST_PG, so
// on a machine without a Postgres service the whole package skips and a column
// mismatch is invisible until CI. This test needs no service.
func TestFactColumnsMatchesScanFactArity(t *testing.T) {
	want := len(strings.Split(factColumns, ", "))

	var c countingScanner
	if _, err := scanFact(&c); err != nil {
		t.Fatalf("scanFact against a no-op scanner: %v", err)
	}

	if c.n != want {
		t.Errorf("scanFact asks for %d destinations, factColumns lists %d columns.\n"+
			"Search paths must go through scanFact rather than keeping their own destination list.\ncolumns: %s",
			c.n, want, factColumns)
	}
}

func TestPrefixedFactColumnsMatchesFactColumns(t *testing.T) {
	plain := strings.Split(factColumns, ", ")
	prefixed := strings.Split(prefixedFactColumns("f."), ", ")

	if len(prefixed) != len(plain) {
		t.Fatalf("prefixedFactColumns has %d columns, factColumns has %d", len(prefixed), len(plain))
	}
	for i, col := range prefixed {
		if strings.TrimSpace(col) != "f."+strings.TrimSpace(plain[i]) {
			t.Errorf("column %d: prefixed %q does not correspond to plain %q", i, col, plain[i])
		}
	}
}
