package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

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
	ms := NewMemoryServerWithConfig(nil, Config{
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
	ms := NewMemoryServerWithConfig(nil, Config{})

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

// The per-call knobs are the whole tuning surface now, so each one has to
// actually reach the search options -- and none of them may outlive the call.
func TestPerCallOverridesApplyAndDoNotPersist(t *testing.T) {
	base := rerankTunables{
		mode: memstore.RerankBalanced, threshold: 0.4, weight: 0.3,
		searchCandidates: 40, searchDocBytes: 2800, timeout: 2 * time.Second,
	}

	got, err := base.search().with(rerankOverrides{
		mode:           "dominant",
		threshold:      ptr(0.9),
		weight:         ptr(0.75),
		candidates:     ptr(8),
		docBytes:       ptr(512),
		timeoutSeconds: ptr(0.5),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := effectiveRerank{memstore.RerankDominant, 0.9, 0.75, 8, 512, 500 * time.Millisecond}
	if got != want {
		t.Errorf("overrides not applied\n got: %+v\nwant: %+v", got, want)
	}

	// The same base, asked again with nothing: the previous call left no trace.
	if again := base.search(); again != (effectiveRerank{memstore.RerankBalanced, 0.4, 0.3, 40, 2800, 2 * time.Second}) {
		t.Errorf("an override persisted into the defaults: %+v", again)
	}
}

// Omitted fields fall back to what the daemon is configured with, and zero is
// not omitted: a zero threshold means "no floor" and a zero deadline means "no
// deadline", both of which a caller must be able to ask for.
func TestOmittedKnobsFallBackAndZeroIsAValue(t *testing.T) {
	base := rerankTunables{mode: memstore.RerankGate, threshold: 0.4, timeout: time.Second}

	if got, _ := base.recall().with(rerankOverrides{}); got.threshold != 0.4 || got.mode != memstore.RerankGate || got.timeout != time.Second {
		t.Errorf("omitted knobs did not fall back: %+v", got)
	}
	got, err := base.recall().with(rerankOverrides{threshold: ptr(0.0), timeoutSeconds: ptr(0.0)})
	if err != nil {
		t.Fatal(err)
	}
	if got.threshold != 0 || got.timeout != 0 {
		t.Errorf("explicit zeros were treated as unset: %+v", got)
	}
}

// search and recall differ in the two knobs that were ever per-tool.
func TestSearchAndRecallCarryTheirOwnBudgets(t *testing.T) {
	base := rerankTunables{
		searchCandidates: 40, recallCandidates: 8,
		searchDocBytes: 2800, recallDocBytes: 1200,
	}
	if s := base.search(); s.candidates != 40 || s.docBytes != 2800 {
		t.Errorf("search budgets = (%d, %d), want (40, 2800)", s.candidates, s.docBytes)
	}
	if r := base.recall(); r.candidates != 8 || r.docBytes != 1200 {
		t.Errorf("recall budgets = (%d, %d), want (8, 1200)", r.candidates, r.docBytes)
	}
}

// An invalid knob is refused, not ignored. The setter this replaced validated
// its arguments; dropping that when the knobs moved onto the call would mean a
// caller that mistypes gets different retrieval than it asked for, silently,
// and the result is indistinguishable from the store holding different things.
func TestInvalidOverridesAreRefused(t *testing.T) {
	base := rerankTunables{mode: memstore.RerankBalanced, threshold: 0.4}

	for name, o := range map[string]rerankOverrides{
		"unknown mode":      {mode: "sideways"},
		"threshold above 1": {threshold: ptr(1.5)},
		"threshold below 0": {threshold: ptr(-0.1)},
		"weight above 1":    {weight: ptr(2.0)},
		"negative pool":     {candidates: ptr(-1)},
		"negative bytes":    {docBytes: ptr(-1)},
		"negative deadline": {timeoutSeconds: ptr(-1.0)},
	} {
		if _, err := base.search().with(o); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

// ptr returns a pointer to v, for the optional per-call knobs.
func ptr[T any](v T) *T { return &v }
