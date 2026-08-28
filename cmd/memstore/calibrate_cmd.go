package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// --- calibrate-similarity ---

// runCalibrateSimilarity measures the corpus the way the auto-link and
// auto-supersede gates see it and prints where the gates would sit for the
// model that produced the stored vectors. Run it after a re-embed, and set
// MEMSTORE_LINK_MIN_SIM / MEMSTORE_SUPERSEDE_MIN_SIM (or add the model to the
// per-model defaults in similarity.go) from what it reports.
func runCalibrateSimilarity(args []string, out io.Writer) {
	fs := flag.NewFlagSet("calibrate-similarity", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	sample := fs.Int("sample", 60, "pairs of each kind to measure")
	floorRank := fs.Int("floor-rank", 50, "neighbour rank whose similarity is reported as the background floor")
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}
	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()
	ctx := context.Background()

	var model string
	if err := pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&model); err != nil {
		fail(fmt.Errorf("calibrate-similarity: no embedding model recorded; nothing has been embedded yet"))
	}
	queryEmbed, why := queryEmbedderFor(model)
	if queryEmbed == nil {
		fmt.Fprintf(os.Stderr, "calibrate-similarity: %s; scoring linked pairs stored-to-stored, which overstates the link gate's view on a prefixed model\n", why)
	}

	s, err := pgstore.SampleSimilarity(ctx, pool, *namespace, *sample, *floorRank, queryEmbed)
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, calibrationReport(s, memstore.DefaultSimilarityPolicy(s.Model)))
}

// queryEmbedderFor builds the query-side embedder for the link measurement
// from the same EMBEDDING_* environment memstored reads, provided it names
// the model the stored vectors came from. Returns nil, with the reason, when
// it cannot: the caller then measures stored-to-stored and says so.
func queryEmbedderFor(storedModel string) (pgstore.QueryEmbedFunc, string) {
	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		return nil, "embedder config: " + err.Error()
	}
	want, _ := embedding.LookupModel(storedModel)
	have, _ := embedding.LookupModel(cfg.Model)
	if want.Canonical != have.Canonical {
		return nil, fmt.Sprintf("EMBEDDING_MODEL is %q but the stored vectors are from %q (set the EMBEDDING_* variables memstored uses)", cfg.Model, storedModel)
	}
	emb, err := embedding.New(cfg)
	if err != nil {
		return nil, "create embedder: " + err.Error()
	}
	return func(ctx context.Context, content string) ([]float32, error) {
		return embedding.Single(ctx, emb, memstore.FactQueryText(emb.Model(), content))
	}, ""
}

// recommendGates derives gates from a sample. The link gate is the 25th
// percentile of the cosines of pairs that were linked, rounded to the nearest
// 0.05, and never below the median background floor: keeping three quarters
// of what the previous gate admitted while staying out of the noise is the
// rule that produced 0.50 for embeddinggemma. The supersede gate is the 20th
// percentile of superseded-to-successor cosines rounded UP to 0.05, because
// supersession hides a live fact and the tail of that distribution is
// legitimate rewrites, not duplicates. Either gate is 0 when there is nothing
// to measure it from.
func recommendGates(s pgstore.SimilaritySample) (link, supersede float64) {
	if len(s.Linked) > 0 {
		cos := make([]float64, 0, len(s.Linked))
		floors := make([]float64, 0, len(s.Linked))
		for _, p := range s.Linked {
			cos = append(cos, p.Cosine)
			if p.Floor > 0 {
				floors = append(floors, p.Floor)
			}
		}
		link = math.Round(quantile(cos, 0.25)*20) / 20
		if len(floors) > 0 {
			if floor := math.Ceil(quantile(floors, 0.5)*20) / 20; link < floor {
				link = floor
			}
		}
	}
	if len(s.Superseded) > 0 {
		cos := make([]float64, 0, len(s.Superseded))
		for _, p := range s.Superseded {
			cos = append(cos, p.Cosine)
		}
		supersede = math.Ceil(quantile(cos, 0.20)*20) / 20
	}
	return link, supersede
}

// calibrationReport renders the sample, the gates in force for its model, and
// the recommendation, as text for the operator.
func calibrationReport(s pgstore.SimilaritySample, current memstore.SimilarityPolicy) string {
	var b strings.Builder
	model := s.Model
	if model == "" {
		model = "(no embedding model recorded)"
	}
	fmt.Fprintf(&b, "similarity calibration for %s\n\n", model)

	dist := func(name string, ps []pgstore.SimilarityPair) []float64 {
		cos := make([]float64, 0, len(ps))
		for _, p := range ps {
			cos = append(cos, p.Cosine)
		}
		if len(cos) == 0 {
			fmt.Fprintf(&b, "%s: none found\n", name)
			return nil
		}
		sort.Float64s(cos)
		fmt.Fprintf(&b, "%s: n=%d  min=%.3f  p10=%.3f  p25=%.3f  median=%.3f  p75=%.3f  max=%.3f\n",
			name, len(cos), cos[0], quantile(cos, 0.10), quantile(cos, 0.25), quantile(cos, 0.5), quantile(cos, 0.75), cos[len(cos)-1])
		return cos
	}
	linkedLabel := "linked pairs (query-side source vs stored target, the gate's view)"
	if !s.LinkedQuerySide {
		linkedLabel = "linked pairs (stored-to-stored; overstates the gate's view on a prefixed model)"
	}
	linked := dist(linkedLabel, s.Linked)
	if linked != nil {
		floors := make([]float64, 0, len(s.Linked))
		for _, p := range s.Linked {
			if p.Floor > 0 {
				floors = append(floors, p.Floor)
			}
		}
		if len(floors) > 0 {
			fmt.Fprintf(&b, "  background floor (rank-N neighbour of each source): median=%.3f\n", quantile(floors, 0.5))
		}
		fmt.Fprintf(&b, "  admitted by the current link gate %.2f: %d/%d\n", current.LinkMinSim, countAtLeast(linked, current.LinkMinSim), len(linked))
	}
	superseded := dist("supersession pairs", s.Superseded)
	if superseded != nil {
		fmt.Fprintf(&b, "  admitted by the current supersede gate %.2f: %d/%d\n", current.SupersedeMinSim, countAtLeast(superseded, current.SupersedeMinSim), len(superseded))
	}

	fmt.Fprintf(&b, "\ncurrent: link>=%.2f supersede>=%.2f (calibrated=%t)\n", current.LinkMinSim, current.SupersedeMinSim, current.Calibrated)
	link, sup := recommendGates(s)
	switch {
	case link == 0 && sup == 0:
		fmt.Fprintln(&b, "recommended: nothing to measure yet -- links and supersessions accumulate as extraction runs")
	default:
		fmt.Fprintf(&b, "recommended: link>=%.2f supersede>=%.2f\n", link, sup)
		fmt.Fprintf(&b, "apply with MEMSTORE_LINK_MIN_SIM=%.2f MEMSTORE_SUPERSEDE_MIN_SIM=%.2f, or add %q to the per-model defaults in similarity.go\n", link, sup, model)
	}
	fmt.Fprintln(&b, "\nlinked pairs were admitted by whatever gate was in force when they were made, so the sample leans toward that gate; supersession pairs include deliberate rewrites, which is why the supersede recommendation stays above the tail.")
	return b.String()
}

// quantile returns the q-th quantile of sorted (or unsorted; it sorts a
// copy) values by linear interpolation.
func quantile(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	v := append([]float64(nil), vals...)
	sort.Float64s(v)
	pos := q * float64(len(v)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return v[lo]
	}
	return v[lo] + (v[hi]-v[lo])*(pos-float64(lo))
}

func countAtLeast(sorted []float64, gate float64) int {
	n := 0
	for _, v := range sorted {
		if v >= gate {
			n++
		}
	}
	return n
}
