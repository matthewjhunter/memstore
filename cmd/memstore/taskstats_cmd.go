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

// runTaskStats reports how the task selector's choices are distributed over a
// window (task_selections). It exists to settle one question before anyone
// changes the ranking: HeuristicScore has no age term, so a high-priority
// task filed in March scores exactly what one filed yesterday scores and
// holds its slot indefinitely. Whether that is a problem in practice or only
// in theory is a measurement, and this is it.
//
// Concentration is the number to read. If a handful of tasks hold nearly
// every slot, the ranking is not turning over and a decay or
// shown-but-not-acted-on term earns its complexity. If selection spreads
// across many distinct tasks, it does not.
func runTaskStats(args []string, out io.Writer) {
	fs := flag.NewFlagSet("task-stats", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	since := fs.Duration("since", 14*24*time.Hour, "window to report on, ending now")
	top := fs.Int("top", 10, "how many of the most-shown tasks to list (0 = all)")
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}
	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	from := time.Now().Add(-*since)
	st, err := pgstore.TaskSelectionStats(context.Background(), pool, from, *top)
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, taskStatsReport(st, from))
}

// taskStatsReport renders the window's turnover. Slots per selection and
// distinct tasks together say how much of the backlog a session ever sees;
// TopShare says how much of that view five tasks account for.
func taskStatsReport(st memstore.TaskSelectionStats, from time.Time) string {
	s := fmt.Sprintf("task selections since %s: %d\n", from.Format("2006-01-02"), st.Selections)
	if st.Selections == 0 {
		return s + "  no selections in the window; nothing to report\n"
	}
	s += fmt.Sprintf("  slots filled: %d (%.1f per selection)\n",
		st.Slots, float64(st.Slots)/float64(st.Selections))
	if st.Slots == 0 {
		return s + "  every selection came back empty\n"
	}
	s += fmt.Sprintf("  distinct tasks shown: %d\n", st.DistinctTasks)
	s += fmt.Sprintf("  top 5 tasks hold %.1f%% of all slots\n", 100*st.TopShare)
	s += "\n  most-shown tasks:\n"
	for _, c := range st.Top {
		s += fmt.Sprintf("    id=%-6d shown %3d  (%.1f%% of slots)\n", c.TaskID, c.Times, 100*c.Share)
	}
	return s
}
