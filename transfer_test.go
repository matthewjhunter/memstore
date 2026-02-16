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
