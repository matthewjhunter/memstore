package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// The tool reports and cannot change anything. Its input type is empty, so
// "cannot" is a compile-time fact rather than a runtime one -- there is no
// field to pass and no branch to forget.
//
// It lost its setter because a stateless server cannot honour one: every
// request builds a fresh server, so a setting made by one call would be gone by
// the next. A tool that silently forgets is worse than one that never offered.
func TestRerankSettingsReportsTheConfiguredPolicy(t *testing.T) {
	ms := NewMemoryServerWithConfig(nil, nil, Config{
		RerankMode:             memstore.RerankDominant,
		RerankThreshold:        0.25,
		RerankCandidates:       48,
		RerankRecallCandidates: 12,
		RerankDocBytes:         2000,
		RerankRecallDocBytes:   900,
	})

	res, out, err := ms.HandleRerankSettings(context.Background(), nil, RerankSettingsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("reporting the policy should not error")
	}

	if out.Mode != "dominant" || out.Threshold != 0.25 {
		t.Errorf("mode/threshold = (%s, %v), want (dominant, 0.25)", out.Mode, out.Threshold)
	}
	if out.SearchCandidates != 48 || out.RecallCandidates != 12 {
		t.Errorf("candidate pools = (%d, %d), want (48, 12)", out.SearchCandidates, out.RecallCandidates)
	}
	if out.SearchDocBytes != 2000 || out.RecallDocBytes != 900 {
		t.Errorf("doc budgets = (%d, %d), want (2000, 900)", out.SearchDocBytes, out.RecallDocBytes)
	}
	if out.Timeout != "none" {
		t.Errorf("timeout = %q, want none", out.Timeout)
	}

	// Reporting twice reports the same thing: nothing about the call changed it.
	_, again, _ := ms.HandleRerankSettings(context.Background(), nil, RerankSettingsInput{})
	if again != out {
		t.Errorf("a second report differs from the first:\n %+v\n %+v", again, out)
	}
}

// A zero mode and zero pools read as "off" and "default" rather than as
// numbers, so an operator reading the report is not told the engine defaults
// are zero.
func TestRerankSettingsNamesTheAbsentValues(t *testing.T) {
	ms := NewMemoryServerWithConfig(nil, nil, Config{})

	_, out, _ := ms.HandleRerankSettings(context.Background(), nil, RerankSettingsInput{})
	if out.Mode != "off" || out.Timeout != "none" {
		t.Errorf("mode/timeout = (%q, %q), want (off, none)", out.Mode, out.Timeout)
	}

	report := ms.tunablesReport()
	for _, want := range []string{"mode=off", "search_candidates=default", "recall_candidates=default", "search_doc_bytes=default", "recall_doc_bytes=default", "timeout=none"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q missing %q", report, want)
		}
	}
}

// A per-call override applies to that call and leaves the server's policy
// alone. This is the property the whole demotion rests on: overriding is how a
// caller retrieves differently now, and it has to be free of side effects, or
// one caller's threshold would follow the next one around.
func TestPerCallOverridesDoNotPersist(t *testing.T) {
	ms := NewMemoryServerWithConfig(nil, nil, Config{
		RerankMode: memstore.RerankBalanced, RerankThreshold: 0.4,
	})

	high := 0.9
	if m, th := ms.resolveRerank("dominant", &high); m != memstore.RerankDominant || th != 0.9 {
		t.Fatalf("override: got (%s, %v), want (dominant, 0.9)", m, th)
	}
	if m, th := ms.resolveRerank("", nil); m != memstore.RerankBalanced || th != 0.4 {
		t.Errorf("the override persisted: got (%s, %v), want (balanced, 0.4)", m, th)
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
