package pgstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
)

// SubjectRename is one proposed or applied change.
type SubjectRename struct {
	From  string
	To    string
	Facts int
	// Merge means To was already in use, so this rename folds two spellings
	// of a topic into one. Merges are the valuable part: they are what turns
	// three subjects holding one fact each into one holding three.
	Merge bool
}

// NormalizeReport is a dry run or an applied run.
type NormalizeReport struct {
	Renames []SubjectRename
	Facts   int      // facts whose subject changes
	Merges  int      // renames that fold into an existing subject
	Skipped []string // subjects with nothing usable left; left untouched
	Applied bool
	Cleared int // vectors dropped for re-embedding
}

// NormalizeSubjects rewrites malformed subjects into the convention (see
// memstore.SlugifySubject). With apply false it only reports.
//
// Applying clears the affected facts' vectors. The subject is rendered into
// the embedded text as a "subject:" header (see factembed.go), so a fact
// whose subject changed but whose vector did not would be retrievable under
// a name it no longer has. Clearing embedding and its chunk rows puts the
// fact back in front of the embed queue, which rebuilds it under the new
// subject. That is the cost of this operation and it is not small: renaming
// N subjects means re-embedding every fact under them.
//
// This is mechanical and makes no claim to be sufficient. It fixes the shape
// of a subject, not its meaning: a subject that was never an entity name is
// still not one after slugifying, it is merely well-formed.
func NormalizeSubjects(ctx context.Context, pool *pgxpool.Pool, ns string, apply bool) (NormalizeReport, error) {
	rep := NormalizeReport{Applied: apply}

	rows, err := pool.Query(ctx, `
		SELECT subject, count(*) FROM memstore_facts
		WHERE namespace = $1 AND superseded_by IS NULL AND subject <> ''
		GROUP BY subject
	`, ns)
	if err != nil {
		return rep, fmt.Errorf("pgstore: normalize subjects: %w", err)
	}
	counts := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return rep, fmt.Errorf("pgstore: normalize subjects: %w", err)
		}
		counts[s] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return rep, fmt.Errorf("pgstore: normalize subjects: %w", err)
	}

	for from, n := range counts {
		if memstore.ValidSubject(from) {
			continue
		}
		to := memstore.SlugifySubject(from)
		if to == "" {
			rep.Skipped = append(rep.Skipped, from)
			continue
		}
		_, existing := counts[to]
		rep.Renames = append(rep.Renames, SubjectRename{From: from, To: to, Facts: n, Merge: existing})
		rep.Facts += n
		if existing {
			rep.Merges++
		}
	}
	// Deterministic order: biggest first, then alphabetical. A report whose
	// row order changes between runs cannot be diffed.
	sort.Slice(rep.Renames, func(i, j int) bool {
		if rep.Renames[i].Facts != rep.Renames[j].Facts {
			return rep.Renames[i].Facts > rep.Renames[j].Facts
		}
		return rep.Renames[i].From < rep.Renames[j].From
	})
	sort.Strings(rep.Skipped)

	if !apply || len(rep.Renames) == 0 {
		return rep, nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("pgstore: normalize subjects: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, r := range rep.Renames {
		// Chunk rows first: they are keyed on the fact and hold the vectors
		// that were built from the old subject header.
		if _, err := tx.Exec(ctx, `
			DELETE FROM memstore_fact_chunks WHERE fact_id IN (
				SELECT id FROM memstore_facts
				WHERE namespace = $1 AND superseded_by IS NULL AND subject = $2)
		`, ns, r.From); err != nil {
			return rep, fmt.Errorf("pgstore: clearing chunks for %q: %w", r.From, err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE memstore_facts
			SET subject = $3, embedding = NULL, embed_failed_at = NULL, embed_error = NULL
			WHERE namespace = $1 AND superseded_by IS NULL AND subject = $2
		`, ns, r.From, r.To)
		if err != nil {
			return rep, fmt.Errorf("pgstore: renaming %q to %q: %w", r.From, r.To, err)
		}
		rep.Cleared += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, fmt.Errorf("pgstore: normalize subjects: %w", err)
	}
	return rep, nil
}
