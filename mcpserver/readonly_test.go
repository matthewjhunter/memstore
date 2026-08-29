package mcpserver_test

import (
	"slices"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
)

// Tools that never mutate the store. memory_rerank_settings is here because
// its writes are per-session retrieval knobs held in the server process, not
// facts. memory_curate_context and memory_rate_context are absent because
// neither registers under a bare Config (no curator, no session store).
var readTools = []string{
	"memory_get_context",
	"memory_get_links",
	"memory_history",
	"memory_list",
	"memory_list_subsystems",
	"memory_rerank_settings",
	"memory_search",
	"memory_status",
	"memory_suggest_agent",
	"memory_task_list",
}

// Tools that mutate the store. Their handlers are methods on WriteServer, so a
// read server cannot register them even by mistake -- these lists say what each
// server type is expected to advertise, and TestEveryToolIsClassified fails
// when a new tool joins neither.
var writeTools = []string{
	"memory_confirm",
	"memory_delete",
	"memory_link",
	"memory_store",
	"memory_store_batch",
	"memory_supersede",
	"memory_task_create",
	"memory_task_update",
	"memory_unlink",
	"memory_update",
	"memory_update_link",
}

// registeredTools returns the tool names a server advertises, via a real MCP
// session. Which server it is asked about is the point: the read/write split is
// carried by the type now, not by a flag, so the test builds the server whose
// tool set it means to check.
func registeredTools(t *testing.T, srv registrar) []string {
	t.Helper()
	return toolNames(t, connect(t, srv))
}

// readServer builds a retrieval-only server from a scoper-issued read handle,
// the way an HTTP request handler will for a caller that may not write.
func readServer(t *testing.T) *mcpserver.MemoryServer {
	t.Helper()
	_, store, _ := newTestServer(t)
	r, err := store.ReadableFor(memstore.Principal{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	return mcpserver.NewMemoryServer(r)
}

func TestReadOnlyRegistersOnlyReadTools(t *testing.T) {
	got := registeredTools(t, readServer(t))

	want := slices.Clone(readTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("read-only tool set mismatch\n got: %v\nwant: %v", got, want)
	}

	// Stated separately from the set comparison so a failure names the tool
	// that leaked rather than dumping two lists to diff by eye.
	for _, name := range writeTools {
		if slices.Contains(got, name) {
			t.Errorf("write tool %s is registered in read-only mode", name)
		}
	}
}

func TestReadWriteRegistersBothSets(t *testing.T) {
	// The guard against gating unconditionally: without ReadOnly, every tool
	// on both lists must still be there.
	srv, _, _ := newTestServer(t)
	got := registeredTools(t, srv)

	want := slices.Concat(readTools, writeTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("default tool set mismatch\n got: %v\nwant: %v", got, want)
	}
}

// A new tool must be classified as read or write, which means adding it to one
// of the lists above. This fails when someone adds a tool and does not, rather
// than letting it default into read-only mode unexamined.
func TestEveryToolIsClassified(t *testing.T) {
	srv, _, _ := newTestServer(t)
	got := registeredTools(t, srv)
	known := slices.Concat(readTools, writeTools)
	for _, name := range got {
		if !slices.Contains(known, name) {
			t.Errorf("tool %s is registered but classified neither read nor write; add it to readTools or writeTools in this file", name)
		}
	}
}
