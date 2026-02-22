package memstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

// insertTestFact inserts a minimal fact (no embedding) for link tests.
func insertTestFact(t *testing.T, store *memstore.SQLiteStore, content, subject string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := store.Insert(ctx, memstore.Fact{
		Content:  content,
		Subject:  subject,
		Category: "note",
	})
	if err != nil {
		t.Fatalf("insert fact %q: %v", content, err)
	}
	return id
}

func TestLinkFacts_Basic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	id, err := store.LinkFacts(ctx, a, b, "passage", false, "north door", map[string]any{"hidden": false})
	if err != nil {
		t.Fatalf("LinkFacts: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive link ID, got %d", id)
	}

	l, err := store.GetLink(ctx, id)
	if err != nil {
		t.Fatalf("GetLink: %v", err)
	}
	if l.SourceID != a {
		t.Errorf("SourceID: want %d, got %d", a, l.SourceID)
	}
	if l.TargetID != b {
		t.Errorf("TargetID: want %d, got %d", b, l.TargetID)
	}
	if l.LinkType != "passage" {
		t.Errorf("LinkType: want %q, got %q", "passage", l.LinkType)
	}
	if l.Bidirectional {
		t.Error("expected Bidirectional=false")
	}
	if l.Label != "north door" {
		t.Errorf("Label: want %q, got %q", "north door", l.Label)
	}
	if l.Metadata == nil {
		t.Error("expected non-nil Metadata")
	}
	if l.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
}

func TestLinkFacts_Bidirectional(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	if _, err := store.LinkFacts(ctx, a, b, "passage", true, "corridor", nil); err != nil {
		t.Fatalf("LinkFacts: %v", err)
	}

	// Outbound from A — should see the edge (A is source).
	links, err := store.GetLinks(ctx, a, memstore.LinkOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 outbound from A, got %d", len(links))
	}

	// Outbound from B — bidirectional, so B can also reach A via this edge.
	links, err = store.GetLinks(ctx, b, memstore.LinkOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 outbound from B (bidirectional), got %d", len(links))
	}

	// Inbound to A — bidirectional means A is reachable from B.
	links, err = store.GetLinks(ctx, a, memstore.LinkInbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 inbound to A (bidirectional), got %d", len(links))
	}
}

func TestGetLinks_DirectedOnlyDirectionality(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")
	c := insertTestFact(t, store, "Room C", "room-c")

	// A -> B and C -> B, both directed.
	if _, err := store.LinkFacts(ctx, a, b, "passage", false, "to B", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LinkFacts(ctx, c, b, "passage", false, "to B from C", nil); err != nil {
		t.Fatal(err)
	}

	// Outbound from B: none (B is only a target).
	out, err := store.GetLinks(ctx, b, memstore.LinkOutbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("expected 0 outbound from B, got %d", len(out))
	}

	// Inbound to B: 2.
	in, err := store.GetLinks(ctx, b, memstore.LinkInbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 2 {
		t.Errorf("expected 2 inbound to B, got %d", len(in))
	}

	// Both for B: 2.
	both, err := store.GetLinks(ctx, b, memstore.LinkBoth)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Errorf("expected 2 total links for B, got %d", len(both))
	}
}

func TestGetLinks_TypeFilter(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	loc := insertTestFact(t, store, "Entry Hall", "hall")
	enc := insertTestFact(t, store, "Guard Encounter", "encounter")
	dest := insertTestFact(t, store, "Throne Room", "throne")

	if _, err := store.LinkFacts(ctx, loc, enc, "event", false, "random encounter", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LinkFacts(ctx, loc, dest, "passage", false, "east corridor", nil); err != nil {
		t.Fatal(err)
	}

	passages, err := store.GetLinks(ctx, loc, memstore.LinkOutbound, "passage")
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) != 1 || passages[0].LinkType != "passage" {
		t.Errorf("expected 1 passage link, got %d", len(passages))
	}

	events, err := store.GetLinks(ctx, loc, memstore.LinkOutbound, "event")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].LinkType != "event" {
		t.Errorf("expected 1 event link, got %d", len(events))
	}

	all, err := store.GetLinks(ctx, loc, memstore.LinkOutbound, "passage", "event")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 links with multi-type filter, got %d", len(all))
	}
}

func TestUpdateLink_LabelAndMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	id, err := store.LinkFacts(ctx, a, b, "passage", false, "old label", map[string]any{"hidden": true, "keep": "yes"})
	if err != nil {
		t.Fatal(err)
	}

	// Update label, add "dc", delete "hidden" via nil.
	if err := store.UpdateLink(ctx, id, "new label", map[string]any{"dc": 15, "hidden": nil}); err != nil {
		t.Fatalf("UpdateLink: %v", err)
	}

	l, err := store.GetLink(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Label != "new label" {
		t.Errorf("Label: want %q, got %q", "new label", l.Label)
	}

	var meta map[string]any
	if err := json.Unmarshal(l.Metadata, &meta); err != nil {
		t.Fatalf("parsing metadata: %v", err)
	}
	if _, ok := meta["hidden"]; ok {
		t.Error("expected 'hidden' to be deleted by nil patch")
	}
	if v, ok := meta["dc"]; !ok || v.(float64) != 15 {
		t.Errorf("expected dc=15, got %v", meta["dc"])
	}
	if v, ok := meta["keep"]; !ok || v != "yes" {
		t.Errorf("expected keep=yes to survive, got %v", meta["keep"])
	}
}

func TestUpdateLink_EmptyLabelPreservesExisting(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	id, err := store.LinkFacts(ctx, a, b, "passage", false, "keep this", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateLink(ctx, id, "", map[string]any{"new_key": "value"}); err != nil {
		t.Fatalf("UpdateLink: %v", err)
	}

	l, err := store.GetLink(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Label != "keep this" {
		t.Errorf("label should be preserved when empty string passed, got %q", l.Label)
	}
}

func TestDeleteLink(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	id, err := store.LinkFacts(ctx, a, b, "passage", false, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteLink(ctx, id); err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}

	if _, err := store.GetLink(ctx, id); err == nil {
		t.Error("expected error getting deleted link")
	}

	if err := store.DeleteLink(ctx, id); err == nil {
		t.Error("expected error on double-delete")
	}
}

func TestDeleteFact_CascadesLinks(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	linkID, err := store.LinkFacts(ctx, a, b, "passage", false, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Deleting the source fact must cascade-delete the link.
	if err := store.Delete(ctx, a); err != nil {
		t.Fatalf("Delete fact: %v", err)
	}

	if _, err := store.GetLink(ctx, linkID); err == nil {
		t.Error("expected link to be cascade-deleted when source fact was deleted")
	}
}

func TestDeleteFact_CascadesLinks_TargetSide(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	linkID, err := store.LinkFacts(ctx, a, b, "passage", false, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Deleting the target fact must also cascade-delete the link.
	if err := store.Delete(ctx, b); err != nil {
		t.Fatalf("Delete fact: %v", err)
	}

	if _, err := store.GetLink(ctx, linkID); err == nil {
		t.Error("expected link to be cascade-deleted when target fact was deleted")
	}
}

func TestGetLink_NotFound(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if _, err := store.GetLink(ctx, 9999); err == nil {
		t.Error("expected error for non-existent link ID")
	}
}

func TestUpdateLink_NotFound(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.UpdateLink(ctx, 9999, "x", nil); err == nil {
		t.Error("expected error updating non-existent link")
	}
}

func TestLinks_NamespaceIsolation(t *testing.T) {
	// Two stores share the same underlying DB but use different namespaces.
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	emb := &mockEmbedder{dim: 4}
	s1, err := memstore.NewSQLiteStore(db, emb, "ns1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := memstore.NewSQLiteStore(db, emb, "ns2")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	a := insertTestFact(t, s1, "Room A ns1", "room-a")
	b := insertTestFact(t, s1, "Room B ns1", "room-b")

	if _, err := s1.LinkFacts(ctx, a, b, "passage", false, "", nil); err != nil {
		t.Fatal(err)
	}

	// ns2 should not see ns1's links.
	links, err := s2.GetLinks(ctx, a, memstore.LinkBoth)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Errorf("expected 0 links in ns2, got %d", len(links))
	}
}

func TestLinkFacts_NoMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	a := insertTestFact(t, store, "Room A", "room-a")
	b := insertTestFact(t, store, "Room B", "room-b")

	id, err := store.LinkFacts(ctx, a, b, "reference", false, "", nil)
	if err != nil {
		t.Fatalf("LinkFacts with nil metadata: %v", err)
	}

	l, err := store.GetLink(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Metadata != nil {
		t.Errorf("expected nil Metadata, got %s", l.Metadata)
	}
	if !strings.EqualFold(l.LinkType, "reference") {
		t.Errorf("LinkType: want %q, got %q", "reference", l.LinkType)
	}
}
