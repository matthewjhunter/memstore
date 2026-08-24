# MCP over HTTP, no local binary -- scope

Status: **scoped**, 2026-08-24. Five of seven decisions taken (1, 4, 5, 6, 7); two open (2, 3). Branch: `feat/mcp-http-transport`.

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

**A1. Authorization does not move -- it vanishes.** In daemon mode `ms.store` *is* the `httpclient`, so every MCP write travels out over HTTP and lands on a REST route guarded by `requireScope(ScopeWrite, ...)`. The MCP layer's `--read-only` is only an advertisement filter; enforcement is the daemon's, one layer down, which is why `applyTokenScopes` can "only tighten the answer, never loosen it".

In-process, `ms.store` becomes the pgstore directly. The REST routes leave the path and `requireScope` with them, so the guarantee is not relocated -- it disappears unless deliberately rebuilt. **Decided: option C below.**

**A2. Per-request server construction.** `NewStreamableHTTPHandler(getServer func(*http.Request) *Server, ...)` gives us the request, so identity comes from the existing auth middleware. But it means building a `MemoryServer` and registering 13 tools per request unless we cache by (user, scopes). Needs measurement before choosing.

**A3. `readOnly` and instructions become per-request.** Both currently derive from a boot-time `/v1/whoami` round trip. In-process there is no round trip -- the Identity is already in the context -- so this gets simpler, but `instructionsFor` and `applyTokenScopes`/`resolveReadOnly` have to move out of `cmd/memstore-mcp` into a package `memstored` can import, with their tests.

**A4. `cacheScope` must be `private`.** The advertised tool list varies by token: a read-scoped token sees only retrieval tools. Marking `tools/list` `public` would let a shared intermediary serve one caller's tool list to another. Getting this wrong is a real leak, and it is a one-word field.

### B. Statelessness fallout

**B1. `memory_rerank_settings` has nowhere to live.** These are explicitly "per-session overrides the model can adjust from observed performance." Stateless means a fresh server per request, so a setting made by one call is gone by the next. **Decided: demote to per-request parameters.** `memory_search` and `memory_get_context` already take `threshold` and `rerank_mode`; the remaining knobs (weight, candidate pools, doc bytes, timeout) either join them or fall back to the daemon's configured defaults. `memory_rerank_settings` loses its setter and becomes, at most, a reporter of the daemon's effective policy.

**B2. No server-to-client requests.** Stateless rejects them outright. memstore does not use sampling, elicitation, or roots today (the curator and generator run server-side), so this costs nothing now, but it forecloses them later.

**B3. Progress and log notifications** only flow on the response stream of the request they belong to, and `notifications/message` is now gated on a per-request `_meta.logLevel`. memstore logs to stderr, so nothing to do.

### C. Retiring the local binary

**C1. What the binary does besides MCP.** The stdio server goes away cleanly. `--hook`, `--transcript`, and the pending-upload retry queue do not: `stop-hook.mjs` shells out to it, and the offline queue is Go code with no Node equivalent. The other five hooks already `fetch` `memstored` directly, so porting is mechanical -- except the retry queue, which is real behaviour with real logic. **Decision needed**: does "no local binary" mean no MCP binary, or nothing installed at all? The second requires porting the queue to Node.

**C2. Local-only capability disappears.** Not a fallback: the two modes are an either-or chosen at launch by whether `--remote` is set, and a daemon-mode process has never degraded to SQLite when the daemon went away. What HTTP-only removes is the *deployment* where memstore runs with no daemon and no Postgres at all -- one binary and a file.

`memstored` is Postgres-only (`pgstore.New`, plus a pgstore-only token store and session store), so "just run `memstored` on loopback" is not a free substitute. Preserving local-only means either keeping a stdio binary for that one deployment, or giving `memstored` a SQLite backend including a token store or a no-auth loopback mode -- each its own piece of work. The `memstore` CLI keeps local SQLite either way, so the loss is scoped to the MCP surface.

A third factor cuts against local-only independent of the transport: **memstore leans on model services throughout.** Retrieval quality depends on an embedder and a cross-encoder reranker, and extraction and curation on a chat model. Local-only is therefore already a choice between FTS5-only search (`--no-embeddings`, meaningfully worse retrieval) and hosting an embed-plus-rerank stack locally -- which a workstation with a 5070 can do and most laptops cannot. The "one binary and a file" pitch is only honest for the FTS-only configuration.

**Decision needed**, and it is an adoption question rather than a reliability one. For Matthew personally: almost never needed. For potential users it is the difference between trying memstore with one binary and a file, and standing up Postgres, a daemon, and a token before the first search -- weighed against the fact that the cheap local configuration is also the weakest one.

**C3. Config and setup surfaces.** `memstore setup` writes MCP registration and hook config; `~/.claude/settings.json` and `.claude.json` carry the stdio entry; earlier work added registrations for other harnesses (codex, zed). All of them change shape from `command` to `type: "http"` + `url` + `headers`.

### D. Auth and transport security

**D1. TLS stops being optional.** `memstored` runs `--tls-disabled` on the LAN today and logs a warning about it. Moving MCP onto that listener puts a bearer token in a Claude Code `headers` entry across an unencrypted LAN on every request, alongside the fact content itself. TLS is a prerequisite for this migration, not a follow-up.

**D2. Client config.** Claude Code supports `{"type": "http", "url": ..., "headers": {"Authorization": "Bearer ..."}}`, with `${VAR}` expansion in `.mcp.json` and `headersHelper` for tokens minted at connect time. A static token in `headers` is the least moving parts and matches the existing `api_tokens` model.

**D3. OAuth is a different role, and should be deferred.** 2026-07-28 expects an OAuth 2.1 resource server with protected-resource metadata and Client ID Metadata Documents. memstore's HTTP API is currently a **dumb OIDC relying party** delegating to webauth, per `oidc-federation-design.md` -- RP and resource server are not the same role, and that design has been re-litigated enough times that it should not be reopened as a side effect of a transport change. **Decided: defer.** Static bearer for this migration; OAuth becomes its own scoped work once the transport is finished, with the federation doc open when it happens.

**D4. Ingest scope must stay unreachable.** "Ingest is implied by nothing, including admin" is load-bearing for the document-corpus design. The MCP surface must not carry it, and the per-tool authorization in A1 is where that could silently regress.

### E. Protocol-revision work

**E1. Structured output.** `structuredContent` now accepts any JSON value and schemas loosened to full JSON Schema 2020-12. The fence `Envelope` (#164) should round-trip unchanged, but it is the thing most worth an explicit end-to-end test, since it is both the security boundary and the newest code.

**E2. `resultType`, `ttlMs`, `cacheScope`** are largely SDK-side; A4 is the part with a policy decision in it.

### F. Testing

The `mcpserver` tests call handlers directly and are transport-agnostic, so they survive the move intact -- that is the main reason this is tractable. What is missing and has to be written: an in-process `httptest` end-to-end pass over the streamable handler doing `server/discover` + `tools/list` + a tool call under **both** a read-scoped and a write-scoped token, asserting the advertised tool lists differ and that a write attempt on a read token is refused at the MCP layer rather than at the REST layer beneath it. Plus registration in the httpapi smoke harness.

## Decisions

1. **Rerank tunables** (B1) -- **decided**: demote to per-request parameters. This gates the handler signature, so it unblocks code.
2. **"No local binary"** (C1) -- *open*, leaning to dropping the binary entirely. If nothing is installed, the pending-upload retry queue has to be ported to Node.
3. **Local-only capability** (C2) -- *open*, leaning to dropping it. Adoption question, not a reliability one; weakened further by memstore's dependence on embedding and rerank services that most laptops cannot host well.
4. **OAuth** (D3) -- **decided**: defer. Fix the transport first.
5. **TLS** (D1) -- **confirmed** prerequisite. No rollout before it.
6. **Multi-identity** (A2) -- **decided**: in scope, as the step immediately after the transport work. It is a planned feature in its own right, and serving MCP per-request from an Identity is the natural place it lands; the transport migration should not foreclose it, but does not have to deliver it.
7. **Per-tool authorization** (A1) -- **decided**: enforce at the existing registration split (option C below).

### Per-tool authorization: the options

**A. Middleware mapping tool name to scope.** A `map[string]Scope` consulted before dispatch. This is #119's objection to a route table, restated: the map is a second copy of a fact declared elsewhere and rots silently. A tool added without a map entry gets the default -- deny and it silently 403s, allow and it silently bypasses. Neither failure announces itself.

**B. Explicit check in each of the 24 handlers.** Nothing to drift, but 24 chances to omit one, and an omission is invisible.

**C. Enforce at the registration split (chosen).** `addWriteTool` already declares write-ness at the definition site: 12 write tools, 12 read. Today it only hides the tool when `readOnly`. Making it also wrap the handler in a scope check gives advertisement and enforcement a single declaration, at the site where the tool is defined -- the exact analogue of `h.requireScope(scope, handler)` in `registerRoutes`, which #119 chose deliberately over a lookup table. A new write tool is enforced by construction, and ingest stays unreachable because only two categories exist.

The cost to price in: `addWriteTool` versus `mcp.AddTool` is currently a UX decision about what to advertise, and promoting it to a security decision means auditing all 24 existing classifications once. That audit should end in a test that enumerates the registered tools and asserts each is reachable under exactly the intended scope, so it never has to be repeated by hand.

## Suggested phasing

1. Move `instructionsFor` / `resolveReadOnly` out of `cmd/memstore-mcp` into a shared package, tests included. No behaviour change, unblocks everything else.
2. Per-tool authorization in `mcpserver`, tested against both token shapes, still under stdio.
3. Answer B1 -- the tunables question gates the handler signature.
4. TLS on `memstored`.
5. `POST /mcp` with `Stateless: true`, per-request server from Identity, end-to-end tests.
6. Cut over client config; port `stop-hook.mjs`.
7. Retire `cmd/memstore-mcp` (or reduce it to the hook shim, per C1).
8. Multi-identity, as its own piece of work on top of the finished transport.

## Non-goals

MRTR, `subscriptions/listen`, `x-mcp-header` passthrough, and MCP-as-a-channel are all newly available and none are needed for parity. Worth knowing they exist; not worth doing here.
