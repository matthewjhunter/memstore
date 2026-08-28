package memstore_test

import (
	"testing"

	"github.com/matthewjhunter/memstore"
)

func TestDefaultSimilarityPolicy_KeyedByCanonicalModel(t *testing.T) {
	cases := []struct {
		model            string
		link, supersede  float64
		calibrated       bool
	}{
		{"nomic-embed-text", 0.60, 0.85, true},
		{"nomic-embed-text:latest", 0.60, 0.85, true},
		{"embeddinggemma", 0.50, 0.85, true},
		{"embeddinggemma:300m", 0.50, 0.85, true},
		// Unknown model: the historical constants, flagged as uncalibrated so
		// startup can say so.
		{"some-new-model", 0.60, 0.85, false},
	}
	for _, c := range cases {
		p := memstore.DefaultSimilarityPolicy(c.model)
		if p.LinkMinSim != c.link || p.SupersedeMinSim != c.supersede || p.Calibrated != c.calibrated {
			t.Errorf("%s: got link=%.2f supersede=%.2f calibrated=%t, want %.2f %.2f %t",
				c.model, p.LinkMinSim, p.SupersedeMinSim, p.Calibrated, c.link, c.supersede, c.calibrated)
		}
	}
}

func TestSimilarityPolicyFromEnv_OverridesOneGateAtATime(t *testing.T) {
	t.Setenv("MEMSTORE_LINK_MIN_SIM", "0.42")
	p, err := memstore.SimilarityPolicyFromEnv("MEMSTORE", "embeddinggemma")
	if err != nil {
		t.Fatal(err)
	}
	if p.LinkMinSim != 0.42 || p.SupersedeMinSim != 0.85 || !p.Calibrated {
		t.Fatalf("got %+v, want link=0.42 supersede=0.85 calibrated", p)
	}

	t.Setenv("MEMSTORE_LINK_MIN_SIM", "")
	t.Setenv("MEMSTORE_SUPERSEDE_MIN_SIM", "0.9")
	p, err = memstore.SimilarityPolicyFromEnv("MEMSTORE", "some-new-model")
	if err != nil {
		t.Fatal(err)
	}
	if p.LinkMinSim != 0.60 || p.SupersedeMinSim != 0.9 {
		t.Fatalf("got %+v, want link=0.60 supersede=0.9", p)
	}
}

func TestSimilarityPolicyFromEnv_RejectsBadValues(t *testing.T) {
	for _, bad := range []string{"abc", "-0.1", "1.5"} {
		t.Setenv("MEMSTORE_LINK_MIN_SIM", bad)
		if _, err := memstore.SimilarityPolicyFromEnv("MEMSTORE", "embeddinggemma"); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}
