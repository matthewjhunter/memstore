# MCP over HTTP, no local binary -- scope

Status: **scoped**, 2026-08-24. Six of seven decisions taken; one open (2, the local binary). Phases 1-6 landed (6's deployment step is pending). Branch: `feat/mcp-http-transport`.

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

**Measured, and decided: share the schema cache, do not pool servers.** Registration was most of the cost -- 2.26ms for the 24 write-capable tools, against 3.8ms for a whole `tools/list` round trip -- because `AddTool` generates each schema by reflecting over the Go input and output types. The SDK's `ServerOptions.SchemaCache` exists for this deployment, naming "one `Server` per request" as its case; one shared instance on the `Handler` takes construction to 0.57ms, a 4x cut, and the documented unbounded-growth caveat does not apply to a fixed set of Go structs.

Pooling servers by (user, scopes) was rejected rather than deferred. A schema is a property of a type: identical for every caller, carrying no authority. A server is the object that encodes what one caller may do, so caching those puts a cache on the authorization boundary -- a revoked token would keep being handed a live `WriteServer` until something invalidated the entry, which means an invalidation path tied to the token store, and per-request state of exactly the kind SEP-2575 removed. The remaining 0.57ms is not worth buying with that.

Worth keeping in proportion: against real Postgres with a real embedder and cross-encoder, a `memory_search` runs tens to hundreds of milliseconds and construction disappears into it. Where it mattered is `tools/list` and `server/discover` -- the cheap, frequent calls where registration was most of the request. `BenchmarkMCPServerConstruction` keeps the number reproducible.

**A3. `readOnly` and instructions become per-request.** Both currently derive from a boot-time `/v1/whoami` round trip. In-process there is no round trip -- the Identity is already in the context -- so this gets simpler, but `instructionsFor` and `applyTokenScopes`/`resolveReadOnly` have to move out of `cmd/memstore-mcp` into a package `memstored` can import, with their tests.

**A4. `cacheScope` must be `private`.** The advertised tool list varies by token: a read-scoped token sees only retrieval tools. Marking `tools/list` `public` would let a shared intermediary serve one caller's tool list to another. Getting this wrong is a real leak, and it is a one-word field.

**Done in `9817cf5`, after being missed in phase 5.** Two things it turned out to need. The SDK applies the `public` default *after* the handler returns, so a handler cannot set the field and it has to be receiving middleware. And the policy is uniform rather than per-method -- `server/discover` carries the instructions, which differ for a read-only session, and any resource memstore serves is the caller's own memory -- so it is applied at `MemoryServer.Register`, the single funnel every server shape goes through. The `ttlMs` the SDK sends is 0, so a compliant client treated the response as immediately stale and the practical exposure was small; that is a mitigation, not the control.

### B. Statelessness fallout

**B1. `memory_rerank_settings` has nowhere to live.** These are explicitly "per-session overrides the model can adjust from observed performance." Stateless means a fresh server per request, so a setting made by one call is gone by the next. **Decided: demote to per-request parameters.** All six knobs -- `rerank_mode`, `threshold`, `weight`, `candidates`, `doc_bytes`, `timeout_seconds` -- are arguments to `memory_search` and `memory_get_context`, applying to the call that carries them and nothing else. An omitted knob falls back to the daemon's configuration. `memory_rerank_settings` loses its setter and becomes, at most, a reporter of the daemon's effective policy.

**B2. No server-to-client requests.** Stateless rejects them outright. memstore does not use sampling, elicitation, or roots today (the curator and generator run server-side), so this costs nothing now, but it forecloses them later.

**B3. Progress and log notifications** only flow on the response stream of the request they belong to, and `notifications/message` is now gated on a per-request `_meta.logLevel`. memstore logs to stderr, so nothing to do.

### C. Retiring the local binary

**C1. What the binary does besides MCP.** The stdio server goes away cleanly. `--hook`, `--transcript`, and the pending-upload retry queue do not: `stop-hook.mjs` shells out to it, and the offline queue is Go code with no Node equivalent. The other five hooks already `fetch` `memstored` directly, so porting is mechanical -- except the retry queue, which is real behaviour with real logic. **Decision needed**: does "no local binary" mean no MCP binary, or nothing installed at all? The second requires porting the queue to Node.

**C2. Local-only capability disappears.** Not a fallback: the two modes are an either-or chosen at launch by whether `--remote` is set, and a daemon-mode process has never degraded to SQLite when the daemon went away. What HTTP-only removes is the *deployment* where memstore runs with no daemon and no Postgres at all -- one binary and a file.

`memstored` is Postgres-only (`pgstore.New`, plus a pgstore-only token store and session store), so "just run `memstored` on loopback" is not a free substitute. Preserving local-only means either keeping a stdio binary for that one deployment, or giving `memstored` a SQLite backend including a token store or a no-auth loopback mode -- each its own piece of work. The `memstore` CLI keeps local SQLite either way, so the loss is scoped to the MCP surface.

A third factor cuts against local-only independent of the transport: **memstore leans on model services throughout.** Retrieval quality depends on an embedder and a cross-encoder reranker, and extraction and curation on a chat model. Local-only is therefore already a choice between FTS5-only search (`--no-embeddings`, meaningfully worse retrieval) and hosting an embed-plus-rerank stack locally -- which a workstation with a 5070 can do and most laptops cannot. The "one binary and a file" pitch is only honest for the FTS-only configuration.

**Decision needed**, and it is an adoption question rather than a reliability one. For Matthew personally: almost never needed. For potential users it is the difference between trying memstore with one binary and a file, and standing up Postgres, a daemon, and a token before the first search -- weighed against the fact that the cheap local configuration is also the weakest one.

**C3. Config and setup surfaces.** `memstore setup` writes MCP registration and hook config; `~/.claude/settings.json` and `.claude.json` carry the stdio entry; earlier work added registrations for other harnesses (codex, zed). All of them change shape from `command` to `type: "http"` + `url` + `headers`.

There are four harnesses, not two, and they are in `Taskfile.yml` rather than in Go or Markdown -- which is why an earlier pass through the source missed two of them:

| Harness | Task | State |
|---|---|---|
| Claude Code | `install:claude`, `memstore setup` | **Done** (phase 6): HTTP transport when a daemon is reachable. |
| Cursor | `install:cursor` | **Done**: `{"url", "headers"}` merged into `~/.cursor/mcp.json`. |
| Zed | `install:zed` | **Done**: prints a `context_servers` block with `url` + `headers`. |
| Codex | `install:codex`, `examples/codex` | Stale, **deferred** by decision: not in regular use. Also pipes to `memstore-mcp --hook`. |

Cursor and Zed both support remote MCP servers natively, so both were cut over rather than deferred: Cursor takes `{"url", "headers"}` in `mcp.json`, and Zed takes the same shape under `context_servers`. (The `mcp-remote` shim in older write-ups is stale -- zed-industries/zed#37770 is closed and the current docs show the native form.) With those done, **Codex is the only harness left on the stdio binary, and it is deferred, so nothing gates phase 7 on the harness side.**

Two details worth keeping. Neither Cursor nor Zed is documented to expand `${VAR}` inside a header value, the way Claude Code does, so the token is written literally and `install:cursor` tightens the file to 0600 and says so. And Zed starts the MCP OAuth flow when a remote server has no `Authorization` header configured -- which memstore does not implement (decision 4) -- so the snippet keeps the header even for a daemon running without auth.

### D. Auth and transport security

**D1. Plaintext stops being a default anyone can fall into.** `memstored` runs `--tls-disabled` on the LAN today and logs a warning nobody reads. Moving MCP onto that listener puts a bearer token in a Claude Code `headers` entry across it on every request, alongside the fact content itself.

The requirement is not that Matthew's LAN gets a certificate -- his network is trusted enough for what runs on it, and that is his call to make. It is that memstore must not *require* a trusted LAN from anyone else who runs it. So the prerequisite is a safe default rather than a deployment: `--tls-disabled` alone no longer starts the daemon, and an operator who wants plaintext affirms once, explicitly, that the listener is reachable only over a path they control.

The daemon cannot decide this for itself. Under Docker a proxy-fronted deployment binds `0.0.0.0` inside a private network, indistinguishable from `0.0.0.0` on a routable LAN, so a check that sniffed the interface would refuse the safe case and get switched off out of irritation -- leaving nothing.

**D2. Client config.** Claude Code supports `{"type": "http", "url": ..., "headers": {"Authorization": "Bearer ..."}}`, with `${VAR}` expansion in `.mcp.json` and `headersHelper` for tokens minted at connect time. A static token in `headers` is the least moving parts and matches the existing `api_tokens` model.

**D3. OAuth is a different role, and should be deferred.** 2026-07-28 expects an OAuth 2.1 resource server with protected-resource metadata and Client ID Metadata Documents. memstore's HTTP API is currently a **dumb OIDC relying party** delegating to webauth, per `oidc-federation-design.md` -- RP and resource server are not the same role, and that design has been re-litigated enough times that it should not be reopened as a side effect of a transport change. **Decided: defer.** Static bearer for this migration; OAuth becomes its own scoped work once the transport is finished, with the federation doc open when it happens.

**D4. Ingest scope must stay unreachable.** "Ingest is implied by nothing, including admin" is load-bearing for the document-corpus design. The MCP surface must not carry it, and the per-tool authorization in A1 is where that could silently regress.

**D5. Mount under a path prefix.** Nothing in the spec or the SDK requires the endpoint at the root: the transport is defined as a single POST endpoint and the client is configured with a full URL, path included. `StreamableHTTPHandler.ServeHTTP` never reads `req.URL.Path`, so `mux.Handle("POST /memstore/mcp", h)` behaves exactly as `/mcp` would. **Decided: mount under `/memstore/`**, so the MCP endpoint, the REST API, and the eventual human web UI share one namespace and no URL migration is needed when memstore stops being the only thing on the host.

One consequence to carry into the OAuth work: RFC 9728 derives protected-resource metadata by inserting the resource path into the well-known URI, so a prefixed endpoint is advertised at `/.well-known/oauth-protected-resource/memstore/mcp` rather than the root form. Mechanical, but it has to be served at the derived path.

### E. Protocol-revision work

**E1. Structured output.** `structuredContent` now accepts any JSON value and schemas loosened to full JSON Schema 2020-12. The fence `Envelope` (#164) should round-trip unchanged, but it is the thing most worth an explicit end-to-end test, since it is both the security boundary and the newest code.

**E2. `resultType`, `ttlMs`, `cacheScope`** are largely SDK-side; A4 is the part with a policy decision in it.

**E3. A client that only speaks `initialize` silently gets 2025-11-25.** `negotiatedVersion` in the SDK's `mcp/shared.go` returns the client's version only when it is both supported and *below* 2026-07-28; everything else falls through to a hardcoded 2025-11-25. So an `initialize` request naming 2026-07-28 is answered `2025-11-25` -- not an error, and not the version asked for. This is deliberate on the SDK's side (`initialize` is the deprecated handshake the new revision removes, so it cannot be the thing that selects it), but it means the failure mode for a stale client is a quiet downgrade: it loses `ttlMs`/`cacheScope` and gets the legacy request shape, with nothing in the response saying a downgrade happened.

The only route to 2026-07-28 is `server/discover` plus the per-request `_meta`. Verified against the deployed daemon on 2026-08-24: `server/discover` answers `supportedVersions: [2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05]` with `cacheScope: private`, and a stateless `tools/list` returns all 22 tools, also `private`. The preconditions are enforced one at a time, each with its own error -- missing `_meta` `protocolVersion` is `-32602`, then `Mcp-Protocol-Version` header missing is `-32020`, then `Mcp-Method` missing is `-32020`, then `_meta` `clientCapabilities` missing is `-32602` -- so a client being built against this gets told which piece it is missing rather than one opaque rejection.

### F. Testing

The `mcpserver` tests call handlers directly and are transport-agnostic, so they survive the move intact -- that is the main reason this is tractable. What is missing and has to be written: an in-process `httptest` end-to-end pass over the streamable handler doing `server/discover` + `tools/list` + a tool call under **both** a read-scoped and a write-scoped token, asserting the advertised tool lists differ and that a write attempt on a read token is refused at the MCP layer rather than at the REST layer beneath it. Plus registration in the httpapi smoke harness.

## Decisions

1. **Rerank tunables** (B1) -- **decided**: demote to per-request parameters. This gates the handler signature, so it unblocks code.
2. **"No local binary"** (C1) -- *open*, and now the only open question. Leaning to redesigning ingestion and the hook helpers so no local binary is required at all, rather than porting the retry queue as-is.
3. **Local-only capability** (C2) -- **decided**: dropped. Claude Code ships its own local-only memory, so memstore does not need to compete for that slot; memstore's value is the cross-session, cross-repo, server-backed layer. The model-service dependency argued the same way -- the cheap local configuration was also the weakest one.
4. **OAuth** (D3) -- **decided**: defer. Fix the transport first.
5. **TLS** (D1) -- **confirmed**, and rescoped: the prerequisite is that memstore never *requires* a trusted LAN, not that this deployment gets a certificate. Plaintext now costs an explicit affirmation; a trusted LAN remains a legitimate answer to give.
6. **Multi-identity** (A2) -- **decided**: in scope, as the step immediately after the transport work. It is a planned feature in its own right, and serving MCP per-request from an Identity is the natural place it lands; the transport migration should not foreclose it, but does not have to deliver it.
7. **Per-tool authorization** (A1) -- **decided**: enforce at the existing registration split (option C below).

### Per-tool authorization: the options

**A. Middleware mapping tool name to scope.** A `map[string]Scope` consulted before dispatch. This is #119's objection to a route table, restated: the map is a second copy of a fact declared elsewhere and rots silently. A tool added without a map entry gets the default -- deny and it silently 403s, allow and it silently bypasses. Neither failure announces itself.

**B. Explicit check in each of the 24 handlers.** Nothing to drift, but 24 chances to omit one, and an omission is invisible.

**C. Enforce at the registration split (chosen, then superseded by the type split below).** `addWriteTool` already declares write-ness at the definition site: 12 write tools, 12 read. Today it only hides the tool when `readOnly`. Making it also wrap the handler in a scope check gives advertisement and enforcement a single declaration, at the site where the tool is defined -- the exact analogue of `h.requireScope(scope, handler)` in `registerRoutes`, which #119 chose deliberately over a lookup table. A new write tool is enforced by construction, and ingest stays unreachable because only two categories exist.

The cost to price in: `addWriteTool` versus `mcp.AddTool` is currently a UX decision about what to advertise, and promoting it to a security decision means auditing all 24 existing classifications once. That audit should end in a test that enumerates the registered tools and asserts each is reachable under exactly the intended scope, so it never has to be repeated by hand.

**D. Capability-typed store handles (implemented).** The store interface splits by what the caller may do -- `ReadableStore`, `WritableStore`, `EmbedStore` -- and `StoreScoper` mints handles for a `Principal`, refusing a writable one to a principal that may not write. `MemoryServer` holds a read handle and serves the retrieval tools; `WriteServer` embeds it, holds the writable handle, and serves the mutations. The runtime authorizer from option C is deleted: a read-capable server has no write handler to register, because those handlers are methods on the other type, and that is checked by the compiler rather than by a test.

Three things this settled that were not obvious going in. `Touch` sits on `ReadableStore` -- it is read telemetry, and pushing it below the interface into `Search` would have counted the recall pipeline and extraction's dedup query as tool-call uses, undoing #158. Embedding at insert time needs vector-write authority, which is granted through `Config.Embed` rather than derived from the writable handle -- deriving it by type assertion would be the move `ReadOnly` exists to prevent, run in the other direction, and authority discovered rather than given is not granted at all. Only local SQLite mode grants it, because it is the only configuration with no daemon to drain the embed queue; in daemon mode the embedder is nil and the async backfill has always been the whole story, so this path leaves the MCP surface entirely when local mode is retired in phase 7. And the scopers return a wrapper promoting only the read method set, because every backend satisfies `WritableStore` and a bare read handle could otherwise be asserted straight back up to it.

## Phases

| # | Status | Phase | Notes |
|---|--------|-------|-------|
| 1 | **done** | Move the instructions and the read-only decision into `mcpserver` | `d69ebee`. Pure motion; the rendered instruction text was verified byte-identical to what shipped. |
| 2 | **done** | Capability-typed stores and the server split | Four commits, below. |
| 3 | **done** | Demote the rerank tunables to per-request parameters | `2d3f5f0` removed the setter and the per-session state; a follow-up put all six knobs on `memory_search` and `memory_get_context` as per-call arguments. Omitted knobs fall back to the daemon's configuration, which `memory_rerank_settings` reports. |
| 4 | **done** | Plaintext requires an explicit affirmation | Decision 5, rescoped: the product must not assume a trusted LAN, so `--tls-disabled` alone refuses to start. Matthew's own deployment stays plaintext on a trusted LAN, now stated rather than assumed. |
| 5 | **done** | `POST /memstore/mcp`, `Stateless: true` | Two commits, below. |
| 6 | **done** | Cut over client config; port `stop-hook.mjs` | Two commits, below. Deployed 2026-08-27: `memstore setup --force` on halo and zero (serenity has no memstore install), then the root alias removed in `42ab8b1` and the daemon redeployed. The root answers 404 now. |
| 7 | **0.6.0 done**; 0.7.0 pending | Retire `cmd/memstore-mcp` and the SQLite backend | 0.4.0 and 0.5.0 shipped as planned below; the 0.6.0 deletion landed 2026-08-29. Decided 2026-08-27, split the same day. **0.4.0 (tagged):** deprecation announced; nothing removed. **0.5.0:** the HTTP, MCP, and client tests run on both backends via `internal/teststore` and `MEMSTORE_TEST_BACKEND` (landed 2026-08-27; the first pg run found `TermDocCounts` returning 0 for every stemmable term, so recall's IDF on the daemon had been wrong); the SQLite run and the conformance suite stay until the backend is removed; `setup` stops registering the stdio binary and the binary warns at startup; the Codex shim moves to `memstore hook`; `memstore import` gains a daemon target (today it only writes a SQLite file, so the documented export-then-import path does not exist yet). **0.6.0:** delete the binary, `Config.Embed`, `--read-only`, setup's stdio fallback, and the SQLite backend except the read-only opener `export --db` needs. **0.7.0:** drop the export reader. The split exists so tests that pass on SQLite and fail on Postgres can be checked against both while both still exist. |
| 8 | | Multi-identity | Decision 6. Its own work on top of a finished transport, not part of it. |

### Phase 6, in the order it happened

| Commit | What |
|--------|------|
| `2e46f3b` | Hook capture moves to `internal/hookcapture`, entered by `memstore hook`. `memstore-mcp` keeps `--hook` and `--transcript` as deprecated aliases onto the same code, so a machine whose hook script and whose binaries are updated in either order keeps working. |
| `f296211` | `memstore setup` derives every client's configuration from one detected base URL, prefers the daemon's prefix, and registers MCP over HTTP rather than as a local stdio binary. |

**Decision 2 is now answerable.** It asked whether "no local binary" meant no MCP binary or nothing installed at all, and the blocker was the pending-upload retry queue: real Go logic with no Node equivalent, reached only through `memstore-mcp`. It is no longer reached only through `memstore-mcp` -- it is a package, entered by the CLI, and nothing about it was ever MCP. So retiring `cmd/memstore-mcp` costs the queue nothing. What a local install still buys is local-only SQLite mode and embed-on-insert, which is the question phase 7 actually has to answer.

**The root alias was deliberately left in place until the clients moved** (removed 2026-08-27, `42ab8b1`). Every configured client on a running machine still addresses the daemon at the root, and they are configured in several places that change on their own schedule. Removing the alias is a deployment step, not a code change: run `memstore setup --force` on each client machine, confirm nothing is still calling the root, then delete the alias from `httpapi.Mount`. Doing it in the other order breaks a working deployment for no gain -- and leaving it forever means the prefix bought nothing, because everything will still be addressing the root.

One thing found while cutting over, unrelated to the transport but in the same code: daemon detection asked for `/healthz`, a path memstored has never served, and counted any response -- including the 404 -- as success. It detected any web server listening on the port. It asks for `/v1/health` now and requires a 2xx.

### Phase 5, in the order it happened

| Commit | What |
|--------|------|
| `ab0087f` | The daemon moves under `/memstore`, with the root kept as a transition alias and `/.well-known/` reserved for the host. `httpapi.Mount` is the composition contract: a module registers routes relative to `/` and never learns where it is mounted. |
| `d453740` | The MCP endpoint. A per-request server built from the request's identity through `StoreScoper`; the tool set a client sees is the authorization result rather than a filter over it. |

Two things settled here that were not in the original scope.

**Read scope is required to reach MCP at all.** `WritableStore` embeds `ReadableStore`, so every server shape the endpoint can build serves the retrieval tools. A write-only token admitted to MCP would therefore gain reads that `GET /v1/facts` refuses it. There is no write-only server shape to hand it instead, so the endpoint refuses with 403 rather than quietly widening the token.

**The route carries no `/v1`.** MCP versions itself through the protocol-version header its own spec defines. A second version number in the path could only ever disagree with it.

The root alias from `ab0087f` is deliberately temporary, and removing it is phase 6's job. Until it goes, every existing client still addresses the daemon at the root -- which is what makes the cutover safe, and also what makes the prefix worth nothing if the alias is forgotten.

### Phase 2, in the order it happened

| Commit | What |
|--------|------|
| `42943d6` | A runtime write authorizer at the tool registration split. **Superseded within the phase** by the type split, and deleted in `9ee9374`. |
| `8abf7a0` | `ReadableStore` / `WritableStore` / `EmbedStore`, `Principal`, and `StoreScoper`, with `ReadOnly` to stop a read handle widening itself. |
| `9ee9374` | `MemoryServer` holds the read handle; `WriteServer` embeds it and holds the writable one. The authorizer, `Config.ReadOnly`, and `addWriteTool` all deleted. |
| `a4b9da5` | Vector-write authority granted through `Config.Embed` rather than derived by type assertion. |

## Non-goals

MRTR, `subscriptions/listen`, `x-mcp-header` passthrough, and MCP-as-a-channel are all newly available and none are needed for parity. Worth knowing they exist; not worth doing here.
