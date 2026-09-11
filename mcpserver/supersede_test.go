package mcpserver_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/mcpserver"
)

// Storing a fact that replaces another: the response names the relation the
// way it is stored -- the new fact supersedes the old one -- and search has to
// agree, returning the new fact and not the old. A test of the response alone
// passes whether or not the relation is written the right way round (#214).
func TestHandleStore_SupersedesDirection(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	oldID := insertFact(t, store, emb, "The deploy host runs Debian 12", "homelab", "note")
	_, out, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content:    "The deploy host runs Debian 13",
		Subject:    "homelab",
		Category:   "note",
		Supersedes: &oldID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Supersedes == nil || *out.Supersedes != oldID {
		t.Fatalf("Supersedes = %v, want the old fact %d", out.Supersedes, oldID)
	}
	if out.ID == oldID {
		t.Fatalf("ID = %d, the old fact's id; want the new fact's", out.ID)
	}
	assertSupersedesKey(t, out)

	result, _, err := srv.HandleSearch(ctx, nil, mcpserver.SearchInput{Query: "deploy host Debian"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Debian 13") {
		t.Errorf("search does not return the new fact:\n%s", text)
	}
	if strings.Contains(text, "Debian 12") {
		t.Errorf("search still returns the superseded fact:\n%s", text)
	}
}

func TestHandleStoreBatch_SupersedesKey(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()

	oldID := insertFact(t, store, emb, "The backup window is 02:00", "homelab", "note")
	_, out, err := srv.HandleStoreBatch(ctx, nil, mcpserver.StoreBatchInput{
		Facts: []mcpserver.StoreInput{{
			Content:    "The backup window is 03:00",
			Subject:    "homelab",
			Category:   "note",
			Supersedes: &oldID,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("%d results, want 1", len(out.Results))
	}
	if got := out.Results[0].Supersedes; got == nil || *got != oldID {
		t.Fatalf("Supersedes = %v, want the old fact %d", got, oldID)
	}
	assertSupersedesKey(t, out.Results[0])
}

// assertSupersedesKey checks the JSON a client sees: the old fact's id under
// "supersedes", and no "superseded_by", which read as the reverse relation.
func assertSupersedesKey(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"supersedes":`) || strings.Contains(string(raw), "superseded_by") {
		t.Errorf("response JSON %s: want a supersedes key and no superseded_by", raw)
	}
}
