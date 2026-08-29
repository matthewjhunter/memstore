package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP surface, served over the same HTTP daemon as the REST API.
//
// Two properties make this endpoint different from every other route, and both
// come from the protocol rather than from us.
//
// It is stateless (SEP-2567). No session id is issued or honoured: each request
// carries everything needed to answer it, and the server that answers it is
// built for that request and discarded. That is why the tunables became per-call
// arguments in phase 3 -- there is nowhere for a setting made by one call to
// live until the next.
//
// And it is one route serving many operations. The REST surface declares a
// scope per route, which works because the path says what the caller is about
// to do; POST /mcp says only "MCP". So the entitlement is not checked per call
// here -- it decides which server gets built, once, before the JSON-RPC body is
// even parsed.

// The one thing shared across requests, and the reason it is safe to share.
//
// Registering a tool generates its JSON Schema by reflecting over the Go input
// and output types, which is most of the cost of building a server: 2.2ms for
// the 24 write-capable tools, against 3.8ms for a whole tools/list round trip.
// Per request, that is paid again for types that never change.
//
// The SDK's SchemaCache exists for exactly this deployment -- its own
// documentation names "one Server per request" as the case it is for -- and it
// caches the right thing. A schema is a property of a Go type: identical for
// every caller and carrying no authority. The server built around it is the
// opposite, and is precisely the object that encodes what this caller may do,
// which is why the servers are not pooled and should not be. Caching those by
// (user, scopes) would put a cache on the authorization boundary, where a
// revoked token would keep being handed a live write server until something
// invalidated the entry -- reintroducing the per-request state SEP-2575 removed,
// to save the half-millisecond the schema cache does not already save.
//
// The cache is documented as growing without bound when tool input types are
// generated dynamically. Memstore's are a fixed set of Go structs, so it tops
// out at one entry per tool type for the life of the process.

// mcpServerKey carries the per-request MCP server from the authorization
// decision to the SDK's getServer callback. The callback is handed the request
// and nothing else, so the server it should return travels on the context.
type mcpServerKey struct{}

// mcpRegistrar is whichever capability-shaped server this request earned.
// *MemoryServer and *WriteServer both register their own tools; only one of
// them has write tools to register, which is the entire authorization result.
type mcpRegistrar interface{ Register(*mcp.Server) }

// errNoScoper reports a backend that cannot mint capability-typed handles.
// Every backend memstore ships does; a third-party Store implementation might
// not, and serving MCP from an unnarrowed handle would hand every caller
// everything.
var errNoScoper = errors.New("backend does not support capability scoping")

// newMCPHandler builds the SDK transport handler. It is constructed once and
// reused: the per-request part is the *mcp.Server it pulls off the context, not
// the transport around it.
func newMCPHandler() *mcp.StreamableHTTPHandler {
	return mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			srv, _ := r.Context().Value(mcpServerKey{}).(*mcp.Server)
			return srv
		},
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
}

// handleMCP resolves the caller's authority, builds the server that authority
// allows, and hands the request to the transport.
func (h *Handler) handleMCP(w http.ResponseWriter, r *http.Request) {
	srv, err := h.mcpServerFor(r)
	if err != nil {
		switch {
		case errors.Is(err, memstore.ErrNotPermitted):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, errNoScoper):
			writeError(w, http.StatusNotImplemented, "MCP is not available on this backend")
		default:
			writeError(w, http.StatusInternalServerError, "store scoping failed")
		}
		return
	}
	h.mcpHTTP.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpServerKey{}, srv)))
}

// mcpServerFor makes the one authorization decision this endpoint makes.
//
// A caller who may write is given a handle minted by WritableFor and a
// *WriteServer built from it; everyone else gets a read handle and a
// *MemoryServer, which has no write handler to call and none to register.
//
// Read is required either way. WritableStore embeds ReadableStore -- writes need
// reads -- so every server shape here serves the retrieval tools, and admitting
// a write-only token would hand it reads the REST surface denies it. Refusing is
// the honest answer: there is no write-only shape to give it.
func (h *Handler) mcpServerFor(r *http.Request) (*mcp.Server, error) {
	scoper, ok := h.store.(memstore.StoreScoper)
	if !ok {
		return nil, errNoScoper
	}

	// An absent identity is the unauthenticated deployment -- no verifier and no
	// API key configured. That is an operator's choice, not a caller's, and
	// requireScope lets it through for the same reason.
	id, authed := IdentityFromContext(r.Context())
	if authed && !id.Allows(ScopeRead) {
		return nil, fmt.Errorf("MCP requires the read scope: %w", memstore.ErrNotPermitted)
	}

	write := !authed || id.Allows(ScopeWrite)
	p := memstore.Principal{UserID: id.UserID, Write: write}

	var (
		reg      mcpRegistrar
		readOnly = !write
	)
	if write {
		ws, err := scoper.WritableFor(p)
		if err != nil {
			return nil, err
		}
		reg = mcpserver.NewWriteServerWithConfig(ws, h.mcpConfig(r))
	} else {
		rs, err := scoper.ReadableFor(p)
		if err != nil {
			return nil, err
		}
		reg = mcpserver.NewMemoryServerWithConfig(rs, h.mcpConfig(r))
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "memstore",
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{
		Instructions: mcpserver.Instructions(readOnly),
		SchemaCache:  h.schemas,
	})
	reg.Register(srv)
	return srv, nil
}

// mcpServerVersion is the implementation version reported to clients. It tracks
// the stdio server's, which is what clients have been seeing.
const mcpServerVersion = "0.5.0"

// mcpConfig builds the tool configuration for one request.
//
// Everything here is the daemon's, not the caller's: the generator it was given,
// the session store scoped to this request, and the retrieval defaults its
// operator configured. What the caller can change, it changes per call, through
// the tool arguments phase 3 added.
//
// Config.Embed is deliberately left nil. Embedding at insert time needs
// vector-write authority, and in a daemon deployment the embed queue already
// owns that: a fact stored through MCP is embedded by the next backfill drain
// rather than by the request handler. Granting it here would hand every write
// request authority to rewrite any fact's vectors, to save a few seconds of
// searchability.
func (h *Handler) mcpConfig(r *http.Request) mcpserver.Config {
	cfg := mcpserver.Config{
		Generator:              h.generator,
		RerankMode:             h.rerankMode,
		RerankThreshold:        h.rerankThreshold,
		RerankCandidates:       h.rerankPoolSize,
		RerankRecallCandidates: h.recallPoolSize,
		RerankDocBytes:         h.rerankDocBytes,
		RerankRecallDocBytes:   h.recallDocBytes,
	}
	// The session store enables memory_rate_context, which records feedback and
	// so belongs behind the write scope, matching its REST equivalent
	// (POST /v1/context/feedback). Nothing here enforces that: the tool is
	// registered by WriteServer.Register, so a read server never advertises it
	// however this config is filled in. Passing the store either way keeps the
	// authorization in one place rather than splitting it across two.
	if ss := sessionFromCtx(r.Context(), h.sessionStore); ss != nil {
		cfg.SessionStore = ss
	}
	return cfg
}
