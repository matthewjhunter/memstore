package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/matthewjhunter/memstore"
)

// Flush removes facts that are old and unused (#157). It deletes user data, so
// it is split into a plan, a backup and a delete, and the delete re-checks the
// rule inside the statement that deletes rather than trusting the plan.
//
// Only active facts are candidates. A superseded fact is history; its chain is
// what memory_history walks.
//
// A fact is unused when its uses total fewer than MaxUses: explicit searches
// (use_count), recall injections (inject_count), confirmations, and the
// file-trigger and startup exposures context_injections records. Recall rows
// there are not added again; inject_count already counts them. Age runs from
// the last of those, or from creation when there has been none, so a fact
// written long ago and used last week is live.
//
// No kind, category or subject is exempt as such: policy lives in the data,
// not in lists here. Trigger facts age out like any other once they stop
// firing -- eval-triggers records a firing against the trigger's own id on the
// file-trigger channel, and that counts as a use. What is kept, each fact
// under the first reason that applies:
//   - marked persistent: the user set memstore.MetaPersistent on it.
//   - open tasks: a task whose status is not a closed one is live work
//     whether or not it has been shown lately.
//   - facts that superseded another: the older fact's superseded_by points at
//     them, and that foreign key has no delete action, so the delete would fail.
//   - facts with an explicit link: someone recorded a relation there. Links of
//     type related are the similarity linker's and are rebuilt on demand, and
//     nearly every fact has one, so they protect nothing.
//   - facts an active trigger loads, by subsystem or subject: that is a use,
//     and trigger loads were not recorded before #230.

// FlushExclusion is why a fact matching the age and use rule is kept.
type FlushExclusion string

const (
	FlushPersistent        FlushExclusion = "persistent"
	FlushOpenTask          FlushExclusion = "open_task"
	FlushSupersedesAnother FlushExclusion = "supersedes_another"
	FlushExplicitLink      FlushExclusion = "explicit_link"
	FlushTriggerLoaded     FlushExclusion = "trigger_loaded"
)

// FlushExclusions lists the reasons in the order they are tested.
var FlushExclusions = []FlushExclusion{FlushPersistent, FlushOpenTask, FlushSupersedesAnother, FlushExplicitLink, FlushTriggerLoaded}

// flushAutoLinkType is the similarity linker's link type.
const flushAutoLinkType = "related"

// FlushOpts selects the facts a flush removes.
type FlushOpts struct {
	OlderThan time.Duration // untouched for at least this long
	MaxUses   int           // used fewer times than this

	// Optional narrowing, to flush a known class rather than sweep.
	Subject, Category, Kind string

	SampleLimit int       // candidates to list in the plan; 0 = none
	Now         time.Time // the time age is measured from; zero means now
}

func (o FlushOpts) validate() error {
	if o.OlderThan <= 0 {
		return errors.New("flush: the age must be positive")
	}
	if o.MaxUses < 1 {
		return errors.New("flush: max uses must be at least 1; 1 means never used")
	}
	return nil
}

// FlushCandidate is one fact in a plan's sample.
type FlushCandidate struct {
	ID                      int64
	Subject, Category, Kind string
	Content                 string
	LastTouched             time.Time
	Uses                    int
}

// FlushPlan is what a flush would delete.
type FlushPlan struct {
	Matched    int                    // facts matching the age and use rule
	Excluded   map[FlushExclusion]int // of those, kept, by reason
	IDs        []int64                // the rest: what would be deleted
	BySubject  map[string]int         // IDs by subject
	ByCategory map[string]int         // IDs by category
	Sample     []FlushCandidate       // the least recently touched of IDs
}

// flushQuery selects every active fact in ns matching the rule, with the
// exclusion that keeps it (” for none). It is the rule's one definition: the
// plan reads it and the delete re-runs it.
func flushQuery(ctx context.Context, db pgx.Tx, ns string, opts FlushOpts) (queryBuilder, error) {
	var hasInjections bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('context_injections') IS NOT NULL`).Scan(&hasInjections); err != nil {
		return queryBuilder{}, fmt.Errorf("pgstore: flush: %w", err)
	}
	exposures := `SELECT NULL::bigint AS fact_id, 0::bigint AS n, NULL::timestamptz AS last_at WHERE false`
	if hasInjections {
		exposures = `SELECT ref_id::bigint AS fact_id, count(*) AS n, max(injected_at) AS last_at
		               FROM context_injections
		              WHERE ref_type = 'fact' AND channel IN ('file_trigger', 'startup') AND ref_id ~ '^[0-9]+$'
		              GROUP BY 1`
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var b queryBuilder
	b.q = `WITH exposure AS (` + exposures + `)
	SELECT f.id, f.subject, f.category, f.kind, f.content,
	       coalesce(greatest(f.last_used_at, f.last_injected_at, f.last_confirmed_at, e.last_at), f.created_at) AS last_touched,
	       f.use_count + f.inject_count + f.confirmed_count + coalesce(e.n, 0) AS uses,
	       CASE
	         WHEN f.metadata->>(`
	b.write(``, memstore.MetaPersistent)
	b.q += `::text) = 'true' THEN '` + string(FlushPersistent) + `'
	         WHEN f.kind = 'task' AND NOT coalesce(f.metadata->>'status', '') = ANY(`
	b.write(``, memstore.ClosedTaskStatuses())
	b.q += `::text[]) THEN '` + string(FlushOpenTask) + `'
	         WHEN EXISTS (SELECT 1 FROM memstore_facts o WHERE o.superseded_by = f.id) THEN '` + string(FlushSupersedesAnother) + `'
	         WHEN EXISTS (SELECT 1 FROM memstore_links l
	                       WHERE (l.source_id = f.id OR l.target_id = f.id) AND l.link_type <> '` + flushAutoLinkType + `') THEN '` + string(FlushExplicitLink) + `'
	         WHEN EXISTS (SELECT 1 FROM memstore_facts t
	                       WHERE t.namespace = f.namespace AND t.kind = 'trigger' AND t.superseded_by IS NULL
	                         AND ((f.subsystem <> '' AND t.metadata->>'load_subsystem' = f.subsystem)
	                           OR (f.subject <> '' AND t.metadata->>'load_subject' = f.subject))) THEN '` + string(FlushTriggerLoaded) + `'
	         ELSE ''
	       END AS excluded
	  FROM memstore_facts f
	  LEFT JOIN exposure e ON e.fact_id = f.id
	 WHERE f.superseded_by IS NULL`
	b.write(` AND f.namespace = `, ns)
	b.write(` AND f.use_count + f.inject_count + f.confirmed_count + coalesce(e.n, 0) < `, opts.MaxUses)
	b.write(` AND coalesce(greatest(f.last_used_at, f.last_injected_at, f.last_confirmed_at, e.last_at), f.created_at) < `,
		now.Add(-opts.OlderThan))
	if opts.Subject != "" {
		b.write(` AND f.subject = `, opts.Subject)
	}
	if opts.Category != "" {
		b.write(` AND f.category = `, opts.Category)
	}
	if opts.Kind != "" {
		b.write(` AND f.kind = `, opts.Kind)
	}
	return b, nil
}

// PlanFlush reports what a flush with opts would delete from ns. It writes
// nothing.
func PlanFlush(ctx context.Context, pool *pgxpool.Pool, ns string, opts FlushOpts) (FlushPlan, error) {
	if err := opts.validate(); err != nil {
		return FlushPlan{}, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return FlushPlan{}, fmt.Errorf("pgstore: flush plan: %w", err)
	}
	defer tx.Rollback(ctx)

	b, err := flushQuery(ctx, tx, ns, opts)
	if err != nil {
		return FlushPlan{}, err
	}
	rows, err := tx.Query(ctx, b.q+` ORDER BY last_touched, f.id`, b.args...)
	if err != nil {
		return FlushPlan{}, fmt.Errorf("pgstore: flush plan: %w", err)
	}
	defer rows.Close()

	p := FlushPlan{
		Excluded:   map[FlushExclusion]int{},
		BySubject:  map[string]int{},
		ByCategory: map[string]int{},
	}
	for rows.Next() {
		var c FlushCandidate
		var excluded string
		if err := rows.Scan(&c.ID, &c.Subject, &c.Category, &c.Kind, &c.Content, &c.LastTouched, &c.Uses, &excluded); err != nil {
			return FlushPlan{}, fmt.Errorf("pgstore: flush plan: %w", err)
		}
		p.Matched++
		if excluded != "" {
			p.Excluded[FlushExclusion(excluded)]++
			continue
		}
		p.IDs = append(p.IDs, c.ID)
		p.BySubject[c.Subject]++
		p.ByCategory[c.Category]++
		if len(p.Sample) < opts.SampleLimit {
			p.Sample = append(p.Sample, c)
		}
	}
	if err := rows.Err(); err != nil {
		return FlushPlan{}, fmt.Errorf("pgstore: flush plan: %w", err)
	}
	return p, nil
}

// WriteFlushBackup writes every row a delete of ids would remove, as the rows
// themselves: the facts, and the chunks and links that cascade with them. A
// restore from the facts alone would lose those. Each list is a JSON array of
// row_to_json objects, so it loads back with json_populate_recordset.
func WriteFlushBackup(ctx context.Context, pool *pgxpool.Pool, ids []int64, w io.Writer) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return fmt.Errorf("pgstore: flush backup: %w", err)
	}
	defer tx.Rollback(ctx)

	backup := struct {
		Version   int             `json:"version"`
		WrittenAt time.Time       `json:"written_at"`
		FactIDs   []int64         `json:"fact_ids"`
		Facts     json.RawMessage `json:"facts"`
		Chunks    json.RawMessage `json:"chunks"`
		Links     json.RawMessage `json:"links"`
	}{Version: 1, WrittenAt: time.Now().UTC(), FactIDs: ids}
	for _, q := range []struct {
		into *json.RawMessage
		sql  string
	}{
		{&backup.Facts, `SELECT coalesce(json_agg(row_to_json(f) ORDER BY f.id), '[]') FROM memstore_facts f WHERE f.id = ANY($1)`},
		{&backup.Chunks, `SELECT coalesce(json_agg(row_to_json(c) ORDER BY c.fact_id, c.ordinal), '[]') FROM memstore_fact_chunks c WHERE c.fact_id = ANY($1)`},
		{&backup.Links, `SELECT coalesce(json_agg(row_to_json(l) ORDER BY l.id), '[]') FROM memstore_links l WHERE l.source_id = ANY($1) OR l.target_id = ANY($1)`},
	} {
		var raw []byte
		if err := tx.QueryRow(ctx, q.sql, ids).Scan(&raw); err != nil {
			return fmt.Errorf("pgstore: flush backup: %w", err)
		}
		*q.into = raw
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	return enc.Encode(backup)
}

// ExecuteFlush deletes the facts in ids that still match opts, and returns how
// many it deleted. The rule is re-run in the delete statement, so a fact used,
// linked or marked persistent since the plan is kept. Chunks and links cascade.
func ExecuteFlush(ctx context.Context, pool *pgxpool.Pool, ns string, opts FlushOpts, ids []int64) (int, error) {
	if err := opts.validate(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pgstore: flush: %w", err)
	}
	defer tx.Rollback(ctx)

	b, err := flushQuery(ctx, tx, ns, opts)
	if err != nil {
		return 0, err
	}
	b.q = `WITH still AS (` + b.q + `)
	DELETE FROM memstore_facts WHERE id IN (SELECT id FROM still WHERE excluded = '')`
	b.write(` AND id = ANY(`, ids)
	b.q += `)`
	tag, err := tx.Exec(ctx, b.q, b.args...)
	if err != nil {
		return 0, fmt.Errorf("pgstore: flush: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pgstore: flush: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
