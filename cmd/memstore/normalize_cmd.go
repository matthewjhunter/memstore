package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/matthewjhunter/memstore/pgstore"
)

// runNormalizeSubjects rewrites malformed subjects into the convention. It
// reports by default and writes only with --apply, because applying clears
// the affected facts' vectors: the subject is part of the embedded text, so
// every renamed fact goes back through the embed queue.
//
// The pass is mechanical on purpose. It fixes the shape of a subject and not
// its meaning -- "Version control action" becomes "version-control-action",
// which is well-formed and still not an entity name. What it is actually for
// is the merges: folding three spellings of one topic into one subject is
// what turns a grouping key back into a grouping key.
func runNormalizeSubjects(args []string, out io.Writer) {
	fs := flag.NewFlagSet("normalize-subjects", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	apply := fs.Bool("apply", false, "write the changes; without this the command only reports")
	limit := fs.Int("show", 15, "renames to list (0 = all); merges are always listed in full")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}
	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	rep, err := pgstore.NormalizeSubjects(context.Background(), pool, *namespace, *apply)
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, normalizeReport(rep, *limit))
}

func normalizeReport(rep pgstore.NormalizeReport, show int) string {
	if len(rep.Renames) == 0 {
		s := "normalize-subjects: every subject already follows the convention\n"
		if len(rep.Skipped) > 0 {
			s += fmt.Sprintf("  %d left alone with nothing usable to slugify: %v\n", len(rep.Skipped), rep.Skipped)
		}
		return s
	}

	verb := "would rename"
	if rep.Applied {
		verb = "renamed"
	}
	s := fmt.Sprintf("normalize-subjects: %s %d subjects covering %d facts\n",
		verb, len(rep.Renames), rep.Facts)
	s += fmt.Sprintf("  %d of them merge into a subject that already exists\n", rep.Merges)
	if len(rep.Skipped) > 0 {
		s += fmt.Sprintf("  %d left alone with nothing usable to slugify: %v\n", len(rep.Skipped), rep.Skipped)
	}

	// Merges get listed in full however long the list: they are the changes
	// that alter what groups with what, so they are the ones worth reading.
	var merges []pgstore.SubjectRename
	for _, r := range rep.Renames {
		if r.Merge {
			merges = append(merges, r)
		}
	}
	if len(merges) > 0 {
		s += "\nmerges into an existing subject:\n"
		for _, r := range merges {
			s += fmt.Sprintf("  %-44q -> %-30q  %d fact(s)\n", r.From, r.To, r.Facts)
		}
	}

	s += "\nrenames:\n"
	shown := 0
	for _, r := range rep.Renames {
		if r.Merge {
			continue
		}
		if show > 0 && shown >= show {
			break
		}
		s += fmt.Sprintf("  %-44q -> %-30q  %d fact(s)\n", r.From, r.To, r.Facts)
		shown++
	}
	if rest := len(rep.Renames) - len(merges) - shown; rest > 0 {
		s += fmt.Sprintf("  ... and %d more (--show 0 for all)\n", rest)
	}

	s += "\n"
	if rep.Applied {
		s += fmt.Sprintf("Cleared %d vectors; the embed queue rebuilds them under the new subjects.\n", rep.Cleared)
		s += "Until it drains, those facts are missing from vector search.\n"
		return s
	}
	s += fmt.Sprintf("Nothing was written. --apply performs the renames and clears %d vectors,\n", rep.Facts)
	s += "since the subject is part of the embedded text and every renamed fact has\n"
	s += "to be embedded again. Until the queue drains they are missing from vector search.\n"
	s += "\nThis fixes the shape of a subject, not its meaning: " +
		strconv.Quote("Version control action") + " becomes\n" +
		strconv.Quote("version-control-action") + ", which is well-formed and still not an entity name.\n"
	return s
}
