package mcpserver_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
)

// persistent marks a fact exempt from age-based flush. It is a metadata key,
// set by hand through memory_store and memory_update, and absent otherwise.

func factMeta(t *testing.T, store memstore.Store, id int64) map[string]any {
	t.Helper()
	f, err := store.Get(context.Background(), id)
	if err != nil || f == nil {
		t.Fatalf("Get %d: %v", id, err)
	}
	m := map[string]any{}
	if len(f.Metadata) > 0 {
		if err := json.Unmarshal(f.Metadata, &m); err != nil {
			t.Fatalf("metadata of %d: %v", id, err)
		}
	}
	return m
}

func TestHandleStore_Persistent(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()

	_, out, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{
		Content:    "The house was built in 1987",
		Subject:    "house",
		Category:   "world",
		Metadata:   mcpserver.Metadata{"source": "conversation"},
		Persistent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := factMeta(t, store, out.ID)
	if m[memstore.MetaPersistent] != true {
		t.Errorf("persistent = %v, want true", m[memstore.MetaPersistent])
	}
	if m["source"] != "conversation" {
		t.Errorf("source = %v, want the caller's metadata kept", m["source"])
	}

	// Unmarked facts carry no key at all, not a false one.
	_, plain, err := srv.HandleStore(ctx, nil, mcpserver.StoreInput{Content: "The house has a blue door", Subject: "house"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := factMeta(t, store, plain.ID)[memstore.MetaPersistent]; ok {
		t.Error("an unmarked fact has a persistent key")
	}
}

func TestHandleStoreBatch_Persistent(t *testing.T) {
	srv, store, _ := newTestServer(t)
	_, out, err := srv.HandleStoreBatch(context.Background(), nil, mcpserver.StoreBatchInput{
		Facts: []mcpserver.StoreInput{
			{Content: "The well is 120 feet deep", Subject: "house", Persistent: true},
			{Content: "The gutters were cleaned in May", Subject: "house"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("%d results, want 2", len(out.Results))
	}
	if factMeta(t, store, out.Results[0].ID)[memstore.MetaPersistent] != true {
		t.Error("first fact: not marked persistent")
	}
	if _, ok := factMeta(t, store, out.Results[1].ID)[memstore.MetaPersistent]; ok {
		t.Error("second fact: marked persistent, want no key")
	}
}

// memory_update sets and clears the mark, and the mark alone is a complete
// update: no other metadata has to come with it.
func TestHandleUpdate_Persistent(t *testing.T) {
	srv, store, emb := newTestServer(t)
	ctx := context.Background()
	id := insertFact(t, store, emb, "The roof was replaced in 2019", "house", "world")

	yes, no := true, false
	result, _, err := srv.HandleUpdate(ctx, nil, mcpserver.UpdateInput{ID: id, Persistent: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("marking persistent: %s", resultText(t, result))
	}
	if factMeta(t, store, id)[memstore.MetaPersistent] != true {
		t.Error("not marked persistent after update")
	}

	result, _, err = srv.HandleUpdate(ctx, nil, mcpserver.UpdateInput{ID: id, Persistent: &no})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("clearing persistent: %s", resultText(t, result))
	}
	if _, ok := factMeta(t, store, id)[memstore.MetaPersistent]; ok {
		t.Error("persistent key still present after clearing it")
	}

	result, _, _ = srv.HandleUpdate(ctx, nil, mcpserver.UpdateInput{ID: id})
	assertRejected(t, result)
}
