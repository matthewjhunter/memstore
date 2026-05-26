package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

func ptrF(f float64) *float64 { return &f }
func ptrI(i int) *int         { return &i }

func TestHandleRerankSettings_SetsAllTunables(t *testing.T) {
	ms := &MemoryServer{}
	res, _, err := ms.HandleRerankSettings(context.Background(), nil, RerankSettingsInput{
		Mode:             "dominant",
		Threshold:        ptrF(0.25),
		Weight:           ptrF(0.8),
		SearchCandidates: ptrI(32),
		RecallCandidates: ptrI(10),
		TimeoutSeconds:   ptrF(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result")
	}
	got := ms.tunables()
	if got.mode != memstore.RerankDominant || got.threshold != 0.25 || got.weight != 0.8 ||
		got.searchCandidates != 32 || got.recallCandidates != 10 || got.timeout != 4*time.Second {
		t.Errorf("tunables not all applied: %+v", got)
	}

	// Omitted fields are left unchanged; only threshold moves.
	if _, _, err := ms.HandleRerankSettings(context.Background(), nil, RerankSettingsInput{Threshold: ptrF(0.5)}); err != nil {
		t.Fatal(err)
	}
	if g := ms.tunables(); g.threshold != 0.5 || g.searchCandidates != 32 || g.mode != memstore.RerankDominant {
		t.Errorf("omitted fields should persist; got %+v", g)
	}
}

func TestHandleRerankSettings_Validates(t *testing.T) {
	ms := &MemoryServer{}
	for _, in := range []RerankSettingsInput{
		{Weight: ptrF(1.5)},
		{Threshold: ptrF(-0.1)},
		{SearchCandidates: ptrI(-1)},
		{TimeoutSeconds: ptrF(-2)},
	} {
		res, _, _ := ms.HandleRerankSettings(context.Background(), nil, in)
		if !res.IsError {
			t.Errorf("input %+v should be rejected", in)
		}
	}
	// A rejected call must not have mutated state.
	if g := ms.tunables(); g.weight != 0 || g.searchCandidates != 0 {
		t.Errorf("state mutated despite validation error: %+v", g)
	}
}

func TestHandleRerankSettings_ReportsCurrent(t *testing.T) {
	ms := &MemoryServer{}
	// A no-arg call reports defaults without mutating anything.
	if res, _, _ := ms.HandleRerankSettings(context.Background(), nil, RerankSettingsInput{}); res.IsError {
		t.Fatal("no-arg get should not error")
	}
	report := ms.tunablesReport()
	for _, want := range []string{"mode=off", "search_candidates=default", "recall_candidates=default", "timeout=none"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q missing %q", report, want)
		}
	}
}

func TestResolveRerank(t *testing.T) {
	ms := &MemoryServer{rerankMode: memstore.RerankBalanced, rerankThreshold: 0.4}

	// No overrides → server defaults.
	if m, th := ms.resolveRerank("", nil); m != memstore.RerankBalanced || th != 0.4 {
		t.Errorf("defaults: got (%s, %v), want (balanced, 0.4)", m, th)
	}
	// Mode override only.
	if m, th := ms.resolveRerank("dominant", nil); m != memstore.RerankDominant || th != 0.4 {
		t.Errorf("mode override: got (%s, %v), want (dominant, 0.4)", m, th)
	}
	// Threshold override including explicit 0 (a pointer distinguishes unset).
	z := 0.0
	if m, th := ms.resolveRerank("", &z); m != memstore.RerankBalanced || th != 0 {
		t.Errorf("threshold 0 override: got (%s, %v), want (balanced, 0)", m, th)
	}
	// An unparseable mode is ignored, leaving the default.
	if m, _ := ms.resolveRerank("bogus", nil); m != memstore.RerankBalanced {
		t.Errorf("invalid mode: got %s, want balanced (unchanged)", m)
	}
}

func TestSetRerankPolicy(t *testing.T) {
	ms := &MemoryServer{}
	ms.setRerankPolicy(memstore.RerankGate, 0.7)
	if m, th := ms.rerankPolicy(); m != memstore.RerankGate || th != 0.7 {
		t.Errorf("after set: got (%s, %v), want (gate, 0.7)", m, th)
	}
}
