package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/matthewjhunter/memstore"
)

// Lint runs the model-free corpus checks. See memstore.LintKind for what
// each looks for and why. It reads only: every finding is a heuristic and
// the caller decides what to do about it.
//
// Counts cover the whole corpus and the sample is separate, because the
// point of the report is to say how large a problem is, not to show ten of
// something and imply that is all of it.
func Lint(ctx context.Context, pool *pgxpool.Pool, ns string, opts memstore.LintOpts) (memstore.LintReport, error) {
	rep := memstore.LintReport{Counts: map[memstore.LintKind]int{}}

	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM memstore_facts
		WHERE namespace = $1 AND superseded_by IS NULL
	`, ns).Scan(&rep.Active); err != nil {
		return rep, fmt.Errorf("pgstore: lint: counting active facts: %w", err)
	}

	// Each check is a predicate over the live facts in this namespace. They
	// are run as (count, sample) pairs against the same WHERE so a sample can
	// never disagree with its own count.
	checks := []struct {
		kind  memstore.LintKind
		where string
		args  []any
	}{
		{memstore.LintDuplicate, `EXISTS (
			SELECT 1 FROM memstore_facts earlier
			WHERE earlier.namespace = f.namespace AND earlier.superseded_by IS NULL
			  AND earlier.content = f.content AND earlier.id < f.id)`, nil},
		{memstore.LintOrphan, `NOT EXISTS (
			SELECT 1 FROM memstore_links l
			WHERE l.source_id = f.id OR l.target_id = f.id)`, nil},
		{memstore.LintMissingSubject, `f.subject = ''`, nil},
		{memstore.LintOddSubject, `f.subject <> '' AND f.subject !~ $2`, []any{memstore.SubjectPattern}},
		{memstore.LintNeverSurfaced, `f.use_count = 0 AND f.inject_count = 0
			AND f.confirmed_count = 0 AND f.created_at <= $2`, nil},
	}

	for _, c := range checks {
		if !opts.WantKind(c.kind) {
			continue
		}
		args := append([]any{ns}, c.args...)
		if c.kind == memstore.LintNeverSurfaced {
			args = append(args, time.Now().Add(-opts.MinAge))
		}
		base := `FROM memstore_facts f WHERE f.namespace = $1 AND f.superseded_by IS NULL AND (` + c.where + `)`

		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) `+base, args...).Scan(&n); err != nil {
			return rep, fmt.Errorf("pgstore: lint %s: %w", c.kind, err)
		}
		rep.Counts[c.kind] = n
		if n == 0 {
			continue
		}

		q := `SELECT f.id, f.subject, f.content, f.created_at ` + base + ` ORDER BY f.id`
		if opts.SampleLimit > 0 {
			q += fmt.Sprintf(" LIMIT %d", opts.SampleLimit)
		}
		rows, err := pool.Query(ctx, q, args...)
		if err != nil {
			return rep, fmt.Errorf("pgstore: lint %s sample: %w", c.kind, err)
		}
		for rows.Next() {
			f := memstore.LintFinding{Kind: c.kind}
			if err := rows.Scan(&f.FactID, &f.Subject, &f.Content, &f.CreatedAt); err != nil {
				rows.Close()
				return rep, fmt.Errorf("pgstore: lint %s sample: %w", c.kind, err)
			}
			f.Content = memstore.Truncate(f.Content, 100)
			rep.Findings = append(rep.Findings, f)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return rep, fmt.Errorf("pgstore: lint %s sample: %w", c.kind, err)
		}
	}
	return rep, nil
}
