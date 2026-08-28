package main

import (
	"math"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

func pairs(cos []float64, floor float64) []pgstore.SimilarityPair {
	out := make([]pgstore.SimilarityPair, len(cos))
	for i, c := range cos {
		out[i] = pgstore.SimilarityPair{Cosine: c, Floor: floor}
	}
	return out
}

// The link gate is the 25th percentile of what was linked, rounded to 0.05
// and held above the median floor; the supersede gate is the 20th percentile
// of what was superseded, rounded up to 0.05. These are the rules applied by
// hand on 2026-08-28 (gemma: linked p25 0.496 / floor 0.484 -> 0.50;
// supersession p10 0.73, p25 0.82 -> 0.80).
func TestRecommendGates(t *testing.T) {
	s := pgstore.SimilaritySample{
		Linked:     pairs([]float64{0.45, 0.48, 0.50, 0.52, 0.53, 0.55, 0.58, 0.60}, 0.484),
		Superseded: pairs([]float64{0.70, 0.75, 0.82, 0.86, 0.90, 0.92, 0.94, 0.96}, 0),
	}
	link, sup := recommendGates(s)
	if math.Abs(link-0.50) > 1e-9 {
		t.Errorf("link gate = %.2f, want 0.50", link)
	}
	if math.Abs(sup-0.80) > 1e-9 {
		t.Errorf("supersede gate = %.2f, want 0.80", sup)
	}

	// A floor above the linked p25 wins: the gate must not admit background.
	s.Linked = pairs([]float64{0.45, 0.48, 0.50, 0.52}, 0.55)
	if link, _ = recommendGates(s); link < 0.55 {
		t.Errorf("link gate %.2f sits below the floor 0.55", link)
	}

	// Nothing to measure: no recommendation, not a zero gate.
	if link, sup = recommendGates(pgstore.SimilaritySample{}); link != 0 || sup != 0 {
		t.Errorf("empty sample recommended %.2f / %.2f, want 0 / 0", link, sup)
	}
}

func TestCalibrationReport_NamesCurrentAndRecommended(t *testing.T) {
	s := pgstore.SimilaritySample{
		Model:      "embeddinggemma",
		Linked:     pairs([]float64{0.45, 0.48, 0.50, 0.52, 0.53, 0.55, 0.58, 0.60}, 0.484),
		Superseded: pairs([]float64{0.70, 0.75, 0.82, 0.86, 0.90, 0.92, 0.94, 0.96}, 0),
	}
	out := calibrationReport(s, memstore.DefaultSimilarityPolicy(s.Model))
	for _, want := range []string{"embeddinggemma", "linked pairs", "stored-to-stored", "n=8", "supersession pairs", "current: link>=0.50 supersede>=0.85", "recommended: link>=0.50 supersede>=0.80", "MEMSTORE_LINK_MIN_SIM"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
