# MCP over HTTP, no local binary -- scope

Status: **draft for discussion**, 2026-08-24. Nothing here is decided. Branch: `feat/mcp-http-transport`.

## The date, and what actually landed

The revision in question is **2026-07-28**, not 07-23. (07-23 is the date GitHub announced its MCP server running the new spec on `go-sdk` v1.7.0-pre.3, which is probably where the number came from.) `go-sdk` **v1.7.0** shipped the same day as the spec and is the latest release; `go.mod` is already on it, so there is nothing to pull -- this branch starts from a current SDK.

This is not a transport tweak. 2026-07-28 rewrites the wire protocol:

- **Sessionless** (SEP-2567): no `Mcp-Session-Id`, no per-connection list variance.
- **Stateless** (SEP-2575): the `initialize` / `notifications/initialized` handshake is gone. Every request carries its own protocol version, client info, and capabilities in `_meta`. A new `server/discover` RPC replaces the handshake.
- **MRTR** (SEP-2322): server-initiated requests (sampling, elicitation, roots) are replaced by returning `InputRequiredResult` and having the client retry with `inputResponses`.
- **`subscriptions/listen`** replaces the GET stream and `resources/subscribe`.
- **No SSE resumability**: `Last-Event-ID` and event IDs are gone; a broken stream loses the in-flight request.
- **Required HTTP headers** `Mcp-Method`, `Mcp-Name`, `Mcp-Protocol-Version`, with body/header mismatch returning `-32020 HeaderMismatch`.
- **`ttlMs` / `cacheScope`** required on every list result and `resources/read`.
- **Deprecated**: roots, sampling, logging; HTTP+SSE transport; OAuth Dynamic Client Registration (in favour of Client ID Metadata Documents).

### The constraint that drives the whole design

From `mcp/streamable.go` in v1.7.0:

> The streamable HTTP transport supports every legacy SDK protocol version, but the SEP-2575 >= 2026-07-28 protocol is only supported when the transport is configured as stateless.

So `StreamableHTTPOptions.Stateless = true` is not one option among several. It is the price of admission for the new revision, and it is what forces most of the work below. A stateless server also serves legacy clients fine (they negotiate down), so this is not a compatibility cliff -- but it does mean **no per-session server state, ever**, and that is where memstore currently keeps things.

## Where memstore stands today

`memstore-mcp` in daemon mode is a thin per-user adapter: a stdio MCP server in front of `httpclient` talking to `memstored`. One process, one identity, one token, resolved once at startup:

- `applyTokenScopes` calls `/v1/whoami` at boot and decides `ReadOnly` for the process lifetime; `instructionsFor(readOnly)` builds the server instructions from that same answer, once.
- `MemoryServer` holds the store, embedder, generator, session store, and a mutex-guarded set of **runtime-mutable rerank tunables** (`memory_rerank_settings`) that the model adjusts mid-session.
- 13 tools registered on one `*mcp.Server`.
- `--hook` and `--transcript` are a second, non-MCP job in the same binary: the Stop hook shim (`stop-hook.mjs`) spawns `memstore-mcp --hook`, and the binary owns a pending-upload retry queue drained one item per Stop event.
- Local SQLite mode (no `--remote`) is a supported runtime with no token, no scope enforcement, and optionally no embedder.

`memstored` already has the pieces the HTTP MCP server needs: bearer-token auth resolving to an `Identity`, per-request store scoping via `storeFromCtx`, and per-route scope enforcement (`requireScope`) covering ~30 REST routes.

## Scope changes

### A. Serve MCP from memstored

**A1. One route, many methods -- the scope model collapses.** `requireScope` declares a scope per route, deliberately not as a lookup table (#119). MCP is a single `POST /mcp` carrying reads and writes alike, so route-level scope cannot express the policy. Authorization has to move to a per-tool check -- either inside each handler or in a receiving middleware mapping tool name to scope. This is the single most security-sensitive item on the list, because it is where a working route-level guarantee gets rebuilt from scratch.

**A2. Per-request server construction.** `NewStreamableHTTPHandler(getServer func(*http.Request) *Server, ...)` gives us the request, so identity comes from the existing auth middleware. But it means building a `MemoryServer` and registering 13 tools per request unless we cache by (user, scopes). Needs measurement before choosing.

**A3. `readOnly` and instructions become per-request.** Both currently derive from a boot-time `/v1/whoami` round trip. In-process there is no round trip -- the Identity is already in the context -- so this gets simpler, but `instructionsFor` and `applyTokenScopes`/`resolveReadOnly` have to move out of `cmd/memstore-mcp` into a package `memstored` can import, with their tests.

**A4. `cacheScope` must be `private`.** The advertised tool list varies by token: a read-scoped token sees only retrieval tools. Marking `tools/list` `public` would let a shared intermediary serve one caller's tool list to another. Getting this wrong is a real leak, and it is a one-word field.

### B. Statelessness fallout

**B1. `memory_rerank_settings` has nowhere to live.** These are explicitly "per-session overrides the model can adjust from observed performance." Stateless means a fresh server per request, so a setting made by one call is gone by the next. Three ways out: persist per user in the DB, demote to per-request parameters only (`memory_search` already takes `threshold` and `rerank_mode`), or drop the tool. **Decision needed** -- see D1 below.

**B2. No server-to-client requests.** Stateless rejects them outright. memstore does not use sampling, elicitation, or roots today (the curator and generator run server-side), so this costs nothing now, but it forecloses them later.

**B3. Progress and log notifications** only flow on the response stream of the request they belong to, and `notifications/message` is now gated on a per-request `_meta.logLevel`. memstore logs to stderr, so nothing to do.

### C. Retiring the local binary

**C1. What the binary does besides MCP.** The stdio server goes away cleanly. `--hook`, `--transcript`, and the pending-upload retry queue do not: `stop-hook.mjs` shells out to it, and the offline queue is Go code with no Node equivalent. The other five hooks already `fetch` `memstored` directly, so porting is mechanical -- except the retry queue, which is real behaviour with real logic. **Decision needed**: does "no local binary" mean no MCP binary, or nothing installed at all? The second requires porting the queue to Node.

**C2. Offline mode disappears.** Local SQLite mode (#55) means the memory tools degrade rather than vanish when the daemon is unreachable. Under HTTP-only, an unreachable `memstored` means Claude Code shows the server as failed and the tools are simply absent. The `memstore` CLI keeps its local mode either way. **Decision needed** -- acceptable, or does a fallback matter?

**C3. Config and setup surfaces.** `memstore setup` writes MCP registration and hook config; `~/.claude/settings.json` and `.claude.json` carry the stdio entry; earlier work added registrations for other harnesses (codex, zed). All of them change shape from `command` to `type: "http"` + `url` + `headers`.

### D. Auth and transport security

**D1. TLS stops being optional.** `memstored` runs `--tls-disabled` on the LAN today and logs a warning about it. Moving MCP onto that listener puts a bearer token in a Claude Code `headers` entry across an unencrypted LAN on every request, alongside the fact content itself. TLS is a prerequisite for this migration, not a follow-up.

**D2. Client config.** Claude Code supports `{"type": "http", "url": ..., "headers": {"Authorization": "Bearer ..."}}`, with `${VAR}` expansion in `.mcp.json` and `headersHelper` for tokens minted at connect time. A static token in `headers` is the least moving parts and matches the existing `api_tokens` model.

**D3. OAuth is a different role, and should be deferred.** 2026-07-28 expects an OAuth 2.1 resource server with protected-resource metadata and Client ID Metadata Documents. memstore's HTTP API is currently a **dumb OIDC relying party** delegating to webauth, per `oidc-federation-design.md` -- RP and resource server are not the same role, and that design has been re-litigated enough times that it should not be reopened as a side effect of a transport change. Recommendation: static bearer for phase 1, OAuth as its own scoped piece of work with the federation doc open.

**D4. Ingest scope must stay unreachable.** "Ingest is implied by nothing, including admin" is load-bearing for the document-corpus design. The MCP surface must not carry it, and the per-tool authorization in A1 is where that could silently regress.

### E. Protocol-revision work

**E1. Structured output.** `structuredContent` now accepts any JSON value and schemas loosened to full JSON Schema 2020-12. The fence `Envelope` (#164) should round-trip unchanged, but it is the thing most worth an explicit end-to-end test, since it is both the security boundary and the newest code.

**E2. `resultType`, `ttlMs`, `cacheScope`** are largely SDK-side; A4 is the part with a policy decision in it.

### F. Testing

The `mcpserver` tests call handlers directly and are transport-agnostic, so they survive the move intact -- that is the main reason this is tractable. What is missing and has to be written: an in-process `httptest` end-to-end pass over the streamable handler doing `server/discover` + `tools/list` + a tool call under **both** a read-scoped and a write-scoped token, asserting the advertised tool lists differ and that a write attempt on a read token is refused at the MCP layer rather than at the REST layer beneath it. Plus registration in the httpapi smoke harness.

## Open decisions

1. **Rerank tunables** (B1): persist per user, demote to per-request parameters, or drop `memory_rerank_settings`?
2. **"No local binary"** (C1): MCP only, or nothing installed -- and if nothing, who owns the pending-upload retry queue?
3. **Offline degradation** (C2): accept that memory tools vanish when `memstored` is down?
4. **Auth** (D2, D3): static bearer now with OAuth deferred, or do the OAuth work up front?
5. **TLS** (D1): confirmed prerequisite before any rollout?
6. **Multi-user** (A2): does the MCP surface become genuinely multi-identity now, or stay one-token-per-client with identity as a side effect?
7. **Per-tool authorization** (A1): middleware mapping tool to scope, or explicit checks in handlers?

## Suggested phasing

1. Move `instructionsFor` / `resolveReadOnly` out of `cmd/memstore-mcp` into a shared package, tests included. No behaviour change, unblocks everything else.
2. Per-tool authorization in `mcpserver`, tested against both token shapes, still under stdio.
3. Answer B1 -- the tunables question gates the handler signature.
4. TLS on `memstored`.
5. `POST /mcp` with `Stateless: true`, per-request server from Identity, end-to-end tests.
6. Cut over client config; port `stop-hook.mjs`.
7. Retire `cmd/memstore-mcp` (or reduce it to the hook shim, per C1).

## Non-goals

MRTR, `subscriptions/listen`, `x-mcp-header` passthrough, and MCP-as-a-channel are all newly available and none are needed for parity. Worth knowing they exist; not worth doing here.
