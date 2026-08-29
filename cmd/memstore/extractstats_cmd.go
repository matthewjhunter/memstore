package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// runExtractStats sums the per-session extraction counters the daemon
// records (extract_runs) over a window. It exists for #160: whether a fact
// Matthew restates unprompted is a usable confirmation signal depends on how
// often extraction drops a duplicate, and that number has to outlive a
// container restart to be measured at all.
func runExtractStats(args []string, out io.Writer) {
	fs := flag.NewFlagSet("extract-stats", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	since := fs.Duration("since", 14*24*time.Hour, "window to sum over, ending now")
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}
	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	from := time.Now().Add(-*since)
	st, err := pgstore.ExtractRunStats(context.Background(), pool, from)
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, extractStatsReport(st, from))
}

// extractStatsReport renders the window's totals and the one ratio #160
// asks for: duplicates per session, and duplicates as a share of every fact
// the extractor produced.
func extractStatsReport(st memstore.ExtractRunStats, from time.Time) string {
	produced := st.Inserted + st.Superseded + st.Duplicates
	s := fmt.Sprintf("extraction runs since %s: %d\n", from.Format("2006-01-02"), st.Runs)
	s += fmt.Sprintf("  inserted %d, superseded %d, duplicates %d, linked %d, errors %d\n",
		st.Inserted, st.Superseded, st.Duplicates, st.Linked, st.Errors)
	if st.Runs == 0 {
		return s + "  no runs in the window; nothing to rate\n"
	}
	s += fmt.Sprintf("  duplicates per run: %.2f\n", float64(st.Duplicates)/float64(st.Runs))
	if produced > 0 {
		s += fmt.Sprintf("  duplicates as a share of facts produced: %.1f%%\n", 100*float64(st.Duplicates)/float64(produced))
	}
	return s
}
