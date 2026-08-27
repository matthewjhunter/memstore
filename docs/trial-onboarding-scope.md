# Trying memstore without an identity provider -- scope

Status: **decided**, 2026-08-27. Follows from `mcp-oauth-scope.md`, which made memstore an OAuth 2.1 resource server and left the authorization server to webauth. This document answers the question that raises: how does someone try memstore when they have no authorization server, and are not going to stand one up to find out whether they want the product?

## The constraint

Under 2026-07-28, memstore validates access tokens; it does not mint them. Whoever mints them must run an authorization-code + PKCE flow that a client can reach from discovery, stamp an `aud` matching the MCP endpoint URL, and accept client registration (DCR today, CIMD later). None of that exists on a fresh box. So every option is one of three things: skip OAuth, bundle an authorization server, or borrow one.

## Options considered

**1. Static bearer token, OAuth silent.** The path that exists today, made ergonomic. On first start against an empty `api_tokens` table, `memstored` mints a bootstrap token and prints it to the log; `memstore setup --remote URL --token T` records it and registers the MCP server with a static `Authorization` header. With no `MEMSTORE_OAUTH_ISSUER` set, the 401 carries no `WWW-Authenticate` challenge and no protected-resource metadata is served, so a client never starts a flow it cannot finish. Security is that of every API-key-fronted MCP server, which matches the threat model of one person's box. Still requires Postgres plus an embedder, but that is the product's cost, not auth's.

**2. webauth as a compose sidecar.** A `--profile oauth` that adds webauth with one tenant, user and client registration on, memstore pointed at its issuer. Spec-correct, reuses existing code, no third party. Costs: one more container, an issuer URL that both the browser and memstored can resolve (loopback across containers is awkward), plain-HTTP tolerated by clients only on loopback, and a hard dependency on webauth B1-B3. Worth having as an opt-in profile once B1-B3 land; not the minimum-fuss path.

**3. An authorization server embedded in memstored.** The spec allows RS and AS co-located and a good part of the ecosystem does it. Rejected. It reverses decision 1 of the OAuth scope and adds exactly the surface we do not want two of: a login form, a credential store, session handling, a signing key, client registration hygiene. The bootstrap-code variant (`/authorize` asks for a code from the log instead of a password) reduces the auth step to possession of the log, which option 1 gives for free with no protocol machinery. If an embedded AS is ever needed, the move is to make webauth embeddable as a library, not to grow a second one here.

**4. Bring your own authorization server** (Keycloak, Authentik, Auth0, Entra, Okta). Already works on this branch: configure an API audience, a PKCE client, and the three scopes on the AS; set `MEMSTORE_OAUTH_ISSUER`, optionally `MEMSTORE_OAUTH_JWKS`, and `MEMSTORE_OAUTH_SCOPE_PREFIX`. This is the intended long-term path for corporate deployments, and it needs documentation with one worked example. It is not an answer to the trial question, because the premise of the question is that no AS exists. Consumer IdPs (Google, GitHub) are excluded: their access tokens are opaque or fixed-audience and will not carry an `aud` for an arbitrary resource server.

**5a. A public webauth tenant as a free AS for self-hosted trials.** Rejected. It removes only the AS from the user's setup and leaves Postgres, pgvector, the embedder, and memstored to stand up before anything is visible. It competes with option 1 and loses, while adding a login on our infrastructure to a box we do not run.

**5b. A hosted demo server.** A public `memstored` behind webauth. The trial user runs one `claude mcp add` with a URL, logs in through the browser, and gets an autoprovisioned namespace. Nothing to pull, nothing to configure. This is the only genuinely zero-setup path, and the only one that exercises the OAuth work end to end in front of a real user. The cost is hosting strangers' memories: retention, abuse, embedding spend, and the requirement that per-user isolation be real rather than assumed.

## Decisions

1. **Two entry points, for two different people.** The docker image is for someone who wants memstore on their own hardware; it uses option 1 and makes no OAuth promises. The hosted demo (5b) is for someone who wants to see what memstore does in five minutes. Option 4 is the production path for organisations and is documented, not demoed.
2. **The docker trial does not do OAuth.** Static bearer token, bootstrapped on first start, OAuth discovery silent unless an issuer is configured. Option 2 may ship later as an opt-in compose profile; it is not required for the trial to work.
3. **memstore does not embed an authorization server.** Reaffirms decision 1 of `mcp-oauth-scope.md`.
4. **5b is what the resource-server work is building toward.** The verifier, autoprovisioning, audience binding, and the ingest filter all earn their keep on the hosted demo, where an arbitrary member of the public holds a token.
5. **Trial policy on the hosted demo is enforced in code, not stated in a README.** Caps, TTL, and scope restrictions are properties of the deployment's configuration and the daemon's behaviour, not of the user's good faith.
6. **The two credential paths still meet at `Identity` and diverge nowhere else.** A user who starts on a static token and later stands up an AS changes configuration, not data. A user who starts on the hosted demo and moves to their own install uses export/import.

## Design notes

### Docker trial (option 1)

- **Bootstrap token.** On first start against a database with a default user and zero `api_tokens` rows, mint one token named `<default-user>@bootstrap`, print the plaintext to the log once, and never again. Existing `EnsureLegacyToken` handles the `MEMSTORE_API_KEY` case; this is the no-env-var case beside it. Open question: admin-scoped (convenient) or read+write with admin requiring `memstore admin` on the host (safer, and mirrors how the ingest token is kept out of band). Leaning read+write.
- **First-run default user.** A fresh Postgres currently stops with "run `memstore admin tier3-init --default-user <name>`". For a compose trial that is one more step; the image should accept `MEMSTORE_DEFAULT_USER` and run the equivalent at first start when the database is empty. Re-running is a no-op.
- **Setup consumes the token.** `memstore setup --remote URL --token T` writes the token into `config.toml` and registers the MCP server with `claude mcp add --transport http --header "Authorization: Bearer T"`. Setup should refuse to write a token into config when the daemon URL is non-loopback plain HTTP unless `MEMSTORE_INSECURE_PLAINTEXT` is set, matching the existing gate.
- **OAuth stays silent.** With no issuer configured, `ServeHTTP` does not attach `WWW-Authenticate: Bearer resource_metadata=...` on 401 and the protected-resource metadata handler is not mounted. Covered by `TestNoChallengeWhenOAuthIsNotConfigured` in `httpapi/protectedresource_test.go`.
- **Compose file.** Postgres with pgvector, memstored, and an Ollama container that pulls `nomic-embed-text` on first start. One `docker compose up`, then read the token from `docker compose logs memstored`. No compose file exists yet; the only deployment is homelab's, which routes every model call to external services.
- **Only embedding is bundled.** Embedding is mandatory and `nomic-embed-text` is small enough to run on CPU, so it ships in the compose. Generation (extraction, hints, curator) and rerank are optional in the daemon (`generator == nil` is handled) and need models that are multi-gigabyte or slow on CPU, so they are off by default with `MEMSTORE_GEN_URL` / `MEMSTORE_GEN_MODEL` / `RERANK_*` documented for pointing at an existing Ollama, LM Studio, or OpenAI-compatible endpoint. Provide an `EMBEDDING_BASE_URL` override so anyone already running Ollama on the host can skip the bundled one rather than run two against one GPU. The trial demonstrates store, search, and recall; extraction is the part that needs a real model anyway.
- **Postgres stays required.** `memstored` is Postgres-only: the token, session, OAuth-user, tier-3, extract-queue, and hint tables exist only in `pgstore`, and the SQLite backend serves the stdio local mode, which has no HTTP and no auth. A single-image trial would mean porting the multi-user schema to SQLite for a trial that is by definition single-user, and then maintaining every isolation query twice. Not doing that. The compose file provisions Postgres with a generated password, so the user runs it rather than sets it up, and anyone who will not run three containers is the person 5b exists for.

### Hosted demo (5b), in dependency order

1. **Per-user isolation, confirmed.** `MIGRATING.md` records that v0.4.0 enforces isolation on every read and write. `installation.md` still carries the v0.3.0 warning and needs updating. Before a public deployment, `httpapi/isolation_test.go` should be reviewed against every surface added since v0.4.0 (documents, hints, feedback, links, tasks), because a public server turns any gap into a disclosure.
2. **webauth B1-B3** (resource indicators, `aud`, scope plumbing), plus B4-B5 discovery, tracked in `mcp-oauth-scope.md`. Without them there is no memstore token to validate and decision 5 of that document (delegated admission) is open enrolment.
3. **Trial policy, enforced.** Per-user caps on fact count and total bytes; a stated TTL after which the namespace is deleted, with a warning at the last login; scopes limited to `read` and `write` (no `admin`, no `ingest`, no document corpus); rate limits on writes, since every stored fact costs an embedding call; a size cap on individual facts. Each of these is a daemon feature with a config knob, not demo-only code.
4. **Abuse posture.** Screening already exists (`MEMSTORE_SCREEN_*`); turn detection on for writes. Content is private to its owner and never rendered to anyone else, which limits abuse to storage and compute rather than distribution. Log the `sub` of every write for takedown handling; store nothing else about the user beyond what the token carries.
5. **Exit path.** `memstore export` already exists for SQLite; the daemon needs an authenticated export of the caller's own namespace over HTTP so a trial user can take their data to a docker install. Import on the other side re-keys rows to the local user, which is the re-keying step decision A4 (key on `sub`, not email) makes necessary.
6. **Deployment.** A dedicated webauth tenant for the demo with user self-registration on, minting tokens only for the demo server's resource URL; homelab templating for `client_registration_enabled` (noted in section C of the OAuth scope); TLS; a public URL that is also `MEMSTORE_PUBLIC_URL`.

### Corporate path (option 4)

Documentation only: one worked example against Keycloak (realm, client with PKCE and no secret, audience mapper, the three scopes with a prefix), the equivalent settings named for Authentik and Auth0, and the memstore side (`MEMSTORE_OAUTH_ISSUER`, `MEMSTORE_OAUTH_JWKS`, `MEMSTORE_OAUTH_SCOPE_PREFIX`, `MEMSTORE_PUBLIC_URL`). A conformance test against a locally generated key already covers the verifier; the doc should say which claims the AS must emit.

## Todo

Ordered roughly by when each unblocks the next. Items marked (webauth) or (homelab) land in those repos.

Docker trial:

- [x] Bootstrap token: resolved by using the existing `MEMSTORE_API_KEY` legacy import rather than a printed one-time token. The compose sets it from `.env`; it is admin-scoped, stated in the example's caveats. A printed token would be a second mechanism for the same job.
- [x] `MEMSTORE_DEFAULT_USER` records the owner at first start on an empty database (`--default-user`).
- [x] `memstore setup --token` writes the key into a config it creates; refuses plaintext non-loopback URLs without `MEMSTORE_INSECURE_PLAINTEXT`. Registration already used the headers helper.
- [x] Test: no issuer configured means no `WWW-Authenticate` challenge and no metadata route (`TestNoChallengeWhenOAuthIsNotConfigured`).
- [x] `examples/docker-compose/` (Postgres+pgvector, embedding-only Ollama, memstored), labelled example only; pointer from `installation.md`.
- [x] Fix the stale v0.3.0 isolation warning in `installation.md`.

Hosted demo:

- [ ] Isolation review of every surface added since v0.4.0 against `isolation_test.go`.
- [ ] (webauth) B1-B5 from `mcp-oauth-scope.md`.
- [ ] Per-user caps: fact count, total bytes, fact size, write rate. Config knobs, enforced in the daemon.
- [ ] Namespace TTL with deletion and a last-login warning.
- [ ] Demo scope policy: read+write only; confirm the ingest filter and add an admin filter behind a config flag.
- [ ] Authenticated own-namespace export over HTTP; import re-keys to the local user.
- [ ] Write-path screening on by default for the demo deployment.
- [ ] (webauth, homelab) Demo tenant: self-registration on, client registration on, resource restricted to the demo URL.
- [ ] (homelab) Deploy `memstored` publicly behind TLS with `MEMSTORE_PUBLIC_URL` set.
- [ ] Demo terms: retention, caps, and the export path stated at first login.

Corporate path:

- [ ] `docs/oauth-bring-your-own-as.md` with a Keycloak walkthrough and the claim requirements.

Later, optional:

- [ ] Compose `--profile oauth` with webauth as a sidecar, after B1-B3.

## Non-goals

- An authorization server inside memstore, in any form.
- A hosted demo before isolation is reviewed and trial caps are enforced.
- Consumer IdP login (Google, GitHub) as a token source.
- Replacing static tokens.
