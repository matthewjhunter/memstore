# MCP OAuth -- scope

Status: **scoped**, 2026-08-24. Follows from decision D3 of `mcp-http-transport-scope.md`, which deferred OAuth and said it "becomes its own scoped work once the transport is finished, with the federation doc open when it happens." The transport is finished; this is that work. Branch: `feat/mcp-http-transport`.

## The role question, settled first

`oidc-federation-design.md` invariant 3 says websites are dumb relying parties that never implement federation. That invariant is about **websites authenticating users**, and it does not govern this work, because memstore is not acting as a relying party here.

An MCP server under 2026-07-28 is an **OAuth 2.1 resource server**: it receives an access token it did not mint, validates it, and maps it to an identity and a set of permissions. It never runs an authorization-code flow, never holds a client secret, never sees a password, and never talks to an upstream identity provider. The client -- Claude Code, Cursor, Zed -- is the party that runs the flow. RP and RS are different roles with different code, and memstore only needs the second one.

So the federation invariants are satisfied by construction rather than by care: **webauth remains the only federation client**, and memstore becomes one more thing that trusts webauth's signature. Nothing here reopens the design.

Two corrections that fall out of establishing this:

- memstore's `CLAUDE.md` currently says the HTTP API "authenticates as a **dumb OIDC relying party** via `oidclient`, delegating to **webauth**." That is not implemented and, per the above, is not the role we want. `oidclient` is not in `go.mod` and there is no OIDC code in the tree. Today's auth is static bearer tokens (`pgstore` `api_tokens`), a legacy single key, and mTLS peer identity. That paragraph should be rewritten to describe a resource server.
- The `resource` identifier for memstore is the MCP endpoint URL, which is prefixed (decision D5 of the transport scope). RFC 9728 derives metadata by path insertion, so it is served at `/.well-known/oauth-protected-resource/memstore/mcp`, not the root form.

## Where things stand

**memstore side.** Small, and the seam already exists. `httpapi.TokenVerifier` (`httpapi/handler.go:29`) is a one-method interface -- `VerifyToken(ctx, token) (Identity, error)` -- and `ServeHTTP` resolves an `Identity` at a single point before dispatch. A JWT verifier is a second implementation of that interface, so handlers, `requireScope`, and the per-request MCP server construction all keep working untouched. The SDK supplies `auth.RequireBearerToken` and `auth.ProtectedResourceMetadataHandler`, and `oauthex.MatchesResource` implements the audience comparison with the trailing-slash relaxation that real IdPs require.

One shape mismatch to absorb: memstore's `TokenVerifier` takes only the token string, while the SDK's takes `(ctx, token, req)`. Verifying a JWT needs nothing from the request, so the adapter can discard it, but the interface should probably gain the request rather than pretend it does not exist.

**webauth side.** It is already a real authorization server -- `/t/{tid}/authorize`, `/token`, `/userinfo`, `/.well-known/openid-configuration`, `/.well-known/jwks.json`, `/register-client` -- with PKCE implemented in `internal/authcode` and per-tenant RSA signing keyed by `kid = tenant_id`. `https://webauth.infodancer.net/t/infodancer` is live and serving discovery today.

What it does **not** do is mint tokens a resource server can validate:

- **No `aud` claim on the access token at all.** `token.IssueToken` (`internal/token/token.go:50`) sets issuer, subject, tenant, email, name, roles, and no audience. Only `IssueIDToken` sets one, to the `client_id`, which is the OIDC convention and the wrong value for this purpose.
- **No RFC 8707 resource indicators.** A client cannot ask for a memstore-audience token because the `resource` parameter is not read at `/authorize` or `/token`.
- **No `scope` plumbing.** Scopes are not requested, granted, or stamped into the token, so every OAuth caller would arrive at memstore with an empty scope set -- which `Identity.Allows` treats as read+write.
- **Discovery gaps.** `tenantDiscovery` omits `code_challenge_methods_supported`, `grant_types_supported`, and `token_endpoint_auth_methods_supported`. Clients commonly refuse an AS that does not advertise S256.
- **Metadata path.** Only the OIDC form `/t/{tid}/.well-known/openid-configuration` is served. MCP clients try the RFC 8414 insertion form `/.well-known/oauth-authorization-server/t/{tid}` first.

The missing `aud` is the one that matters, and it is not a formality. The same webauth tenant is meant to serve several applications, so a token minted for one is today indistinguishable from a token minted for memstore. Audience binding is the control that stops an application from replaying its users' tokens against a peer, and without it memstore would be trusting every holder of any token from that issuer.

## Scope changes

### A. memstore as a resource server

**A1. Protected resource metadata.** Serve `oauthex.ProtectedResourceMetadata` at `/.well-known/oauth-protected-resource/memstore/mcp` via `auth.ProtectedResourceMetadataHandler`, declaring `resource` (the canonical MCP endpoint URL), `authorization_servers` (the webauth tenant issuer), `scopes_supported`, and `bearer_methods_supported: ["header"]`. Unauthenticated, and CORS-open by the SDK's handler, which is correct -- it is public discovery data.

**A2. Challenge on 401.** `ServeHTTP`'s unauthorized path must add `WWW-Authenticate: Bearer resource_metadata="..."`. Without it a client has no way to discover where to authenticate and the flow never starts. This is the one change to the existing auth dispatch rather than an addition alongside it.

**A3. Token validation.** Fetch and cache the tenant JWKS; validate signature, `iss` against the configured tenant issuer, `exp`, and `aud` against the resource identifier via `oauthex.MatchesResource`. Pin the algorithm to RS256 and reject `alg: none` and any HMAC variant explicitly rather than relying on the library's defaults. Cache JWKS with a bounded TTL and refresh on unknown `kid` so key rotation does not require a restart, with a floor on refresh frequency so an unknown-kid flood cannot be turned into a request amplifier against webauth.

**A4. Mapping a token to an Identity.** `sub` identifies the webauth user; `memstore_users` is keyed `(namespace, name)`. A token whose `sub` has no user row **autoprovisions one** (decision 5). Scopes come from the token's `scope` claim, filtered through the rules in `httpapi/scopes.go`, which stay exactly as they are.

The provisioned row is keyed on `sub`, never on the email address. `sub` is stable and non-reassignable; an email address can change, and at some providers can be released and re-registered by a different person -- keying on it would make account takeover a matter of waiting for an address to be recycled. Email goes on the row as a display attribute and is never the lookup key.

**A5. Ingest stays unreachable, and OAuth must not become the way in.** "Ingest is implied by nothing, including admin" is the guarantee behind the document corpus. The OAuth path must never grant `ingest`, whatever the token says, and this should be enforced at the point of mapping rather than left to webauth's configuration -- a filter memstore applies to its own inputs, not a promise it asks another service to keep.

**A6. Static tokens coexist.** OAuth is for interactive harnesses. The hooks, the offline retry queue, CI, and the ingest path all want a long-lived credential with no browser in the loop, and static `api_tokens` remain the right thing for them. The two paths meet at `Identity` and diverge nowhere else. This also means the migration is additive: nothing that works today stops working.

### B. webauth as the authorization server

This is the larger half, and it lands in another repo and another org. It should be its own PR series against `infodancer/webauth`, not smuggled into a memstore branch.

**B1. Resource indicators (RFC 8707).** Accept `resource` at `/authorize`, carry it through the authorization code record (`internal/authcode`), and honour it at `/token`.

**B2. Audience claim.** Stamp `aud` on the access token from the granted resource. This changes `token.IssueToken`'s signature and every caller.

**B3. Scope plumbing.** Request, grant, and stamp `scope`. memstore needs `read`, `write`, and `admin`; the scope strings should be namespaced so they are meaningful across a multi-tenant AS serving several resources.

**B4. Discovery fields.** Add `code_challenge_methods_supported: ["S256"]`, `grant_types_supported`, `token_endpoint_auth_methods_supported`, and the memstore scopes to `scopes_supported`.

**B5. RFC 8414 metadata path.** Serve the insertion form alongside the existing OIDC form.

**B6. DCR is deprecated; CIMD is the replacement.** 2026-07-28 deprecates Dynamic Client Registration in favour of Client ID Metadata Documents, where the client's `client_id` is an HTTPS URL serving its own metadata. webauth has DCR (`internal/api/client_registration.go`, gated per tenant by `ClientRegistrationEnabled`) and no CIMD. DCR remains functional through the deprecation window, so this is not blocking, but it should be scoped before the window closes rather than after.

### C. Deployment

The tenant memstore uses must have client registration enabled, or its clients must be pre-registered. Note that homelab's `webauth/config.toml.j2` templates only `registration_enabled` -- **user** self-registration -- and never templates `client_registration_enabled`, which is the DCR flag and a different setting. The `infodancer` tenant sets neither. Whichever tenant is chosen needs that flag added to the ansible template and to the tenant entry in `docker-web.yml`.

### D. Testing

An `httptest` end-to-end pass with a locally generated RSA key standing in for the tenant JWKS: a token with the right audience is accepted, one with another resource's audience is rejected, one with no audience is rejected, an expired one is rejected, one signed by an unknown key is rejected, and one carrying `ingest` in its scope claim does not receive the ingest scope. Plus a test asserting the 401 carries `WWW-Authenticate` with the derived metadata URL, and that the metadata document round-trips through the SDK's type.

## Decisions

1. **webauth is the authorization server; memstore is a pure resource server.** Not a relying party, and not its own AS.
2. **Spec-correct audience binding**, rather than treating a dedicated tenant's issuer as an implicit audience boundary. The cheaper option was considered and rejected on 2026-08-24: it works, but the boundary would be a deployment convention enforced by nothing in code, and it fails silently the first time that tenant is given a second application.
3. **Static bearer tokens survive.** OAuth is added alongside them, not in place of them.
4. **The OAuth path never grants ingest**, enforced memstore-side.
5. **memstore autoprovisions users and delegates the admission decision to the authorization server.** A validated token whose `sub` has no user row gets one created. Who may hold a memstore-audience token is webauth's decision to make and express, not a roster memstore maintains in parallel and has to keep in sync.

Decision 5 carries a precondition, and it is a sequencing constraint rather than a caveat. The delegation is only real once webauth can express the decision, which is B1 through B3: without a `resource` parameter and a `scope` grant there is no way for the AS to say "this user may use memstore" and no way for it to withhold that. Until then, autoprovisioning against that AS means every user the tenant authenticates gets a memstore namespace. So the code is written now and **the OAuth verifier is not enabled in production until B1-B3 have landed.** Building it in the other order produces open enrolment while looking like delegation.

## Open questions

**Scope naming is configuration, not a constant.** An authorization server serving several resources namespaces its scopes, and the namespace is that deployment's convention rather than a property of either program. `--oauth-scope-prefix` tells memstore what to expect; empty means bare `read`/`write`/`admin`, which suits a server serving only memstore. The prefix is applied in two places that must agree: the metadata document advertises prefixed names, because that is what a client should request, and the verifier strips the prefix on the way in. Stripping happens BEFORE ingest is filtered -- reverse the two and `memstore:ingest` walks past a filter looking for `ingest` and is then stripped into a granted one.

**Which tenant an operator points memstore at is deployment configuration** (`--oauth-issuer`), and it is an admission policy: because memstore autoprovisions, whoever that tenant authenticates is who gets a memstore account. No tenant is named in memstore's source. For the infodancer deployment the choice is the `infodancer` tenant, where an account there is meant to confer a memstore account; the reasoning and the alternatives are in webauth's `docs/oauth-resource-servers.md`.

**Whether the REST API moves too, or only MCP.** The scope above protects the MCP endpoint. The REST surface has its own callers with their own credentials, and there is no forcing reason to move them at the same time.

## Non-goals

- memstore implementing federation, a login UI, or an authorization server of its own.
- Replacing static tokens.
- CIMD, this pass. Scoped in B6, deferred until the DCR deprecation window is the reason to act.
