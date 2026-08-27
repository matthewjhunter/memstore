package mcpserver_test

import (
	"context"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The advertised tool list varies by token: a read-scoped caller is served a
// different set than a write-capable one. Marked "public" -- which is what the
// SDK defaults every cacheable result to -- a shared intermediary would be
// entitled to cache one caller's list and serve it to another.
//
// The ttlMs the SDK sends alongside it is 0, so a compliant client treats the
// response as immediately stale and the practical exposure is small. That is a
// mitigation, not the control: the field that says who may serve the response
// is this one, and it has to say "private".
func TestToolListIsPrivatelyCached(t *testing.T) {
	srv, store, emb := newTestServer(t)

	res, err := connect(t, srv).ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheScope != "private" {
		t.Errorf("read server tools/list cacheScope = %q, want %q", res.CacheScope, "private")
	}

	// The write server registers through the same funnel, so it must inherit
	// the policy rather than restate it.
	w, err := store.WritableFor(memstore.Principal{UserID: 1, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	wres, err := connect(t, mcpserver.NewWriteServer(w, emb)).ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if wres.CacheScope != "private" {
		t.Errorf("write server tools/list cacheScope = %q, want %q", wres.CacheScope, "private")
	}
}

// The policy is uniform, so the other cacheable results carry it too. memstore
// registers no prompts or resources today; these still answer, and the day one
// is registered it is already marked correctly rather than needing to be
// remembered.
func TestEveryCacheableResultIsPrivate(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cs := connect(t, srv)
	ctx := context.Background()

	prompts, err := cs.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if prompts.CacheScope != "private" {
		t.Errorf("prompts/list cacheScope = %q", prompts.CacheScope)
	}

	resources, err := cs.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if resources.CacheScope != "private" {
		t.Errorf("resources/list cacheScope = %q", resources.CacheScope)
	}

	templates, err := cs.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if templates.CacheScope != "private" {
		t.Errorf("resources/templates/list cacheScope = %q", templates.CacheScope)
	}
}
