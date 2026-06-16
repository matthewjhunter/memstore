package httpapi_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/infodancer/smoke"
	"github.com/matthewjhunter/memstore"
)

// TestSmokeManifestCoverage is the coverage gate: every route the handler
// registers must carry enough smoke metadata to be probed -- a path parameter
// needs an Example, or the route needs a Skip/Write. A new route added without
// one fails here, forcing a smoke spec at registration time.
func TestSmokeManifestCoverage(t *testing.T) {
	h, _ := newTestHandler(t)
	m := h.Manifest()
	if len(m.Routes) == 0 {
		t.Fatal("no routes recorded in the smoke manifest")
	}
	for _, r := range m.Incomplete() {
		t.Errorf("route %s %s lacks smoke coverage: add a smoke.Example for its path params, or a smoke.Skip/smoke.Write",
			r.EffectiveMethod(), r.Pattern)
	}
}

// TestSmokeProbeReadRoutes black-box probes every read route against a live
// handler, catching wiring regressions that unit tests miss (a route that
// registers but 500s, an example that points at absent data). Writes and
// body-requiring POSTs are skipped by their specs; only ReadOnly routes run,
// serially, against a fresh in-memory store seeded to match the Example values.
func TestSmokeProbeReadRoutes(t *testing.T) {
	h, store := newTestHandler(t)
	ctx := context.Background()

	// Seed fixtures matching the registered Example values: fact id 1 (probed by
	// GET /v1/facts/{id}, .../history, .../links) and link id 1 (GET
	// /v1/links/{id}). A fresh in-memory SQLite store autoincrements from 1.
	id1, err := store.Insert(ctx, memstore.Fact{Content: "smoke fact one", Subject: "smoke"})
	if err != nil {
		t.Fatalf("seed fact 1: %v", err)
	}
	id2, err := store.Insert(ctx, memstore.Fact{Content: "smoke fact two", Subject: "smoke"})
	if err != nil {
		t.Fatalf("seed fact 2: %v", err)
	}
	if _, err := store.LinkFacts(ctx, id1, id2, "relates", false, "", nil); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	srv := httptest.NewServer(h) // newTestHandler builds with auth disabled
	defer srv.Close()

	report, err := smoke.Run(ctx, h.Manifest(), smoke.RunOptions{
		BaseURL:       srv.URL,
		Target:        smoke.Preview, // reads + writes eligible...
		IncludeWrites: false,         // ...but writes stay off, so only reads probe
		Concurrency:   1,             // serial: in-memory SQLite is single-connection
	})
	if err != nil {
		t.Fatalf("smoke run: %v", err)
	}

	for _, r := range report.Failed() {
		t.Errorf("probe %s %s -> %s (status %d): %s", r.Method, r.URL, r.Outcome, r.Status, r.Reason)
	}
	pass, fail, skip := report.Counts()
	t.Logf("smoke probe: %d pass, %d fail, %d skip", pass, fail, skip)
	if pass == 0 {
		t.Error("no read routes were probed; check fixtures and route specs")
	}
}
