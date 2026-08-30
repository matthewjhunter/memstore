package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// runBackfillLinks creates the links the extraction path would have made for
// facts that already existed when the gate was corrected.
//
// The gate is read at one point in the live system: a newly extracted fact
// compared against its neighbours. Correcting it therefore changes new facts
// and leaves every older one alone, which is why 67% of the corpus had no
// links on 2026-08-30 at a rate flat across provenance -- not a mistuned
// gate, and not one ingestion path misbehaving, but a fix that never looked
// backwards.
//
// The gate defaults to the policy for the configured embedding model, so the
// backfill agrees with what extraction does now rather than with whatever
// was configured when a fact was stored.
func runBackfillLinks(args []string, out io.Writer) {
	fs := flag.NewFlagSet("backfill-links", flag.ExitOnError)
	pgDSN := fs.String("pg", "", "PostgreSQL DSN (defaults to MEMSTORE_PG_SECRET / config)")
	minSim := fs.Float64("min-sim", 0, "cosine gate (default: the configured model's calibrated LinkMinSim)")
	topK := fs.Int("top-k", 10, "neighbours considered per fact before the gate")
	maxPer := fs.Int("max-per-fact", 3, "new links one fact may gain, mirroring extraction's cap")
	apply := fs.Bool("apply", false, "write the links; without this the command only reports")
	show := fs.Int("show", 10, "candidate pairs to list (0 = all)")
	namespace := fs.String("namespace", defaultAdminNamespace(), namespaceFlagUsage)
	if _, err := parseAdminArgs(fs, args); err != nil {
		fail(err)
	}

	gate := *minSim
	model := ""
	if gate == 0 {
		cfg, err := memstore.EmbedConfigFromEnv()
		if err == nil {
			model = cfg.Model
		}
		pol := memstore.DefaultSimilarityPolicy(model)
		gate = pol.LinkMinSim
	}

	pool, closePool, err := openPool(*pgDSN)
	if err != nil {
		fail(err)
	}
	defer closePool()

	rep, err := pgstore.BackfillLinks(context.Background(), pool, *namespace, pgstore.BackfillLinksOpts{
		MinSim: gate, TopK: *topK, MaxPerFact: *maxPer, Apply: *apply, SampleLimit: *show,
	})
	if err != nil {
		fail(err)
	}
	fmt.Fprint(out, backfillLinksReport(rep, model))
}

func backfillLinksReport(rep pgstore.BackfillLinksReport, model string) string {
	gate := fmt.Sprintf("gate %.2f", rep.MinSim)
	if model != "" {
		gate += fmt.Sprintf(" (calibrated for %s)", model)
	}
	s := fmt.Sprintf("backfill-links: %d facts with a vector, %s\n", rep.Facts, gate)
	if rep.NoVector > 0 {
		s += fmt.Sprintf("  %d active facts have no vector and were not compared\n", rep.NoVector)
	}
	if rep.Candidates == 0 {
		return s + "\n  no unlinked pairs clear the gate; the graph is already what the gate implies\n"
	}

	verb := "would add"
	if rep.Applied {
		verb = "added"
	}
	s += fmt.Sprintf("  %s %d links\n", verb, rep.Candidates)

	s += "\nsimilarity distribution:\n"
	buckets := make([]string, 0, len(rep.Buckets))
	for b := range rep.Buckets {
		buckets = append(buckets, b)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(buckets)))
	for _, b := range buckets {
		s += fmt.Sprintf("  %-12s %d\n", b, rep.Buckets[b])
	}

	s += "\nstrongest pairs:\n"
	for _, c := range rep.Sample {
		s += fmt.Sprintf("  %6d -- %-6d  %.3f\n", c.SourceID, c.TargetID, c.Sim)
	}
	if rest := rep.Candidates - len(rep.Sample); rest > 0 {
		s += fmt.Sprintf("  ... and %d more (--show 0 for all)\n", rest)
	}

	s += "\n"
	if rep.Applied {
		s += fmt.Sprintf("Wrote %d bidirectional 'related' links, the shape extraction writes.\n", rep.Added)
		return s
	}
	s += "Nothing was written. --apply creates them as bidirectional 'related' links,\n"
	s += "the same shape the extraction path writes. Links are not embedded, so this\n"
	s += "costs no re-embedding.\n"
	return s
}
