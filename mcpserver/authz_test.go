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

// newClientSession connects a client to a server built with cfg over in-memory
// transports. Tests go through a real MCP session rather than calling handler
// methods directly, because the guard lives in registration: a test that
// invoked ms.HandleStore would bypass the very thing it is checking.
func newClientSession(t *testing.T, cfg mcpserver.Config) (*mcp.ClientSession, *memstore.SQLiteStore) {
	t.Helper()
	srv, store, _ := newTestServerWithConfig(t, cfg)

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
	return cs, store
}

// validArgs synthesises an argument set satisfying a tool's declared required
// properties. The SDK validates arguments against the input schema before the
// handler runs, so a call with empty arguments never reaches the guard -- and a
// test that read the schema error as a refusal would pass whether or not the
// guard existed. Deriving the arguments from the schema also means a write tool
// added later is exercised without anyone hand-writing a fixture for it.
func validArgs(schema any) map[string]any {
	obj, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	args := map[string]any{}
	req, _ := obj["required"].([]any)
	props, _ := obj["properties"].(map[string]any)
	for _, r := range req {
		name, _ := r.(string)
		var propSchema any
		if props != nil {
			propSchema = props[name]
		}
		args[name] = sampleFor(propSchema)
	}
	return args
}

// sampleFor produces the least interesting value a schema will accept. Ints are
// 1 rather than 0 because several handlers reject a non-positive id as invalid
// input, which would mask the denial this is trying to observe.
func sampleFor(schema any) any {
	obj, _ := schema.(map[string]any)
	switch typ := schemaType(obj); typ {
	case "integer", "number":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{sampleFor(obj["items"])}
	case "object":
		return validArgs(obj)
	default:
		return "x"
	}
}

// schemaType resolves the declared type. A nullable property arrives as a
// union ("null" plus the real type) rather than a bare string, and an optional
// one may declare no type at all -- in which case the presence of "items" or
// "properties" says what it is. Reading only the bare string form produced a
// string for memstore_store_batch's facts array, which the SDK then rejected
// before the guard was ever reached.
func schemaType(obj map[string]any) string {
	switch t := obj["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, _ := v.(string); s != "" && s != "null" {
				return s
			}
		}
	}
	if _, ok := obj["items"]; ok {
		return "array"
	}
	if _, ok := obj["properties"]; ok {
		return "object"
	}
	return ""
}

// writeToolArgs lists the advertised write tools with arguments that will pass
// schema validation, so each call reaches the guard.
func writeToolArgs(t *testing.T, cs *mcp.ClientSession) map[string]map[string]any {
	t.Helper()
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := map[string]map[string]any{}
	for _, tool := range res.Tools {
		if slices.Contains(writeTools, tool.Name) {
			out[tool.Name] = validArgs(tool.InputSchema)
		}
	}
	return out
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func denyAll(reason string) mcpserver.WriteAuthorizer {
	return func(context.Context) error { return errors.New(reason) }
}

// Every write tool must refuse when the authorizer denies. This is the
// guarantee that stops existing in-process: today the MCP server's store IS the
// httpclient, so a write lands on a REST route guarded by requireScope. Served
// from inside memstored the store is the pgstore directly, the route leaves the
// path, and nothing checks the caller's scopes unless this does.
func TestWriteToolsRefuseWhenTheAuthorizerDenies(t *testing.T) {
	cs, _ := newClientSession(t, mcpserver.Config{Authorize: denyAll("token lacks the write scope")})

	args := writeToolArgs(t, cs)
	if len(args) != len(writeTools) {
		t.Fatalf("advertised %d write tools, expected %d: %v", len(args), len(writeTools), args)
	}

	for name, a := range args {
		t.Run(name, func(t *testing.T) {
			text, isError := callText(t, cs, name, a)

			if !strings.Contains(text, "token lacks the write scope") {
				t.Errorf("%s did not report the denial reason, got: %s", name, text)
			}
			// A denial is a fact about the caller, not a memstore failure. Same
			// rule as an invalid argument: IsError would tell a client the
			// result is untrustworthy and cost it the structured output.
			if isError {
				t.Errorf("%s set IsError on an authorization denial: %s", name, text)
			}
		})
	}
}

// The authorizer governs writes only. A read tool must be unaffected, or a
// read-scoped token would lose the retrieval it is entitled to.
func TestReadToolsIgnoreTheWriteAuthorizer(t *testing.T) {
	cs, _ := newClientSession(t, mcpserver.Config{Authorize: denyAll("token lacks the write scope")})

	text, isError := callText(t, cs, "memory_status", map[string]any{})
	if isError {
		t.Errorf("memory_status failed under a deny-all write authorizer: %s", text)
	}
	if strings.Contains(text, "write scope") {
		t.Errorf("memory_status was refused by the write authorizer: %s", text)
	}
}

// The decision is made per call, from the context of that call. Registration
// happens once; the caller may not be the same one every time. When the HTTP
// server caches a server per identity, a stale or mis-keyed cache entry is the
// failure this forecloses -- the guard asks again rather than trusting what was
// true when the tool was registered.
func TestWriteAuthorizerIsConsultedPerCall(t *testing.T) {
	var calls int
	cs, _ := newClientSession(t, mcpserver.Config{
		Authorize: func(context.Context) error {
			calls++
			if calls == 1 {
				return nil
			}
			return errors.New("token lacks the write scope")
		},
	})

	args := map[string]any{"content": "a fact worth keeping", "subject": "authz", "category": "note"}

	if text, isError := callText(t, cs, "memory_store", args); isError || strings.Contains(text, "write scope") {
		t.Fatalf("first call should have been allowed, got: %s", text)
	}
	if text, _ := callText(t, cs, "memory_store", args); !strings.Contains(text, "token lacks the write scope") {
		t.Errorf("second call was not re-checked; the decision was captured at registration, got: %s", text)
	}
	if calls != 2 {
		t.Errorf("authorizer consulted %d times across two calls, want 2", calls)
	}
}

// A refused call must not have changed anything. The guard runs before the
// handler, so nothing is written, partially written, or embedded on the way to
// finding out the caller was not allowed.
//
// Note what this does NOT claim: the SDK validates arguments against the input
// schema before dispatch, so a malformed unauthorized call is answered with a
// schema error rather than a denial. Moving authorization ahead of that would
// mean a receiving middleware keyed by tool name -- the name-to-scope table
// this design rejected. The caller learns nothing from the schema error it
// could not read off tools/list, and no write occurs either way.
func TestARefusedWriteChangesNothing(t *testing.T) {
	cs, store := newClientSession(t, mcpserver.Config{Authorize: denyAll("token lacks the write scope")})
	ctx := context.Background()

	before, err := store.ActiveCount(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for name, a := range writeToolArgs(t, cs) {
		if text, _ := callText(t, cs, name, a); !strings.Contains(text, "token lacks the write scope") {
			t.Fatalf("%s was not refused: %s", name, text)
		}
	}

	after, err := store.ActiveCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("refused calls changed the store: %d facts before, %d after", before, after)
	}
}

// Without an authorizer nothing is gated: the stdio binary configures no
// authorizer today, and its writes must keep working exactly as before.
func TestWritesAreAllowedWithoutAnAuthorizer(t *testing.T) {
	cs, _ := newClientSession(t, mcpserver.Config{})

	text, isError := callText(t, cs, "memory_store", map[string]any{
		"content": "a fact worth keeping", "subject": "authz", "category": "note",
	})
	if isError {
		t.Errorf("memory_store failed with no authorizer configured: %s", text)
	}
	if strings.Contains(text, "not authorized") || strings.Contains(text, "write scope") {
		t.Errorf("memory_store was gated with no authorizer configured: %s", text)
	}
}

// A write tool registered with a bare mcp.AddTool would be advertised and
// unguarded, and nothing in the type system would say so. Walking the
// advertised set and asserting each one refuses catches that here rather than
// in production.
func TestEveryAdvertisedWriteToolIsGuarded(t *testing.T) {
	cs, _ := newClientSession(t, mcpserver.Config{Authorize: denyAll("token lacks the write scope")})

	for name, a := range writeToolArgs(t, cs) {
		if text, _ := callText(t, cs, name, a); !strings.Contains(text, "token lacks the write scope") {
			t.Errorf("write tool %s is advertised but not guarded, got: %s", name, text)
		}
	}
}

// The denial has to reach the structured channel too, and say something a
// client can act on differently from a bad argument. "Fix your arguments" and
// "you may not do this at all" call for different responses: a model told
// invalid_input will reasonably retry, and a retry of a forbidden call can
// never succeed. Same reasoning as #164 -- both channels reach a model, and the
// server is not told which one the client read.
func TestDenialIsStructurallyDistinctFromInvalidInput(t *testing.T) {
	ctx := context.Background()
	args := map[string]any{"content": "a fact worth keeping", "subject": "authz", "category": "note"}

	denied, _ := newClientSession(t, mcpserver.Config{Authorize: denyAll("token lacks the write scope")})
	res, err := denied.CallTool(ctx, &mcp.CallToolParams{Name: "memory_store", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := res.StructuredContent.(map[string]any)
	if got := out["status"]; got != "forbidden" {
		t.Errorf("denied store reported status %v, want forbidden", got)
	}
	if out["error"] == "" || out["error"] == nil {
		t.Error("denied store carried no reason on the structured channel")
	}

	// The contrast: a bad argument on a server with no authorizer still reports
	// invalid_input, so the two remain tellable apart.
	open, _ := newClientSession(t, mcpserver.Config{})
	res, err = open.CallTool(ctx, &mcp.CallToolParams{Name: "memory_store",
		Arguments: map[string]any{"content": "", "subject": "authz"}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = res.StructuredContent.(map[string]any)
	if got := out["status"]; got != "invalid_input" {
		t.Errorf("empty content reported status %v, want invalid_input", got)
	}
}
