package main

import (
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

func TestHintMinSimilarity(t *testing.T) {
	t.Setenv("MEMSTORE_HINT_MIN_SIMILARITY", "")
	if got, err := hintMinSimilarity(); err != nil || got != httpapi.DefaultHintMinSimilarity {
		t.Errorf("unset: got %v, %v; want the default", got, err)
	}
	for v, want := range map[string]float64{"0": 0, "0.45": 0.45, "1": 1} {
		t.Setenv("MEMSTORE_HINT_MIN_SIMILARITY", v)
		if got, err := hintMinSimilarity(); err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", v, got, err, want)
		}
	}
	// A value that would silently disable or invert the gate is refused.
	for _, v := range []string{"-0.1", "1.5", "NaN", "high"} {
		t.Setenv("MEMSTORE_HINT_MIN_SIMILARITY", v)
		if _, err := hintMinSimilarity(); err == nil {
			t.Errorf("%q: accepted, want an error", v)
		}
	}
}
