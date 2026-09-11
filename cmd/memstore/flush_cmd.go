package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// runFlush deletes facts that are old and unused (#157); see pgstore.PlanFlush
// for the rule and what it never touches. It reports by default. With --apply
// it writes a backup of every row it will remove, then deletes, re-checking
// the rule as it does.
func runFlush(args []string, out io.Writer) {
	fs := flag.NewFlagSet("flush", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	olderThan := fs.String("older-than", "90d", "only facts untouched for at least this long (90d, 720h)")
	maxUses := fs.Int("max-uses", 1, "only facts used fewer times than this: searches, recall injections, "+
		"confirmations and file-trigger or startup exposures together (1 = never used)")
	subject := fs.String("subject", "", "only facts with this subject")
	category := fs.String("category", "", "only facts in this category")
	kind := fs.String("kind", "", "only facts of this kind")
	sample := fs.Int("sample", 10, "candidates to list, least recently touched first")
	apply := fs.Bool("apply", false, "write the backup and delete; without this the command only reports")
	backup := fs.String("backup", "", "backup file to write before deleting (default: $XDG_STATE_HOME/memstore/flush-<time>.json)")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		fail(fmt.Errorf("flush: --older-than: %w", err))
	}
	opts := pgstore.FlushOpts{
		OlderThan: age, MaxUses: *maxUses,
		Subject: *subject, Category: *category, Kind: *kind,
		SampleLimit: *sample,
	}

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()
	ctx := context.Background()

	plan, err := pgstore.PlanFlush(ctx, pool, *namespace, opts)
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, flushReport(plan, *olderThan, *maxUses, *namespace))
	if len(plan.IDs) == 0 {
		return
	}
	if !*apply {
		fmt.Fprintln(out, "\nNothing deleted. --apply writes a backup of these facts, their chunks and links, then deletes them.")
		return
	}

	path := *backup
	if path == "" {
		if path, err = defaultFlushBackupPath(time.Now()); err != nil {
			fail(err)
		}
	}
	if err := writeFlushBackupFile(path, func(w io.Writer) error {
		return pgstore.WriteFlushBackup(ctx, pool, plan.IDs, w)
	}); err != nil {
		fail(fmt.Errorf("flush: backup: %w (nothing deleted)", err))
	}
	fmt.Fprintf(out, "\nBackup: %s\n", path)

	deleted, err := pgstore.ExecuteFlush(ctx, pool, *namespace, opts, plan.IDs)
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(out, "Deleted %d facts.", deleted)
	if kept := len(plan.IDs) - deleted; kept > 0 {
		fmt.Fprintf(out, " %d changed since the plan and were kept.", kept)
	}
	fmt.Fprintln(out)
}

// parseAge reads an age as whole days ("90d") or a Go duration ("720h"). It
// must be positive.
func parseAge(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("%q is neither days (90d) nor a duration (720h)", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not a positive age", s)
	}
	return d, nil
}

// defaultFlushBackupPath is where a backup goes when --backup is not given:
// the XDG state directory, named for the time it was taken so no flush
// overwrites another's backup.
func defaultFlushBackupPath(now time.Time) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("flush: no --backup given and no home directory for the default: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "memstore", "flush-"+now.Format("20060102-150405")+".json"), nil
}

// writeFlushBackupFile writes the backup to path. The file holds fact content,
// so it is created 0600 in a 0700 directory, and never over an existing file.
// It is synced before the delete is allowed to run.
func writeFlushBackupFile(path string, write func(io.Writer) error) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if err := write(f); err != nil {
		return err
	}
	return f.Sync()
}

var flushExclusionLabels = map[pgstore.FlushExclusion]string{
	pgstore.FlushPersistent:        "marked persistent",
	pgstore.FlushOpenTask:          "open task",
	pgstore.FlushSupersedesAnother: "superseded another fact",
	pgstore.FlushExplicitLink:      "has a link other than related",
	pgstore.FlushTriggerLoaded:     "loaded by a trigger",
}

func flushReport(p pgstore.FlushPlan, olderThan string, maxUses int, ns string) string {
	var b strings.Builder
	used := "never used"
	if maxUses > 1 {
		used = fmt.Sprintf("used fewer than %d times", maxUses)
	}
	fmt.Fprintf(&b, "Flush, namespace %s: %d active facts untouched for %s and %s.\n", ns, p.Matched, olderThan, used)
	for _, reason := range pgstore.FlushExclusions {
		if n := p.Excluded[reason]; n > 0 {
			fmt.Fprintf(&b, "  kept %5d: %s\n", n, flushExclusionLabels[reason])
		}
	}
	fmt.Fprintf(&b, "To delete: %d\n", len(p.IDs))
	if len(p.IDs) == 0 {
		return b.String()
	}
	writeTopCounts(&b, "By subject", p.BySubject, 10)
	writeTopCounts(&b, "By category", p.ByCategory, 10)
	if len(p.Sample) > 0 {
		fmt.Fprintf(&b, "\nLeast recently touched %d:\n", len(p.Sample))
		for _, c := range p.Sample {
			fmt.Fprintf(&b, "  [fact %d] %s %s | %s\n", c.ID, c.LastTouched.Format("2006-01-02"),
				memstore.SanitizeNotice(c.Subject), memstore.NoticeSnippet(c.Content, 80))
		}
	}
	return b.String()
}

// writeTopCounts lists the n largest counts, largest first, and says how many
// more there are.
func writeTopCounts(b *strings.Builder, title string, counts map[string]int, n int) {
	keys := slices.SortedFunc(maps.Keys(counts), func(x, y string) int {
		return cmp.Or(cmp.Compare(counts[y], counts[x]), strings.Compare(x, y))
	})
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, k := range keys[:min(n, len(keys))] {
		label := memstore.SanitizeNotice(k)
		if label == "" {
			label = "(none)"
		}
		fmt.Fprintf(b, "  %5d  %s\n", counts[k], label)
	}
	if rest := len(keys) - n; rest > 0 {
		fmt.Fprintf(b, "  ... and %d more\n", rest)
	}
}
