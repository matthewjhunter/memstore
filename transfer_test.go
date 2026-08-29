package memstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestExportEmpty(t *testing.T) {
	db := openTestDB(t)
	if _, err := memstore.NewSQLiteStore(db, nil, ""); err != nil {
		t.Fatal(err)
	}

	data, err := memstore.Export(context.Background(), db)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if data.Version != 1 {
		t.Errorf("version = %d, want 1", data.Version)
	}
	if len(data.Facts) != 0 {
		t.Errorf("facts = %d, want 0", len(data.Facts))
	}
	if data.ExportedAt.IsZero() {
		t.Error("expected non-zero ExportedAt")
	}
}

func TestExportRoundTrip(t *testing.T) {
	srcDB := openTestDB(t)
	storeA, err := memstore.NewSQLiteStore(srcDB, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := memstore.NewSQLiteStore(srcDB, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	meta := json.RawMessage(`{"source":"test","chapter":3}`)
	created := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	idA1, err := storeA.Insert(ctx, memstore.Fact{
		Content: "Alpha old fact", Subject: "X", Category: "preference",
		Kind: "convention", Subsystem: "auth",
		Metadata: meta, CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	idA2, err := storeA.Insert(ctx, memstore.Fact{
		Content: "Alpha new fact", Subject: "X", Category: "preference",
		CreatedAt: created.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.Supersede(ctx, idA1, idA2); err != nil {
		t.Fatal(err)
	}
	// A link within alpha; its ids must be remapped on import along with
	// the facts, and its created_at kept.
	if _, err := storeA.LinkFacts(ctx, idA2, idA1, "related", true, "seeded", map[string]any{"w": 1.0}); err != nil {
		t.Fatal(err)
	}

	if _, err := storeB.Insert(ctx, memstore.Fact{
		Content: "Beta fact", Subject: "Y", Category: "identity",
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}

	// Export from source.
	data, err := memstore.Export(ctx, srcDB)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(data.Facts) != 3 {
		t.Fatalf("exported %d facts, want 3", len(data.Facts))
	}
	if len(data.Links) != 1 {
		t.Fatalf("exported %d links, want 1", len(data.Links))
	}
	srcLink := data.Links[0]
	if srcLink.SourceID != idA2 || srcLink.TargetID != idA1 || srcLink.Namespace != "alpha" ||
		srcLink.LinkType != "related" || !srcLink.Bidirectional || srcLink.Label != "seeded" {
		t.Errorf("exported link = %+v", srcLink)
	}

	// Import into fresh DB.
	dstDB := openTestDB(t)
	if _, err := memstore.NewSQLiteStore(dstDB, nil, ""); err != nil {
		t.Fatal(err)
	}

	result, err := memstore.Import(ctx, dstDB, data, memstore.ImportOpts{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.Imported != 3 {
		t.Errorf("imported = %d, want 3", result.Imported)
	}
	if result.Skipped != 0 {
		t.Errorf("skipped = %d, want 0", result.Skipped)
	}
	if result.Links != 1 || result.LinksSkipped != 0 {
		t.Errorf("links = %d skipped %d, want 1/0", result.Links, result.LinksSkipped)
	}

	// Verify alpha facts in destination.
	dstStoreA, err := memstore.NewSQLiteStore(dstDB, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	alphaAll, err := dstStoreA.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(alphaAll) != 2 {
		t.Fatalf("alpha facts = %d, want 2", len(alphaAll))
	}

	// The link arrived on the remapped ids, with its attributes and its
	// original created_at (raw import preserves timestamps).
	var newOld, newNew int64
	for _, f := range alphaAll {
		switch f.Content {
		case "Alpha old fact":
			newOld = f.ID
		case "Alpha new fact":
			newNew = f.ID
		}
	}
	links, err := dstStoreA.GetLinks(ctx, newNew, memstore.LinkOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links from the new fact = %d, want 1", len(links))
	}
	l := links[0]
	if l.SourceID != newNew || l.TargetID != newOld || l.LinkType != "related" || !l.Bidirectional || l.Label != "seeded" {
		t.Errorf("imported link = %+v, want %d->%d related bidirectional 'seeded'", l, newNew, newOld)
	}
	var lm map[string]any
	if err := json.Unmarshal(l.Metadata, &lm); err != nil || lm["w"] != 1.0 {
		t.Errorf("link metadata = %s (err %v), want w=1", l.Metadata, err)
	}
	if !l.CreatedAt.Equal(srcLink.CreatedAt) {
		t.Errorf("link created_at = %v, want %v preserved", l.CreatedAt, srcLink.CreatedAt)
	}

	// Supersession chain preserved.
	alphaActive, err := dstStoreA.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(alphaActive) != 1 {
		t.Fatalf("alpha active = %d, want 1", len(alphaActive))
	}
	if alphaActive[0].Content != "Alpha new fact" {
		t.Errorf("active content = %q, want %q", alphaActive[0].Content, "Alpha new fact")
	}

	// Metadata preserved on superseded fact.
	var oldFact *memstore.Fact
	for i := range alphaAll {
		if alphaAll[i].Content == "Alpha old fact" {
			oldFact = &alphaAll[i]
		}
	}
	if oldFact == nil {
		t.Fatal("old fact not found")
		return // SA5011: newer staticcheck misses that Fatal terminates
	}
	if oldFact.Metadata == nil {
		t.Fatal("metadata not preserved")
	}
	var m map[string]any
	if err := json.Unmarshal(oldFact.Metadata, &m); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if m["source"] != "test" {
		t.Errorf("metadata source = %v", m["source"])
	}

	// CreatedAt preserved.
	if !oldFact.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", oldFact.CreatedAt, created)
	}

	// Category preserved.
	if oldFact.Category != "preference" {
		t.Errorf("category = %q, want %q", oldFact.Category, "preference")
	}

	// Kind and subsystem preserved.
	if oldFact.Kind != "convention" {
		t.Errorf("kind = %q, want %q", oldFact.Kind, "convention")
	}
	if oldFact.Subsystem != "auth" {
		t.Errorf("subsystem = %q, want %q", oldFact.Subsystem, "auth")
	}

	// Namespace preserved.
	if oldFact.Namespace != "alpha" {
		t.Errorf("namespace = %q, want %q", oldFact.Namespace, "alpha")
	}

	// Beta facts in destination.
	dstStoreB, err := memstore.NewSQLiteStore(dstDB, nil, "beta")
	if err != nil {
		t.Fatal(err)
	}
	betaFacts, err := dstStoreB.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(betaFacts) != 1 {
		t.Fatalf("beta facts = %d, want 1", len(betaFacts))
	}
	if betaFacts[0].Content != "Beta fact" {
		t.Errorf("beta content = %q", betaFacts[0].Content)
	}
	if betaFacts[0].Category != "identity" {
		t.Errorf("beta category = %q", betaFacts[0].Category)
	}
}

func TestImportSkipDuplicates(t *testing.T) {
	srcDB := openTestDB(t)
	store, err := memstore.NewSQLiteStore(srcDB, nil, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store.Insert(ctx, memstore.Fact{Content: "Duplicate fact", Subject: "X", Category: "test"})
	store.Insert(ctx, memstore.Fact{Content: "Unique fact", Subject: "Y", Category: "test"})

	data, err := memstore.Export(ctx, srcDB)
	if err != nil {
		t.Fatal(err)
	}

	// Import twice with SkipDuplicates.
	dstDB := openTestDB(t)
	if _, err := memstore.NewSQLiteStore(dstDB, nil, ""); err != nil {
		t.Fatal(err)
	}

	r1, err := memstore.Import(ctx, dstDB, data, memstore.ImportOpts{SkipDuplicates: true})
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if r1.Imported != 2 {
		t.Errorf("first import: imported = %d, want 2", r1.Imported)
	}

	r2, err := memstore.Import(ctx, dstDB, data, memstore.ImportOpts{SkipDuplicates: true})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if r2.Imported != 0 {
		t.Errorf("second import: imported = %d, want 0", r2.Imported)
	}
	if r2.Skipped != 2 {
		t.Errorf("second import: skipped = %d, want 2", r2.Skipped)
	}

	// Verify no duplicates in database.
	dstStore, err := memstore.NewSQLiteStore(dstDB, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := dstStore.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Errorf("total facts = %d, want 2", len(facts))
	}
}

func TestImportVersionCheck(t *testing.T) {
	db := openTestDB(t)
	if _, err := memstore.NewSQLiteStore(db, nil, ""); err != nil {
		t.Fatal(err)
	}

	data := &memstore.ExportData{Version: 99}
	_, err := memstore.Import(context.Background(), db, data, memstore.ImportOpts{})
	if err == nil {
		t.Error("expected error for unsupported version")
	}
}

func TestExportIncludesSuperseded(t *testing.T) {
	db := openTestDB(t)
	store, err := memstore.NewSQLiteStore(db, nil, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	id1, _ := store.Insert(ctx, memstore.Fact{Content: "old", Subject: "X", Category: "test"})
	id2, _ := store.Insert(ctx, memstore.Fact{Content: "new", Subject: "X", Category: "test"})
	store.Supersede(ctx, id1, id2)

	data, err := memstore.Export(ctx, db)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if len(data.Facts) != 2 {
		t.Fatalf("exported %d facts, want 2 (including superseded)", len(data.Facts))
	}

	// Verify the superseded fact includes its supersession info.
	var superseded *memstore.ExportedFact
	for i := range data.Facts {
		if data.Facts[i].Content == "old" {
			superseded = &data.Facts[i]
		}
	}
	if superseded == nil {
		t.Fatal("superseded fact not found in export")
		return // SA5011: newer staticcheck misses that Fatal terminates
	}
	if superseded.SupersededBy == nil {
		t.Error("superseded fact should have SupersededBy set")
	} else if *superseded.SupersededBy != id2 {
		t.Errorf("SupersededBy = %d, want %d", *superseded.SupersededBy, id2)
	}
	if superseded.SupersededAt == nil {
		t.Error("superseded fact should have SupersededAt set")
	}
}

// TestExportRoundTrip_UserPreserved verifies that the User field in ExportedFact
// is populated on export and that a re-imported fact has a non-zero UserID.
func TestExportRoundTrip_UserPreserved(t *testing.T) {
	srcDB := openTestDB(t)
	store, err := memstore.NewSQLiteStore(srcDB, nil, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	id, err := store.Insert(ctx, memstore.Fact{
		Content: "user ownership fact", Subject: "test-subject", Category: "project",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the inserted fact has a non-zero UserID.
	inserted, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inserted.UserID == 0 {
		t.Fatal("inserted fact has UserID=0; store should set it from resolved identity")
	}

	// Export: the ExportedFact.User field should be the user's name.
	data, err := memstore.Export(ctx, srcDB)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(data.Facts) != 1 {
		t.Fatalf("exported %d facts, want 1", len(data.Facts))
	}
	if data.Facts[0].User == "" {
		t.Error("ExportedFact.User is empty; expected the owning user name")
	}

	// Import into a fresh DB and verify UserID is populated.
	dstDB := openTestDB(t)
	if _, err := memstore.NewSQLiteStore(dstDB, nil, ""); err != nil {
		t.Fatal(err)
	}
	result, err := memstore.Import(ctx, dstDB, data, memstore.ImportOpts{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}

	dstStore, err := memstore.NewSQLiteStore(dstDB, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := dstStore.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("dst facts = %d, want 1", len(facts))
	}
	if facts[0].UserID == 0 {
		t.Error("imported fact has UserID=0; should have been assigned from default user")
	}
}

// TestImport_LegacyExportNoUser verifies that a legacy export record with an
// empty User field still imports successfully and gets the destination store's
// default user assigned as UserID.
func TestImport_LegacyExportNoUser(t *testing.T) {
	// Build a synthetic export with no User field (simulates a pre-V12 export).
	data := &memstore.ExportData{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Facts: []memstore.ExportedFact{
			{
				ID:        1,
				Namespace: "test",
				User:      "", // empty = legacy, no user recorded
				Content:   "legacy fact",
				Subject:   "legacy-subject",
				Category:  "project",
				CreatedAt: time.Now().UTC(),
			},
		},
	}

	ctx := context.Background()
	dstDB := openTestDB(t)
	if _, err := memstore.NewSQLiteStore(dstDB, nil, ""); err != nil {
		t.Fatal(err)
	}

	result, err := memstore.Import(ctx, dstDB, data, memstore.ImportOpts{})
	if err != nil {
		t.Fatalf("Import legacy export: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("imported = %d, want 1", result.Imported)
	}

	dstStore, err := memstore.NewSQLiteStore(dstDB, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := dstStore.List(ctx, memstore.QueryOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("dst facts = %d, want 1", len(facts))
	}
	// With a legacy export (User == ""), the importer falls back to the store's
	// default user, so UserID must be non-zero.
	if facts[0].UserID == 0 {
		t.Error("legacy-imported fact has UserID=0; should have been assigned from default user")
	}
}

// TestStoreImport_LinksRemappedAndDanglingSkipped: through the Store
// interface (the daemon path) links are recreated on the new ids; a link
// whose endpoint was skipped as a duplicate is counted, not failed.
func TestStoreImport_LinksRemappedAndDanglingSkipped(t *testing.T) {
	ctx := context.Background()
	srcDB := openTestDB(t)
	src, err := memstore.NewSQLiteStore(srcDB, nil, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := src.Insert(ctx, memstore.Fact{Content: "fact a", Subject: "s", Category: "note"})
	b, _ := src.Insert(ctx, memstore.Fact{Content: "fact b", Subject: "s", Category: "note"})
	c, _ := src.Insert(ctx, memstore.Fact{Content: "fact c already there", Subject: "s", Category: "note"})
	if _, err := src.LinkFacts(ctx, a, b, "related", false, "kept", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := src.LinkFacts(ctx, b, c, "related", false, "dangling", nil); err != nil {
		t.Fatal(err)
	}
	data, err := memstore.Export(ctx, srcDB)
	if err != nil {
		t.Fatal(err)
	}

	dstDB := openTestDB(t)
	dst, err := memstore.NewSQLiteStore(dstDB, nil, "daemon")
	if err != nil {
		t.Fatal(err)
	}
	// c is a duplicate in the destination and will be skipped, so the b->c
	// link has no target to land on.
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
