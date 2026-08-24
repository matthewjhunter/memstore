package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearer attaches a token to every request the MCP client transport makes.
// The transport takes an *http.Client, not headers, so authentication rides in
// on a RoundTripper the way any other client credential would.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	next := b.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// mcpServe starts the daemon handler, mounted the way memstored mounts it, and
// returns the endpoint an MCP client should address. Going through Mount is
// deliberate: the endpoint under test is /memstore/mcp, and a test that skipped
// the mount would not notice the prefix breaking.
func mcpServe(t *testing.T, opts ...httpapi.HandlerOpt) string {
	t.Helper()
	h := newTestHandlerWith(t, opts...)
	srv := httptest.NewServer(httpapi.Mount(httpapi.DefaultPrefix, h))
	t.Cleanup(srv.Close)
	return srv.URL + httpapi.DefaultPrefix + "/mcp"
}

// mcpConnect opens a real MCP session over streamable HTTP against endpoint.
func mcpConnect(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearer{token: token}},
		// A stateless server answers GET with 405; the standalone stream is
		// optional and there is nothing server-initiated to receive.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect to %s: %v", endpoint, err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func mcpToolNames(t *testing.T, cs *mcp.ClientSession) []string {
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

func mcpText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// The endpoint serves MCP over HTTP at all: a client connects, lists tools, and
// gets the retrieval surface. This is the whole point of the phase.
func TestMCPEndpoint_ServesToolsOverHTTP(t *testing.T) {
	cs := mcpConnect(t, mcpServe(t), "")

	names := mcpToolNames(t, cs)
	if !slices.Contains(names, "memory_search") {
		t.Fatalf("memory_search missing from the served tool set: %v", names)
	}
}

// A read-scoped token gets a server built from a readable handle, so the write
// tools are not there to call. Same guarantee the capability split already
// proves in-process, now reached over the wire.
func TestMCPEndpoint_ReadTokenGetsNoWriteTools(t *testing.T) {
	endpoint := mcpServe(t, httpapi.WithTokenVerifier(scopeVerifier{
		"tok-read":  {httpapi.ScopeRead},
		"tok-admin": {httpapi.ScopeAdmin},
	}))

	readNames := mcpToolNames(t, mcpConnect(t, endpoint, "tok-read"))
	if slices.Contains(readNames, "memory_store") {
		t.Error("a read-scoped token was served memory_store")
	}
	if !slices.Contains(readNames, "memory_search") {
		t.Errorf("a read-scoped token lost the retrieval tools: %v", readNames)
	}

	adminNames := mcpToolNames(t, mcpConnect(t, endpoint, "tok-admin"))
	if !slices.Contains(adminNames, "memory_store") {
		t.Errorf("a write-capable token was not served memory_store: %v", adminNames)
	}
}

// Not merely unadvertised: calling a write tool by name over a read-scoped
// session fails, and stores nothing.
func TestMCPEndpoint_WriteToolIsUnreachableOnAReadToken(t *testing.T) {
	endpoint := mcpServe(t, httpapi.WithTokenVerifier(scopeVerifier{
		"tok-read":  {httpapi.ScopeRead},
		"tok-admin": {httpapi.ScopeAdmin},
	}))
	ctx := context.Background()

	res, err := mcpConnect(t, endpoint, "tok-read").CallTool(ctx, &mcp.CallToolParams{
		Name:      "memory_store",
		Arguments: map[string]any{"content": "smuggled over http", "subject": "mcp", "category": "note"},
	})
	if err == nil && !res.IsError {
		t.Fatal("memory_store succeeded over a read-scoped MCP session")
	}

	// The fact must not exist, which an admin session is entitled to check.
	admin := mcpConnect(t, endpoint, "tok-admin")
	found, err := admin.CallTool(ctx, &mcp.CallToolParams{
		Name:      "memory_search",
		Arguments: map[string]any{"query": "smuggled over http"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mcpText(found), "smuggled over http") {
		t.Error("the refused write reached the store anyway")
	}
}

// A write-capable session stores a fact and finds it again -- the split did not
// cost a legitimate caller anything over HTTP either.
func TestMCPEndpoint_WriteTokenCanStoreAndRetrieve(t *testing.T) {
	endpoint := mcpServe(t, httpapi.WithTokenVerifier(scopeVerifier{
		"tok-admin": {httpapi.ScopeAdmin},
	}))
	ctx := context.Background()
	cs := mcpConnect(t, endpoint, "tok-admin")

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "memory_store",
		Arguments: map[string]any{"content": "kept across the wire", "subject": "mcp", "category": "note"},
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if res.IsError {
		t.Fatalf("store failed: %s", mcpText(res))
	}

	// A second session, a second HTTP request: nothing about the write depended
	// on session state the stateless transport does not keep.
	found, err := mcpConnect(t, endpoint, "tok-admin").CallTool(ctx, &mcp.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{"subject": "mcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(found), "kept across the wire") {
		t.Errorf("stored fact not visible to a later session: %s", mcpText(found))
	}
}

// The endpoint is behind the same auth as every other route: no credential, no
// session. Checked at the HTTP layer because a failed connect says little about
// why it failed.
func TestMCPEndpoint_RequiresACredential(t *testing.T) {
	endpoint := mcpServe(t, httpapi.WithTokenVerifier(scopeVerifier{
		"tok-read": {httpapi.ScopeRead},
	}))

	req, err := http.NewRequest("POST", endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated MCP request: got %d, want 401", resp.StatusCode)
	}
}

// Every server shape serves the read tools, because WritableStore embeds
// ReadableStore -- so admitting a write-only token would hand it reads the REST
// surface denies it. The endpoint refuses instead of quietly widening the token.
func TestMCPEndpoint_RefusesATokenWithoutRead(t *testing.T) {
	endpoint := mcpServe(t, httpapi.WithTokenVerifier(scopeVerifier{
		"tok-write":  {httpapi.ScopeWrite},
		"tok-ingest": {httpapi.ScopeIngest},
	}))

	for _, token := range []string{"tok-write", "tok-ingest"} {
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s reached the MCP endpoint: got %d, want 403", token, resp.StatusCode)
		}
	}
}

// Stateless means stateless: the transport must not mint a session id, because
// a client that pinned one would be pinning nothing, and the next request could
// land on a different process with no memory of it.
func TestMCPEndpoint_IsStateless(t *testing.T) {
	req, err := http.NewRequest("POST", mcpServe(t), strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Mcp-Session-Id"); got != "" {
		t.Errorf("stateless endpoint issued a session id: %q", got)
	}
}

// memory_rate_context records feedback, and its REST equivalent
// (POST /v1/context/feedback) requires the write scope. It must not be reachable
// through MCP by a token that could not reach it over HTTP.
//
// It holds by placement -- the tool is registered in WriteServer.Register -- so
// this test is here to keep it that way. The daemon hands the session store to
// both server shapes, and the day someone moves the registration up to
// MemoryServer to "make feedback available to readers", this fails.
func TestMCPEndpoint_FeedbackToolFollowsTheWriteScope(t *testing.T) {
	endpoint := mcpServe(t,
		httpapi.WithSessionStore(&mockSessionStore{}),
		httpapi.WithTokenVerifier(scopeVerifier{
			"tok-read":  {httpapi.ScopeRead},
			"tok-admin": {httpapi.ScopeAdmin},
		}))

	if names := mcpToolNames(t, mcpConnect(t, endpoint, "tok-read")); slices.Contains(names, "memory_rate_context") {
		t.Error("a read-scoped token was served memory_rate_context")
	}
	if names := mcpToolNames(t, mcpConnect(t, endpoint, "tok-admin")); !slices.Contains(names, "memory_rate_context") {
		t.Errorf("a write-capable token lost memory_rate_context: %v", names)
	}
}
