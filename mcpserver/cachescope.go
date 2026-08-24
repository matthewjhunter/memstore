package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Cache scope for everything memstore serves.
//
// The 2026-07-28 revision has every list result and resource read carry a
// caching hint: ttlMs for how long, and cacheScope for who. "public" means any
// client or intermediary may cache the response and serve it on; "private"
// means only the requesting user's client may.
//
// The SDK defaults every one of them to "public", applied after the handler
// returns, so a handler cannot set it -- which is why this is middleware rather
// than a field somewhere.
//
// Everything memstore serves is identity-dependent. The tool list varies by
// token: a read-scoped caller is advertised the retrieval tools and nothing
// else, because the server built for it has no write handlers to register.
// server/discover carries the instructions, which say different things to a
// read-only session. And any resource memstore ever serves is by definition the
// caller's own memory. There is no result here that is the same for two
// callers, so the policy is uniform rather than per-method: an intermediary is
// never entitled to pass one of these on.
//
// This is not the enforcement of anything -- an intermediary that ignores the
// hint is not stopped by it, and nothing about authorization depends on it. It
// is the correct declaration, and the alternative is a wrong one.

// privateCacheScope marks every cacheable result private on its way out.
//
// It type-switches on the concrete result types rather than through an
// interface because the SDK's CacheableResult interface is read-only: it has
// GetCacheScope and no setter, and the Cacheable struct is embedded by value.
// A result type added to the protocol later will not appear here, which is what
// the test guards.
func privateCacheScope(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil {
			return res, err
		}
		switch r := res.(type) {
		case *mcp.ListToolsResult:
			r.CacheScope = cacheScopePrivate
		case *mcp.DiscoverResult:
			r.CacheScope = cacheScopePrivate
		case *mcp.ListPromptsResult:
			r.CacheScope = cacheScopePrivate
		case *mcp.ListResourcesResult:
			r.CacheScope = cacheScopePrivate
		case *mcp.ListResourceTemplatesResult:
			r.CacheScope = cacheScopePrivate
		case *mcp.ReadResourceResult:
			r.CacheScope = cacheScopePrivate
		}
		return res, nil
	}
}

const cacheScopePrivate = "private"
