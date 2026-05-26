package mcpserver

import (
	"testing"

	"github.com/matthewjhunter/memstore"
)

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
