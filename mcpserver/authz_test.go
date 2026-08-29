package mcpserver_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registrar is either server type. Both register tools; only one has write
// tools to register.
type registrar interface{ Register(*mcp.Server) }

// connect runs a real MCP session against srv over in-memory transports. Tests
// go through a session rather than inspecting registration state, because what
// matters is what a client can see and call.
func connect(t *testing.T, srv registrar) *mcp.ClientSession {
	t.Helper()
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
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
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

// A principal with no write right is refused a writable handle, so there is
// nothing to build a WriteServer from. This is the whole authorization
// decision: it happens once, when the handle is minted, and everything after
// it is types.
func TestReadPrincipalCannotObtainAWriteHandle(t *testing.T) {
	_, store, _ := newTestServer(t)

	if _, err := store.WritableFor(memstore.Principal{UserID: 1}); !errors.Is(err, memstore.ErrNotPermitted) {
		t.Errorf("WritableFor(read principal): errors.Is(err, ErrNotPermitted) = false; err = %v", err)
	}
	if _, err := store.WritableFor(memstore.Principal{UserID: 1, Write: true}); err != nil {
		t.Errorf("WritableFor(write principal): %v", err)
	}
}

// A server built from a read handle advertises the retrieval tools and none of
// the others. Not because it filtered them out -- because their handlers are
// methods on WriteServer, so this server has no way to register them.
func TestReadServerAdvertisesOnlyReadTools(t *testing.T) {
	_, store, _ := newTestServer(t)

	r, err := store.ReadableFor(memstore.Principal{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := toolNames(t, connect(t, mcpserver.NewMemoryServer(r)))

	want := slices.Clone(readTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("read server tool set mismatch\n got: %v\nwant: %v", got, want)
	}
	for _, name := range writeTools {
		if slices.Contains(got, name) {
			t.Errorf("write tool %s is registered on a read-only server", name)
		}
	}
}

// The write tools are not merely unadvertised on a read server: calling one by
// name fails and changes nothing. A client that learned the name elsewhere gets
// no further than one that did not.
func TestWriteToolsAreUnreachableOnAReadServer(t *testing.T) {
	_, store, _ := newTestServer(t)
	ctx := context.Background()

	r, err := store.ReadableFor(memstore.Principal{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mcpserver.NewMemoryServer(r))

	before, err := store.ActiveCount(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range writeTools {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      name,
			Arguments: map[string]any{"content": "smuggled", "subject": "authz", "category": "note"},
		})
		if err == nil && !res.IsError {
			t.Errorf("%s succeeded against a read-only server", name)
		}
	}

	after, err := store.ActiveCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("calls to unregistered write tools changed the store: %d facts before, %d after", before, after)
	}
}

// The other half: a write-capable server serves both sets, so the split did not
// cost a legitimate caller anything.
func TestWriteServerServesBothToolSets(t *testing.T) {
	_, store, _ := newTestServer(t)

	w, err := store.WritableFor(memstore.Principal{UserID: 1, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mcpserver.NewWriteServer(w))

	got := toolNames(t, cs)
	want := slices.Concat(readTools, writeTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("write server tool set mismatch\n got: %v\nwant: %v", got, want)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "memory_store",
		Arguments: map[string]any{"content": "a fact worth keeping", "subject": "authz", "category": "note"},
	})
	if err != nil {
		t.Fatalf("store through a write server: %v", err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		t.Errorf("store through a write server failed: %s", b.String())
	}
}

// A bad per-call knob is refused through the tool, on the terms read tools use:
// the reason reaches the caller and IsError stays down, because the arguments
// never described a request rather than memstore having failed at one.
func TestSearchRefusesABadPerCallKnob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cs := connect(t, srv)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "memory_search",
		Arguments: map[string]any{"query": "anything", "rerank_mode": "sideways"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if !strings.Contains(b.String(), "sideways") {
		t.Errorf("an unknown rerank mode was not named in the refusal: %s", b.String())
	}
	if res.IsError {
		t.Errorf("IsError set on a bad argument: %s", b.String())
	}
}
