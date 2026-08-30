package pgstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LinkCandidate is one pair the extraction path would have linked.
type LinkCandidate struct {
	SourceID int64
	TargetID int64
	Sim      float64
}

// BackfillLinksOpts tunes the pass.
type BackfillLinksOpts struct {
	// MinSim is the cosine gate. Pass the running daemon's
	// SimilarityPolicy.LinkMinSim so the backfill agrees with what
	// extraction does now rather than with what it did when a fact was
	// stored.
	MinSim float64
	// TopK neighbours to consider per fact before the gate. The vector index
	// makes this the cost knob: it bounds the work at len(facts)*TopK rather
	// than the 11.8M pairs a full comparison would need on this corpus.
	TopK int
	// MaxPerFact caps how many NEW links one fact may gain, mirroring the
	// extraction path's limit of three per newly stored fact. Pre-existing
	// links do not count against it, exactly as they do not at extraction.
	MaxPerFact int
	// Apply writes; without it the pass only reports.
	Apply bool
	// SampleLimit caps the listed candidates. Counts are always complete.
	SampleLimit int
}

// BackfillLinksReport is a dry run or an applied run.
type BackfillLinksReport struct {
	Facts      int // active facts with a vector
	NoVector   int // active facts skipped for want of one
	Candidates int // pairs above the gate, after existing links and duplicates are removed
	Added      int
	Applied    bool
	MinSim     float64
	Buckets    map[string]int // similarity distribution of the candidates
	Sample     []LinkCandidate
}

// BackfillLinks creates the links the extraction path would have made, for
// facts that already existed when it ran.
//
// The gate is applied at exactly one point in the live system -- a newly
// extracted fact compared against its neighbours (httpapi/extractqueue.go)
// -- so correcting the gate changes new facts and leaves every older one
// alone. On the 2026-08-30 corpus that was 3,247 of 4,854 facts with no
// links at all, at a rate flat across provenance, which is what ruled out
// any single ingestion path being at fault.
//
// Pairs are deduplicated as unordered: links are written bidirectional, so
// a->b and b->a are the same edge and writing both would double the graph.
// Candidates are taken strongest first, which makes the result deterministic
// and spends each fact's budget on its best neighbours.
func BackfillLinks(ctx context.Context, pool *pgxpool.Pool, ns string, opts BackfillLinksOpts) (BackfillLinksReport, error) {
	if opts.TopK <= 0 {
		opts.TopK = 10
	}
	if opts.MaxPerFact <= 0 {
		opts.MaxPerFact = 3
	}
	rep := BackfillLinksReport{Applied: opts.Apply, MinSim: opts.MinSim, Buckets: map[string]int{}}

	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE embedding IS NOT NULL),
		       count(*) FILTER (WHERE embedding IS NULL)
		FROM memstore_facts WHERE namespace = $1 AND superseded_by IS NULL
	`, ns).Scan(&rep.Facts, &rep.NoVector); err != nil {
		return rep, fmt.Errorf("pgstore: backfill links: counting facts: %w", err)
	}
	if rep.Facts == 0 {
		return rep, nil
	}

	// Top-k per fact through the HNSW index, then the gate. Pairs already
	// linked in either direction are excluded here rather than later: the
	// edge exists, and which way it points is not this pass's business.
	rows, err := pool.Query(ctx, `
		SELECT f.id, n.id, 1 - (f.embedding <=> n.embedding) AS sim
		FROM memstore_facts f
		CROSS JOIN LATERAL (
			SELECT c.id, c.embedding
			FROM memstore_facts c
			WHERE c.namespace = f.namespace AND c.superseded_by IS NULL
			  AND c.embedding IS NOT NULL AND c.id <> f.id
			ORDER BY c.embedding <=> f.embedding
			LIMIT $2
		) n
		WHERE f.namespace = $1 AND f.superseded_by IS NULL AND f.embedding IS NOT NULL
		  AND 1 - (f.embedding <=> n.embedding) >= $3
		  AND NOT EXISTS (
			SELECT 1 FROM memstore_links l
			WHERE (l.source_id = f.id AND l.target_id = n.id)
			   OR (l.source_id = n.id AND l.target_id = f.id))
	`, ns, opts.TopK, opts.MinSim)
	if err != nil {
		return rep, fmt.Errorf("pgstore: backfill links: searching neighbours: %w", err)
	}
	seen := map[[2]int64]bool{}
	var cands []LinkCandidate
	for rows.Next() {
		var c LinkCandidate
		if err := rows.Scan(&c.SourceID, &c.TargetID, &c.Sim); err != nil {
			rows.Close()
			return rep, fmt.Errorf("pgstore: backfill links: %w", err)
		}
		key := [2]int64{c.SourceID, c.TargetID}
		if key[0] > key[1] {
			key[0], key[1] = key[1], key[0]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		cands = append(cands, LinkCandidate{SourceID: key[0], TargetID: key[1], Sim: c.Sim})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return rep, fmt.Errorf("pgstore: backfill links: %w", err)
	}

	// Strongest first, id order breaking ties so a rerun proposes the same
	// graph. Then spend each fact's budget greedily.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Sim != cands[j].Sim {
			return cands[i].Sim > cands[j].Sim
		}
		if cands[i].SourceID != cands[j].SourceID {
			return cands[i].SourceID < cands[j].SourceID
		}
		return cands[i].TargetID < cands[j].TargetID
	})
	budget := map[int64]int{}
	var chosen []LinkCandidate
	for _, c := range cands {
		if budget[c.SourceID] >= opts.MaxPerFact || budget[c.TargetID] >= opts.MaxPerFact {
			continue
		}
		budget[c.SourceID]++
		budget[c.TargetID]++
		chosen = append(chosen, c)
		rep.Buckets[simBucket(c.Sim)]++
	}
	rep.Candidates = len(chosen)
	for i, c := range chosen {
		if opts.SampleLimit > 0 && i >= opts.SampleLimit {
			break
		}
		rep.Sample = append(rep.Sample, c)
	}
	if !opts.Apply || len(chosen) == 0 {
		return rep, nil
	}

	// The same guarded insert LinkFacts uses in service scope: the owner is
	// derived from the endpoints and a pair spanning two users inserts
	// nothing. Isolation is not this pass's to relax.
	for _, c := range chosen {
		tag, err := pool.Exec(ctx, `
			INSERT INTO memstore_links (namespace, user_id, source_id, target_id, link_type, bidirectional, label, metadata, created_at)
			SELECT $1, o.user_id, $2, $3, 'related', TRUE, '', NULL, NOW()
			FROM (SELECT user_id, COUNT(DISTINCT id) AS n
			      FROM memstore_facts
			      WHERE id IN ($2, $3) AND namespace = $1
			      GROUP BY user_id) o
			WHERE o.n = 2
		`, ns, c.SourceID, c.TargetID)
		if err != nil {
			return rep, fmt.Errorf("pgstore: backfill links: linking %d-%d: %w", c.SourceID, c.TargetID, err)
		}
		rep.Added += int(tag.RowsAffected())
	}
	return rep, nil
}

// simBucket groups a similarity for the distribution report, so an operator
// can see whether the gate sits on a cliff or in the middle of a mass.
func simBucket(sim float64) string {
	switch {
	case sim >= 0.90:
		return "0.90+"
	case sim >= 0.80:
		return "0.80-0.90"
	case sim >= 0.70:
		return "0.70-0.80"
	case sim >= 0.60:
		return "0.60-0.70"
	case sim >= 0.50:
		return "0.50-0.60"
	default:
		return "below 0.50"
	}
}
