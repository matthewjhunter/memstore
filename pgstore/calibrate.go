package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// QueryEmbedFunc embeds a fact's content the way the search path embeds a
// query. The auto-link gate compares a query-side embedding of the new fact's
// content against document-side stored vectors, and on models with task
// prefixes those two sides are not symmetric: a document-to-document cosine
// overstates what the gate sees. SampleSimilarity uses this for the link side
// when given one, and falls back to the stored vector otherwise.
type QueryEmbedFunc func(ctx context.Context, content string) ([]float32, error)

// SimilarityPair is one measured pair for calibrating the similarity gates.
type SimilarityPair struct {
	SourceID, TargetID int64
	// Cosine is the similarity between the two facts' stored vectors.
	Cosine float64
	// Floor is the similarity between the source and its floorRank-th nearest
	// active neighbour -- the background level a gate has to sit above. Only
	// filled for linked pairs.
	Floor float64
}

// SimilaritySample is what SampleSimilarity measures: the pairs the auto-link
// gate is meant to admit (existing "related" links between active facts) and
// the pairs the auto-supersede gate is meant to admit (a superseded fact and
// its successor), both scored on the vectors as stored, plus the model that
// produced those vectors.
type SimilaritySample struct {
	Model string
	// LinkedQuerySide is true when the linked pairs were scored from a
	// query-side embedding of the source (the gate's view) rather than the
	// source's stored vector.
	LinkedQuerySide bool
	Linked          []SimilarityPair
	Superseded      []SimilarityPair
}

// SampleSimilarity reads up to n linked pairs and n supersession pairs from
// namespace and scores each with pgvector, so the gates in
// memstore.SimilarityPolicy can be set from the corpus rather than carried
// over from a previous model. Run it after a re-embed has finished.
//
// Linked pairs are scored as the auto-link gate scores them when queryEmbed
// is given: the source's content embedded as a query against the target's
// stored vector, and the floor is the floorRank-th nearest active fact to that
// query vector. With a nil queryEmbed both use the source's stored vector,
// which is a document-to-document comparison and, on a prefixed model, runs
// higher than the gate's view. Supersession pairs are always stored-to-stored,
// which is how the supersede gate compares them. The linked sample is
// pseudo-random (ordered by a hash of the link id); the supersession sample
// is the most recent n.
func SampleSimilarity(ctx context.Context, pool *pgxpool.Pool, namespace string, n, floorRank int, queryEmbed QueryEmbedFunc) (SimilaritySample, error) {
	out := SimilaritySample{LinkedQuerySide: queryEmbed != nil}
	if n <= 0 {
		n = 60
	}
	if floorRank <= 0 {
		floorRank = 50
	}
	if err := pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&out.Model); err != nil {
		out.Model = ""
	}

	rows, err := pool.Query(ctx, `
		SELECT l.source_id, l.target_id, a.content, 1 - (a.embedding <=> b.embedding), b.embedding
		FROM memstore_links l
		JOIN memstore_facts a ON a.id = l.source_id
		JOIN memstore_facts b ON b.id = l.target_id
		WHERE l.namespace = $1 AND l.link_type = 'related'
		  AND a.superseded_by IS NULL AND b.superseded_by IS NULL
		  AND a.embedding IS NOT NULL AND b.embedding IS NOT NULL
		ORDER BY md5(l.id::text)
		LIMIT $2`, namespace, n)
	if err != nil {
		return out, fmt.Errorf("pgstore: sampling linked pairs: %w", err)
	}
	type linked struct {
		pair    SimilarityPair
		content string
		target  pgvector.Vector
	}
	var links []linked
	for rows.Next() {
		var l linked
		if err := rows.Scan(&l.pair.SourceID, &l.pair.TargetID, &l.content, &l.pair.Cosine, &l.target); err != nil {
			rows.Close()
			return out, err
		}
		links = append(links, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, l := range links {
		p := l.pair
		// The vector the gate compares from: the query-side embedding of the
		// source content when available, else the source's stored vector.
		var from any = nil
		if queryEmbed != nil {
			q, err := queryEmbed(ctx, l.content)
			if err != nil {
				return out, fmt.Errorf("pgstore: embedding source %d as a query: %w", p.SourceID, err)
			}
			qv := pgvector.NewVector(q)
			from = qv
			if err := pool.QueryRow(ctx, `SELECT 1 - ($1::vector <=> $2::vector)`, qv, l.target).Scan(&p.Cosine); err != nil {
				return out, fmt.Errorf("pgstore: scoring pair %d->%d: %w", p.SourceID, p.TargetID, err)
			}
		}
		var floorErr error
		if from != nil {
			floorErr = pool.QueryRow(ctx, `
				SELECT 1 - (f.embedding <=> $1::vector)
				FROM memstore_facts f
				WHERE f.namespace = $2 AND f.id <> $3
				  AND f.superseded_by IS NULL AND f.embedding IS NOT NULL
				ORDER BY f.embedding <=> $1::vector
				LIMIT 1 OFFSET $4`, from, namespace, p.SourceID, floorRank-1).Scan(&p.Floor)
		} else {
			floorErr = pool.QueryRow(ctx, `
				SELECT 1 - (f.embedding <=> a.embedding)
				FROM memstore_facts f, memstore_facts a
				WHERE a.id = $1 AND f.namespace = $2 AND f.id <> a.id
				  AND f.superseded_by IS NULL AND f.embedding IS NOT NULL
				ORDER BY f.embedding <=> a.embedding
				LIMIT 1 OFFSET $3`, p.SourceID, namespace, floorRank-1).Scan(&p.Floor)
		}
		if floorErr != nil {
			// Fewer active facts than floorRank: no floor to report.
			p.Floor = 0
		}
		out.Linked = append(out.Linked, p)
	}

	rows, err = pool.Query(ctx, `
		SELECT o.id, o.superseded_by, 1 - (o.embedding <=> s.embedding)
		FROM memstore_facts o
		JOIN memstore_facts s ON s.id = o.superseded_by
		WHERE o.namespace = $1 AND o.embedding IS NOT NULL AND s.embedding IS NOT NULL
		ORDER BY o.superseded_at DESC NULLS LAST
		LIMIT $2`, namespace, n)
	if err != nil {
		return out, fmt.Errorf("pgstore: sampling supersession pairs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p SimilarityPair
		if err := rows.Scan(&p.SourceID, &p.TargetID, &p.Cosine); err != nil {
			return out, err
		}
		out.Superseded = append(out.Superseded, p)
	}
	return out, rows.Err()
}
