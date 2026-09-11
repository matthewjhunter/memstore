package pgstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/matthewjhunter/memstore"
)

// Approximate nearest-neighbour search over fact chunk vectors.
//
// Fact vector search is an exact scan: the distance from the query to every
// chunk, collapsed to each fact's nearest chunk. That is right for a small
// store and linear in the chunk count, so once a store reaches the threshold an
// HNSW index takes over for searches with no selective filter.
//
// The vector columns carry no dimension -- no deployment sets vec-dim, see
// vectorColumnType -- and pgvector cannot index an unconstrained column. The
// index is therefore on an expression that casts to the dimension the stored
// vectors have, and a query uses it only when it orders by that same cast. That
// is why the dimension is part of the index name and of the state the query
// path reads.
//
// Two things decide recall, measured on the live corpus (6,900 chunks, the top
// 60 facts against the exact scan's). Iterative scans matter most: without them
// an HNSW scan returns at most ef_search rows whatever the LIMIT says, so
// filtering the nearest 240 chunks afterwards matched 42% at ef_search 40 and
// 83% at 100, and with them 98% at either. Filtering inside the scan rather
// than afterwards took that to 99.7% or better at every ef_search tried, and it
// is what keeps a scoped store's search from being crowded out by other users'
// chunks. Iterative scans need pgvector 0.8.

const (
	// annIndexPrefix names the index; the dimension follows it.
	annIndexPrefix = "idx_memstore_fact_chunk_ann_"

	// DefaultANNThreshold is the chunk count at which EnsureFactChunkIndex
	// builds the index. Below it the exact scan is fast enough -- about 25 ms
	// at 7,000 chunks and 40 ms at 10,000, measured -- and it is exact.
	DefaultANNThreshold = 20000

	annEfSearch = 100

	// annOverfetch sizes the chunk scan against the facts wanted: a fact with
	// several near chunks takes several of its rows.
	annOverfetch = 4
)

// annState is shared by a store and its scoped copies -- ForUser and
// ServiceScope copy the struct -- so one EnsureFactChunkIndex serves them all.
type annState struct {
	mu        sync.RWMutex
	dim       int // dimension of the usable index; 0 when there is none
	threshold int

	versionWarning sync.Once
}

func newANNState() *annState { return &annState{threshold: DefaultANNThreshold} }

// SetANNThreshold sets the chunk count at which EnsureFactChunkIndex builds the
// index. Zero or less builds it as soon as there are vectors.
func (s *PostgresStore) SetANNThreshold(n int) {
	s.ann.mu.Lock()
	defer s.ann.mu.Unlock()
	s.ann.threshold = n
}

func (s *PostgresStore) annThreshold() int {
	s.ann.mu.RLock()
	defer s.ann.mu.RUnlock()
	return s.ann.threshold
}

// annDim is the dimension of the index the query path may use, or 0.
func (s *PostgresStore) annDim() int {
	s.ann.mu.RLock()
	defer s.ann.mu.RUnlock()
	return s.ann.dim
}

func (s *PostgresStore) setANNDim(dim int) {
	s.ann.mu.Lock()
	defer s.ann.mu.Unlock()
	s.ann.dim = dim
}

// useANN reports whether a search with opts goes to the index. A selective
// filter keeps it exact: an index walk looking for the few chunks that pass
// gives up after hnsw.max_scan_tuples, and could stop short of them.
func (s *PostgresStore) useANN(opts memstore.SearchOpts) bool {
	if s.annDim() == 0 {
		return false
	}
	return opts.Subject == "" && opts.Category == "" && opts.Kind == "" && opts.Subsystem == "" &&
		len(opts.MetadataFilters) == 0 && opts.CreatedAfter == nil && opts.CreatedBefore == nil
}

// EnsureFactChunkIndex brings the ANN index into line with the store, and
// reports whether it built one. It drops an index that a failed concurrent
// build left invalid, or that is cast to a dimension the vectors no longer
// have; it keeps a valid one; and it builds one once the chunk count has
// reached the threshold.
//
// The build is concurrent, so writes carry on while it runs, and on a large
// store it takes a while (49 s for 50,000 chunks, measured). Call it from a
// background task, never on a request.
func (s *PostgresStore) EnsureFactChunkIndex(ctx context.Context) (bool, error) {
	ok, err := s.iterativeScanSupported(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		s.setANNDim(0)
		return false, nil
	}

	var dim int
	err = s.pool.QueryRow(ctx,
		`SELECT vector_dims(embedding) FROM memstore_fact_chunks WHERE embedding IS NOT NULL LIMIT 1`).Scan(&dim)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("pgstore: reading chunk vector dimension: %w", err)
	}

	indexes, err := s.annIndexes(ctx)
	if err != nil {
		return false, err
	}
	kept := 0
	for _, ix := range indexes {
		if ix.valid && ix.dim == dim && kept == 0 {
			kept = ix.dim
			continue
		}
		if _, err := s.pool.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+pgx.Identifier{ix.name}.Sanitize()); err != nil {
			return false, fmt.Errorf("pgstore: dropping %s: %w", ix.name, err)
		}
	}
	if kept > 0 {
		s.setANNDim(kept)
		return false, nil
	}
	s.setANNDim(0)
	if dim == 0 {
		return false, nil
	}

	var chunks int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM memstore_fact_chunks`).Scan(&chunks); err != nil {
		return false, fmt.Errorf("pgstore: counting fact chunks: %w", err)
	}
	if chunks < int64(s.annThreshold()) {
		return false, nil
	}

	name := annIndexPrefix + strconv.Itoa(dim)
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON memstore_fact_chunks USING hnsw ((embedding::vector(%d)) vector_cosine_ops)`,
		pgx.Identifier{name}.Sanitize(), dim)); err != nil {
		return false, fmt.Errorf("pgstore: building %s: %w", name, err)
	}
	s.setANNDim(dim)
	return true, nil
}

// iterativeScanSupported reports whether the installed pgvector has iterative
// index scans (0.8 and later). Without them a filtered index scan returns at
// most ef_search rows and the filter is applied to what is left, which is the
// recall loss measured above, so an older pgvector keeps the exact scan.
func (s *PostgresStore) iterativeScanSupported(ctx context.Context) (bool, error) {
	var version string
	if err := s.pool.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&version); err != nil {
		return false, fmt.Errorf("pgstore: reading pgvector version: %w", err)
	}
	major, minor, _ := strings.Cut(version, ".")
	minor, _, _ = strings.Cut(minor, ".")
	maj, err1 := strconv.Atoi(major)
	mnr, err2 := strconv.Atoi(minor)
	if err1 == nil && err2 == nil && (maj > 0 || mnr >= 8) {
		return true, nil
	}
	s.ann.versionWarning.Do(func() {
		log.Printf("pgstore: pgvector %s has no iterative index scans (0.8 and later); fact vector search stays exact", version)
	})
	return false, nil
}

type annIndex struct {
	name  string
	dim   int
	valid bool
}

// annIndexes lists the ANN indexes on the fact chunk table, with the dimension
// read from each name.
func (s *PostgresStore) annIndexes(ctx context.Context) ([]annIndex, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.relname, i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		  WHERE i.indrelid = 'memstore_fact_chunks'::regclass AND starts_with(c.relname, $1)`, annIndexPrefix)
	if err != nil {
		return nil, fmt.Errorf("pgstore: listing ANN indexes: %w", err)
	}
	defer rows.Close()
	var out []annIndex
	for rows.Next() {
		var ix annIndex
		if err := rows.Scan(&ix.name, &ix.valid); err != nil {
			return nil, fmt.Errorf("pgstore: scanning ANN index: %w", err)
		}
		// An unparseable suffix leaves dim at 0, which matches no vectors, so
		// the index is dropped as stale.
		ix.dim, _ = strconv.Atoi(strings.TrimPrefix(ix.name, annIndexPrefix))
		out = append(out, ix)
	}
	return out, rows.Err()
}

// dropANNIndexesSQL drops every fact chunk ANN index, whatever its dimension,
// in one statement, so the two paths that clear vectors can run it on a pool or
// inside a transaction alike.
const dropANNIndexesSQL = `DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
            WHERE i.indrelid = 'memstore_fact_chunks'::regclass
              AND starts_with(c.relname, '` + annIndexPrefix + `')
  LOOP
    EXECUTE format('DROP INDEX IF EXISTS %I', r.relname);
  END LOOP;
END $$`

// searchVectorANN is searchVector through the index.
func (s *PostgresStore) searchVectorANN(ctx context.Context, queryEmb []float32, opts memstore.SearchOpts, dim int) ([]memstore.SearchResult, error) {
	b, err := s.annVectorQuery(queryEmb, opts, dim)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("pgstore: ANN vector search: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := applyANNSettings(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: ANN vector search: %w", err)
	}
	return scanVectorResults(rows)
}

// annVectorQuery is the index-driven form of searchVector's query. The nearest
// chunks that pass the filters come off the index in distance order; they are
// collapsed to each fact's nearest chunk and ranked, as the exact query does.
// relaxed_order lets the iterative scan return rows slightly out of order,
// which the outer sort puts right.
func (s *PostgresStore) annVectorQuery(queryEmb []float32, opts memstore.SearchOpts, dim int) (queryBuilder, error) {
	qv := pgvector.NewVector(queryEmb)
	cast := fmt.Sprintf("::vector(%d)", dim)
	// The same expression as the index's, or the planner cannot use it.
	dist := `c.embedding` + cast + ` <=> `

	var b queryBuilder
	b.write(`SELECT `+prefixedFactColumns("f.")+`, 1 - (`+dist, qv)
	b.q += cast + `) AS similarity
	         FROM memstore_fact_chunks c
	         JOIN memstore_facts f ON f.id = c.fact_id
	         WHERE 1 = 1`
	if err := s.appendVectorFilters(&b, opts); err != nil {
		return b, err
	}
	b.write(` ORDER BY `+dist, qv)
	b.q += cast
	b.write(` LIMIT `, memstore.FetchLimit(opts)*annOverfetch)
	b.q = `SELECT * FROM (SELECT DISTINCT ON (id) * FROM (` + b.q + `) n ORDER BY id, similarity DESC) t ORDER BY similarity DESC`
	b.write(` LIMIT `, memstore.FetchLimit(opts))
	return b, nil
}

// applyANNSettings sets, for the transaction only, what the index-driven query
// needs. Iterative scans keep reading the index until enough rows pass the
// filters. Sequential scans are off because below a few tens of thousands of
// rows the planner prefers a scan and sort to the index, which is the exact
// cost this path exists to avoid; the threshold, not the planner, decides.
func applyANNSettings(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx,
		`SELECT set_config('enable_seqscan', 'off', true),
		        set_config('hnsw.iterative_scan', 'relaxed_order', true),
		        set_config('hnsw.ef_search', $1, true)`, strconv.Itoa(annEfSearch)); err != nil {
		return fmt.Errorf("pgstore: ANN search settings: %w", err)
	}
	return nil
}
