package memstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/teststore"
	_ "modernc.org/sqlite"
)

// fixtureCreated is the created_at the fixture's facts were written with.
var fixtureCreated = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

// openFixture opens testdata/export-source.sqlite read-only. The file was
// written by memstore 0.5.0's SQLite backend, which no longer exists in the
// tree: two namespaces (alpha with a supersession, a bidirectional link with
// metadata, and one confirmation; beta with one fact), owned by
// "fixture-user". It is what the export reader has to keep reading until
// 0.7.0 drops it.
func openFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:testdata/export-source.sqlite?mode=ro")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exportFixture(t *testing.T) *memstore.ExportData {
	t.Helper()
	data, err := memstore.Export(context.Background(), openFixture(t))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return data
}

func factByContent(facts []memstore.ExportedFact, content string) *memstore.ExportedFact {
	for i := range facts {
		if facts[i].Content == content {
			return &facts[i]
		}
	}
	return nil
}

func TestExport_ReadsPreDeletionSQLite(t *testing.T) {
	data := exportFixture(t)
	if data.Version != 1 {
		t.Errorf("version = %d, want 1", data.Version)
	}
	if len(data.Facts) != 3 {
		t.Fatalf("exported %d facts, want 3 (superseded included)", len(data.Facts))
	}
	if len(data.Links) != 1 {
		t.Fatalf("exported %d links, want 1", len(data.Links))
	}

	old := factByContent(data.Facts, "Alpha old fact")
	newer := factByContent(data.Facts, "Alpha new fact")
	beta := factByContent(data.Facts, "Beta fact")
	if old == nil || newer == nil || beta == nil {
		t.Fatal("fixture facts missing from export")
	}
	if old.Namespace != "alpha" || beta.Namespace != "beta" {
		t.Errorf("namespaces = %q/%q, want alpha/beta", old.Namespace, beta.Namespace)
	}
	if old.User != "fixture-user" {
		t.Errorf("user = %q, want fixture-user", old.User)
	}
	if old.SupersededBy == nil || *old.SupersededBy != newer.ID || old.SupersededAt == nil {
		t.Errorf("supersession not exported: %+v", old)
	}
	if old.Kind != "convention" || old.Subsystem != "auth" || old.Category != "preference" {
		t.Errorf("classification: kind=%q subsystem=%q category=%q", old.Kind, old.Subsystem, old.Category)
	}
	var m map[string]any
	if err := json.Unmarshal(old.Metadata, &m); err != nil || m["source"] != "test" || m["chapter"] != 3.0 {
		t.Errorf("metadata = %s (err %v)", old.Metadata, err)
	}
	if !old.CreatedAt.Equal(fixtureCreated) {
		t.Errorf("created_at = %v, want %v", old.CreatedAt, fixtureCreated)
	}
	if newer.ConfirmedCount != 1 {
		t.Errorf("confirmed_count = %d, want 1", newer.ConfirmedCount)
	}

	l := data.Links[0]
	if l.SourceID != newer.ID || l.TargetID != old.ID || l.Namespace != "alpha" ||
		l.LinkType != "related" || !l.Bidirectional || l.Label != "seeded" {
		t.Errorf("link = %+v", l)
	}
	var lm map[string]any
	if err := json.Unmarshal(l.Metadata, &lm); err != nil || lm["w"] != 1.0 {
		t.Errorf("link metadata = %s (err %v)", l.Metadata, err)
	}
}

// TestStoreImport_RoundTripFromFixture is the migration path itself: a 0.5.x
// SQLite export lands in a daemon store with supersession and links on the
// new ids and created_at kept.
func TestStoreImport_RoundTripFromFixture(t *testing.T) {
	ctx := context.Background()
	data := exportFixture(t)
	dst := teststore.New(t, nil, "daemon")

	res, err := memstore.StoreImport(ctx, dst, data, memstore.ImportOpts{})
	if err != nil {
		t.Fatalf("StoreImport: %v", err)
	}
	if res.Imported != 3 || res.Skipped != 0 || res.Links != 1 || res.LinksSkipped != 0 {
		t.Fatalf("result = %+v, want 3 facts and 1 link", res)
	}

	all, err := dst.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("daemon holds %d facts, want 3 (both namespaces land in one)", len(all))
	}
	var old, newer *memstore.Fact
	for i := range all {
		switch all[i].Content {
		case "Alpha old fact":
			old = &all[i]
		case "Alpha new fact":
			newer = &all[i]
		}
	}
	if old == nil || newer == nil {
		t.Fatal("alpha facts did not arrive")
	}
	if old.SupersededBy == nil || *old.SupersededBy != newer.ID {
		t.Errorf("supersession not restored on the new ids: %+v", old.SupersededBy)
	}
	if !old.CreatedAt.Equal(fixtureCreated) {
		t.Errorf("created_at = %v, want %v preserved", old.CreatedAt, fixtureCreated)
	}
	if old.Kind != "convention" || old.Subsystem != "auth" {
		t.Errorf("classification lost: kind=%q subsystem=%q", old.Kind, old.Subsystem)
	}
	var m map[string]any
	if err := json.Unmarshal(old.Metadata, &m); err != nil || m["source"] != "test" {
		t.Errorf("metadata = %s (err %v)", old.Metadata, err)
	}
	if old.UserID == 0 {
		t.Error("imported fact has no owner")
	}

	links, err := dst.GetLinks(ctx, newer.ID, memstore.LinkOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].TargetID != old.ID || links[0].LinkType != "related" || !links[0].Bidirectional || links[0].Label != "seeded" {
		t.Errorf("links from the new fact = %+v, want one related bidirectional link to %d", links, old.ID)
	}
}

func TestStoreImport_SkipDuplicates(t *testing.T) {
	ctx := context.Background()
	data := exportFixture(t)
	dst := teststore.New(t, nil, "daemon")

	r1, err := memstore.StoreImport(ctx, dst, data, memstore.ImportOpts{SkipDuplicates: true})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Imported != 3 || r1.Skipped != 0 {
		t.Fatalf("first import = %+v, want 3 imported", r1)
	}
	r2, err := memstore.StoreImport(ctx, dst, data, memstore.ImportOpts{SkipDuplicates: true})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Imported != 0 || r2.Skipped != 3 {
		t.Errorf("second import = %+v, want 3 skipped", r2)
	}
	// Every endpoint was skipped, so the link had nowhere to land.
	if r2.Links != 0 || r2.LinksSkipped != 1 {
		t.Errorf("second import links = %d/%d skipped, want 0/1", r2.Links, r2.LinksSkipped)
	}
}

func TestStoreImport_VersionCheck(t *testing.T) {
	dst := teststore.New(t, nil, "daemon")
	data := &memstore.ExportData{Version: 2}
	if _, err := memstore.StoreImport(context.Background(), dst, data, memstore.ImportOpts{}); err == nil {
		t.Error("expected an error for an unsupported export version")
	}
}

// TestStoreImport_LinksRemappedAndDanglingSkipped: a link whose endpoint was
// skipped as a duplicate is counted, not failed.
func TestStoreImport_LinksRemappedAndDanglingSkipped(t *testing.T) {
	ctx := context.Background()
	data := &memstore.ExportData{
		Version: 1,
		Facts: []memstore.ExportedFact{
			{ID: 10, Namespace: "laptop", Content: "fact a", Subject: "s", Category: "note", CreatedAt: fixtureCreated},
			{ID: 11, Namespace: "laptop", Content: "fact b", Subject: "s", Category: "note", CreatedAt: fixtureCreated},
			{ID: 12, Namespace: "laptop", Content: "fact c already there", Subject: "s", Category: "note", CreatedAt: fixtureCreated},
		},
		Links: []memstore.ExportedLink{
			{ID: 1, Namespace: "laptop", SourceID: 10, TargetID: 11, LinkType: "related", Label: "kept"},
			{ID: 2, Namespace: "laptop", SourceID: 11, TargetID: 12, LinkType: "related", Label: "dangling"},
		},
	}
	dst := teststore.New(t, nil, "daemon")
	if _, err := dst.Insert(ctx, memstore.Fact{Content: "fact c already there", Subject: "s", Category: "note"}); err != nil {
		t.Fatal(err)
	}

	res, err := memstore.StoreImport(ctx, dst, data, memstore.ImportOpts{SkipDuplicates: true})
	if err != nil {
		t.Fatalf("StoreImport: %v", err)
	}
	if res.Imported != 2 || res.Skipped != 1 {
		t.Fatalf("facts: %+v, want 2 imported 1 skipped", res)
	}
	if res.Links != 1 || res.LinksSkipped != 1 {
		t.Errorf("links: %+v, want 1 imported 1 skipped", res)
	}
	all, err := dst.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var newA int64
	for _, f := range all {
		if f.Content == "fact a" {
			newA = f.ID
		}
	}
	links, err := dst.GetLinks(ctx, newA, memstore.LinkOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Label != "kept" {
		t.Errorf("links from a = %+v, want the one labelled kept", links)
	}
}

// TestStoreImport_LegacyExportNoUser: an export written before facts carried
// an owner (no "user" field) still imports, and the facts land on the
// target's default user.
func TestStoreImport_LegacyExportNoUser(t *testing.T) {
	ctx := context.Background()
	data := &memstore.ExportData{
		Version: 1,
		Facts: []memstore.ExportedFact{
			{ID: 1, Namespace: "old", Content: "legacy fact", Subject: "s", Category: "note", CreatedAt: fixtureCreated},
		},
	}
	dst := teststore.New(t, nil, "daemon")
	res, err := memstore.StoreImport(ctx, dst, data, memstore.ImportOpts{})
	if err != nil {
		t.Fatalf("StoreImport: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("result = %+v", res)
	}
	all, err := dst.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].UserID == 0 {
		t.Errorf("legacy fact = %+v, want one fact with an owner", all)
	}
}
