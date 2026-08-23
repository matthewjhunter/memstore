package mcpserver_test

import (
	"context"
	"slices"
	"testing"

	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// Tools that mutate the store and so must not exist in read-only mode.
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

// registeredTools connects a session against a server built with cfg and
// returns the tool names it advertises. It goes through a real MCP session
// rather than inspecting registration state directly, because what matters is
// what a client can actually see and call.
func registeredTools(t *testing.T, cfg mcpserver.Config) []string {
	t.Helper()
	srv, _, _ := newTestServerWithConfig(t, cfg)

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "memstore-test", Version: "0.0.0"}, nil)
	srv.Register(mcpSrv)

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	serverSession, err := mcpSrv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestReadOnlyRegistersOnlyReadTools(t *testing.T) {
	got := registeredTools(t, mcpserver.Config{ReadOnly: true})

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
	got := registeredTools(t, mcpserver.Config{})

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
	got := registeredTools(t, mcpserver.Config{})
	known := slices.Concat(readTools, writeTools)
	for _, name := range got {
		if !slices.Contains(known, name) {
			t.Errorf("tool %s is registered but classified neither read nor write; add it to readTools or writeTools in this file", name)
		}
	}
}
