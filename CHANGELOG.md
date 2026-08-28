# Changelog

All notable changes to memstore are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0,
breaking changes can land in minor releases (and have).

## [Unreleased]

Nothing yet.

## [0.4.0] - 2026-08-27

The first tag since v0.2.0. It carries everything written up under the never-tagged 0.3.0 heading below (the Postgres daemon, TLS, bearer tokens, the search pipeline, session capture) plus the work since: per-user isolation, MCP over HTTP under the 2026-07-28 protocol revision, an OAuth resource-server role, and a first-start path that lets a container come up cold.

On multi-user: v0.3.0's notes promised that "the next release" would enforce per-user isolation. This release does -- every read and write is filtered by the token's user -- but it is not yet a multi-tenant product. There is no self-service enrolment, the OAuth path is inert until an authorization server that mints audience-bound tokens exists, and per-user quotas do not exist. Holding the tag until all of that landed was making "next release" mean nothing. What is here is the substrate; the rest is scoped in `docs/trial-onboarding-scope.md` and `docs/mcp-oauth-scope.md`.

### Added -- per-user isolation, enforced

Every fact, link, session, hint, and feedback row carries a `user_id`, and every read and write is scoped to the user the bearer token belongs to. Two tokens for two users never see each other's data. Existing single-user deployments migrate automatically when their token names share a prefix; see `docs/MIGRATING.md` for the case where they do not.

### Added -- MCP over HTTP

`memstored` serves MCP at `POST /memstore/mcp`, stateless, under the 2026-07-28 revision (`server/discover`, per-request `_meta`, `ttlMs`/`cacheScope`); older clients negotiate down to 2025-11-25. The per-session stdio binary is no longer how Claude Code, Cursor, or Zed reach memstore: `memstore setup` registers the HTTP endpoint, with the token read from `config.toml` at connect time through a headers helper rather than copied into the client's config. A read-scoped token is never shown a write tool; authorization is enforced at tool registration from the caller's identity, per request. The Stop hook runs through `memstore hook` in the CLI.

### Added -- OAuth 2.1 resource server

With `MEMSTORE_OAUTH_ISSUER` and `MEMSTORE_PUBLIC_URL` set, the daemon serves protected-resource metadata at `/.well-known/oauth-protected-resource/memstore/mcp`, challenges on 401 with `WWW-Authenticate`, and verifies access tokens (issuer, audience against the MCP endpoint, signature via cached JWKS) through `oidclient`. Users are autoprovisioned keyed on `sub`. Scopes come from the token under a configurable prefix (`MEMSTORE_OAUTH_SCOPE_PREFIX`); `ingest` is never granted this way. Unconfigured, none of this is served and the daemon behaves as before. It is not a relying party and never runs a login flow of its own. Not enabled in any deployment yet: it waits on the authorization server side (`docs/mcp-oauth-scope.md`, section B).

### Added -- cold start

- `MEMSTORE_DEFAULT_USER` (`--default-user`) records the owner of an empty database on first start, which is what `memstore admin tier3-init` did from a CLI the container image does not ship. A no-op on every later start.
- `memstore setup --token` writes the key into a `config.toml` it creates, never into an existing one, and refuses a plaintext non-loopback daemon URL unless `MEMSTORE_INSECURE_PLAINTEXT` affirms the network.
- `examples/docker-compose/`: Postgres with pgvector, an Ollama serving only the embedding model, and memstored on loopback with a static token. Example only; its README says what it leaves out.

### Changed -- the daemon lives under `/memstore/`

The API and MCP endpoint are mounted under a prefix, with `/.well-known/` reserved for the host; the root no longer answers. Existing clients need `memstore setup --force`, which rewrites `config.toml`'s `remote` and re-registers the MCP server. Plaintext now requires `MEMSTORE_INSECURE_PLAINTEXT` alongside `MEMSTORE_TLS_DISABLED`; the daemon refuses to start on the first flag alone. Rerank tunables are daemon configuration with per-call overrides on `memory_search` and `memory_get_context`, and the MCP server reports its version as 0.4.0 rather than 0.1.0.

### Deprecated -- `memstore-mcp` and the SQLite backend

The stdio binary and local SQLite mode still work in this release, unchanged. The next minor moves the test suite to Postgres and makes the deprecation visible (`setup` stops registering the stdio binary for new installs, the binary warns at startup); the one after removes both. Anyone with a pre-daemon SQLite file should `memstore export --db <file>` and import into a daemon before upgrading past 0.4.x; the export reader survives one release beyond the backend. Reasoning in `docs/mcp-http-transport-scope.md`, phase 7.

### Added -- prompt-injection defense

The screening half is deliberately unfinished: it
waits on provenance metadata, without which enforcement cannot tell a stored
user preference from an injection (see below).

- **Content fencing (active).** Everything memstore renders back to a model --
  the MCP read tools and the `/v1/recall` block the SessionStart hook injects --
  now encloses stored content in a per-response nonce fence, with a preamble
  naming it. Previously a fact whose text read like a section header or an
  instruction arrived with the same authority as memstore's own output, in every
  session in every repo. Metadata is split by value shape rather than key name,
  so field-name flexibility is preserved: short single-line scalars stay inline,
  anything longer or structured goes inside the fence.

- **Write screening (regex active, model opt-in).** Every write passes an inline
  regex screen -- nothing enters the store unscreened -- and is rejected with
  `ErrScreenRejected` above `screen_detect_score` (default 80). `UpdateMetadata`
  is screened too, closing the "store benign content, then patch a payload into
  metadata" bypass.

- **`screen_mode`: `off` (default) | `observe` | `gate`.** `observe` records the
  model's verdict on live writes while gating nothing -- facts stay readable and
  nothing is blocked. `gate` holds a write unreadable until the model clears it
  and blocks at `screen_threat`.

  **`gate` mode adds write-to-read latency**: a new fact is invisible from the
  moment it is written until the worker screens it, roughly one tick plus one
  model call (~30-60s at default settings). Harmless for a store read minutes or
  sessions later; it will break anything that writes a fact and reads it back
  immediately, tests especially.

- **`memstore scan`** reports what enforcement would do to an existing corpus
  without changing anything. Read-only: with `--pg` it bypasses the store
  entirely so it neither migrates the schema nor hides the pending and blocked
  facts a calibration pass most needs to see.

### Known limitation

The model screen's test is "is this text addressed to an AI", which is also a
fair description of memstore's most valuable content: stored preferences and
conventions that direct assistant behavior. Measured on a real 3858-fact corpus,
a 200-fact sample flagged two facts at `threat>=6`, one of them a legitimate
user preference phrased as instructions. Text alone cannot separate the two --
provenance can, and provenance metadata is the work `gate` mode waits on.

Until then: run `observe`, and prefer storing preferences informationally
("Matthew wants honest evaluation") over imperatively ("give the real answer").

### Added -- retrieval-only consumers

- **`memstore-mcp --read-only`.** Registers only the retrieval tools; the twelve
  store-mutating tools are not advertised, so they never appear in `tools/list`
  and cannot be called. Intended for a chatbot doing RAG over the corpus, where
  a model that can see `memory_store` will keep trying to use it.

- **`GET /v1/whoami`.** Reports what the calling credential may do. Returns the
  *effective* set computed by `Identity.Allows`, not the raw scopes on the
  token, so the implication rules (admin implies read+write, an empty set means
  read+write, ingest is implied by nothing) keep exactly one implementation and
  no client can drift from them. Authenticated but unscoped: asking what you may
  do is not a privileged act, and requiring `read` would leave an ingest-only
  token unable to discover its own capabilities.

- **The MCP tool list is derived from the token.** In daemon mode `memstore-mcp`
  queries `/v1/whoami` at startup, before the tool list and the server
  instructions are built, and drops the write tools when the credential cannot
  write. What the model sees now matches what the daemon will permit, instead of
  advertising writes that return 403. `--read-only` remains a floor: it can
  tighten the result, never loosen it.

  A daemon predating the endpoint returns 404, and the query is bounded at five
  seconds. Either way the configured value stands and the reason is logged --
  an error is deliberately **not** read as "no permissions", because a blip or
  an old daemon must not silently strip capability from a session that has it.

  In read-only mode the server instructions say the session is retrieval-only,
  so a model is not left hunting for a storage tool that was never registered.

### Added -- recall injections are counted

- **`inject_count` / `last_injected_at`**, bumped by `handleRecall` for every
  fact it surfaces. Previously `use_count` was incremented in exactly one place
  -- an explicit `memory_search` -- so the highest-volume read path in the
  system left no trace and a fact injected into hundreds of prompts read as
  never used. On the live corpus that was 3,538 of 4,657 facts (76%) sitting at
  zero.

  Kept as its own pair rather than folded into `use_count`, because the two are
  different evidence: a fact the model went looking for is a stronger signal
  than one the daemon offered unprompted, and #157's prune predicate has to tell
  them apart.

  The recording lives in the daemon, not in a hook or a model-initiated tool
  call. Both of those have been tried and both went silent --
  `context_injections` stopped 2026-05-27, `context_feedback` 2026-06-07, and
  `confirmed_count` is zero across the entire corpus. A signal the daemon
  derives from its own work cannot drift out of use.

  `memory_get_context` now calls `Touch` as well; it is a genuine retrieval and
  was the other read path recording nothing.

  Migration `V17` (SQLite) / `V10` (Postgres). The Postgres migration seeds the
  new counter from the historical `context_injections` rows, guarded on that
  table existing since it belongs to the session store and is absent from a
  fresh facts-only database. On the live corpus that backfill seeds 402 active
  facts, 96 of which a 90-day prune would otherwise have deleted despite recall
  having injected them repeatedly -- one of them 57 times.

### Changed -- `memstore search` defaults to the best available arm

- **`--hybrid` is replaced by `--search auto|hybrid|fts`, defaulting to `auto`.**
  The old default was keyword-only, so anyone who did not know to pass
  `--hybrid` was searching a semantically-indexed corpus by keyword and quietly
  missing facts that FTS cannot reach.

  `auto` uses hybrid wherever it is available -- always against a daemon, where
  the vector arm runs server-side and costs the client nothing -- and falls back
  to `fts` locally when no embedder can be built, printing the reason to stderr.
  `hybrid` forces it and fails if unavailable; `fts` forces keyword-only.

  The asymmetry is deliberate: `auto` is a preference and degrades with a
  warning, `hybrid` is an instruction and errors, because quietly returning
  keyword-only results to someone who asked for semantic search makes a thin
  result set look like a thin corpus. The same rule now governs a runtime
  failure mid-search, which previously fell back to FTS regardless of what was
  asked for.

  `--hybrid` is removed rather than aliased. Scripts passing it fail loudly;
  `--search hybrid` is the replacement, though against a daemon the default
  already does the right thing.

  Also stops building a local embedder in daemon mode, where it was constructed
  and then discarded.

### Changed -- least privilege at token issuance

- **`memstore admin issue-token` now defaults to `--scopes read`.** Issuing a
  token is routine and the common case only retrieves, so write must be asked
  for: pass `--scopes read,write` for a token that stores. The receipt always
  prints the granted scopes now, and says when they came from the default.

  This does **not** change what an empty scope set means. Tokens minted before
  scope enforcement carry one, and `Identity.Allows` still reads it as
  read+write; tightening that would revoke access from running deployments.
  Newly issued tokens simply never carry an empty set.

  **Upgrade note:** any automation that issues a token without `--scopes` and
  then writes will start getting 403s. Add `--scopes read,write`. Existing
  tokens are unaffected, including through `rotate-token`, which preserves the
  scopes already on the token.

## [0.3.0] - never tagged

Written in May 2026 and never released on its own; everything here shipped for the first time in 0.4.0. Kept as written because the migration notes in `docs/MIGRATING.md` refer to it by number.

The platform release. Memstore was a CLI + SQLite library in v0.2.0;
v0.3.0 is a client-server system with a Postgres backend, TLS, bearer-token
auth, a three-stage search pipeline (FTS + vector + cross-encoder rerank),
a session-capture pipeline, and a substantially larger MCP tool surface.

See [`docs/MIGRATING.md`](docs/MIGRATING.md) for upgrade instructions.

### Added

- **`memstored` daemon.** HTTP API for facts, search, recall, session
  transcripts, hint generation, learn (deprecated, see Removed). Container
  image published to GHCR on every push to main. Unauthenticated
  `/v1/health` for liveness probes.
- **Postgres backend.** `pgstore` package with pgvector for embeddings,
  tsvector for FTS, async embed queue, and incremental schema migrations.
- **Cross-encoder reranking.** Optional third stage on the search pipeline,
  using llama.cpp's `/v1/rerank` (Cohere/Jina-compatible wire shape). Four
  fusion modes (`off`, `balanced`, `dominant`, `gate`) plus a relevance
  threshold. Per-path document truncation budgets keep recall latency
  inside the per-prompt hook timeout.
- **Runtime-tunable retrieval.** `memory_rerank_settings` MCP tool lets the
  model adjust mode, threshold, weight, candidate pool sizes, doc-byte
  budgets, and timeout per session.
- **Per-prompt context injection.** `/v1/recall` endpoint plus a
  `UserPromptSubmit` hook (`memstore-prompt.mjs`) that surfaces relevant
  facts on every user message in Claude Code.
- **Session capture pipeline.** `session_turns`, `context_hints`,
  `context_injections`, `context_feedback` tables. Hooks record turns and
  injections; a Stop hook ingests the transcript and runs ExtractQueue
  Stage 2 (local Ollama) to generate hints for future sessions.
- **Self-improving recall.** Injected facts are auto-rated at session end;
  the per-fact aggregate feeds back as a multiplier on recall ranking.
  `backfill-feedback` retroactively rates historical sessions on daemon
  startup.
- **Native TLS for the daemon.** TLS 1.2+ with optional mTLS. `memstore
  tls init-ca / issue-server / issue-client` subcommands wrap a stdlib-only
  CA package (`internal/caetl`) for self-signed bootstrap.
- **Bearer-token authentication.** `api_tokens` table; tokens stored as
  SHA-256 hashes, plaintext shown once at issue. Constant-time comparison
  on verify. `memstore admin` subcommands for issue/list/revoke/rotate.
- **Identity request context.** `httpapi.Identity` struct populated by
  bearer middleware. Carried through request context, but not yet
  enforced at the store layer (see Unreleased / v0.4.0).
- **`memstore setup`** for automated installation: hooks, MCP
  registration, config file. Per-harness install tasks for Claude Code,
  Cursor, Zed, and an experimental Codex notify shim.
- **New MCP tools:**
  - `memory_store_batch`: store up to 20 facts in one call
  - `memory_get_context`: composite task-driven loading
  - `memory_curate_context`: filter candidate set via a curator model
  - `memory_suggest_agent`: agent routing by stored conventions
  - `memory_rate_context`: rate injected context usefulness
  - `memory_rerank_settings`: get/set runtime retrieval tunables
  - `memory_link`, `memory_unlink`, `memory_get_links`,
    `memory_update_link`: explicit graph operations
  - `memory_list_subsystems`: distinct subsystems for discovery
  - `memory_task_create`, `memory_task_update`, `memory_task_list`:
    cross-session task tracking with scoped ownership
  - `memory_history`: walk supersession chains
  - `memory_confirm`: increment a fact's verified-accuracy counter
  - `memory_status`: active-fact counts by subject and category
- **Schema migrations.**
  - V7: explicit graph links table (`memstore_links`)
  - V8: `kind` and `subsystem` as first-class columns
  - V9: term-frequency materialization for IDF scoring
- **AllNamespaces search.** Cross-namespace retrieval for the daemon's
  internal queries.
- **Eval-triggers CLI** and `cwd_pattern` trigger type for
  directory-aware context injection.
- **`--no-embeddings` flag** on `memstore-mcp` for FTS-only operation
  when an embedder isn't configured.

### Changed

- **Daemon is Postgres-only.** `memstored` no longer supports SQLite. The
  CLI binary still uses SQLite for local-only operation. See
  [`docs/MIGRATING.md`](docs/MIGRATING.md).
- **Embedder migrated to standalone module.** Embedding code lives in
  [`go-embedding`](https://github.com/matthewjhunter/go-embedding) (v0.4.6
  at time of release). `ollama.go` removed; `openai.go` added; both are
  superseded by the external module.
- **Embedder is env-driven.** `MEMSTORE_EMBED_BACKEND`,
  `MEMSTORE_EMBED_BASE_URL`, `MEMSTORE_EMBED_MODEL` (with `EMBEDDING_*`
  fallbacks). `AppConfig.Model` removed.
- **Generator endpoint separable from embedder.** New `MEMSTORE_GEN_URL`
  and `MEMSTORE_GEN_MODEL` for chat/extraction endpoints distinct from
  embeddings.
- **Persona is per-request.** Defaults to the OS username; no longer
  required in daemon config.
- **Fact content capped at 8000 chars at the DB layer** (V2 migration).
  Application-layer validation existed; the schema check is the wall
  behind the wall after a 50 KB fact poisoned the embed queue.
- **Embed queue is per-fact, not batched.** A batched call lets one
  poisoned input stall the entire queue. Per-fact serializes failures.
- **Recall ranking:** confidence-weighted feedback, project-surface
  boost, IDF thresholding, low-relevance cutoff (relative to top result),
  symbol-fact demotion, cross-project summary filtering.
- **Session summaries** structured as a JSON envelope with explicit scope
  and strict `json_schema` enforcement.
- **Summary corpus cap lifted** from 32 KB to 120 KB.
- **PostCompact hook** logs every invocation; gates `/exit` on
  uncompacted long sessions (later removed; see Removed).
- **Hooks** moved into `cmd/memstore/hooks/` with install-time
  placeholders; installed by `memstore setup`.
- **Recall hook timeout** raised from 3s to 4.5s to accommodate
  cross-encoder rerank latency.
- **Dependencies:** MCP Go SDK upgraded through 1.4.0/1.4.1/1.5.0/1.6.0;
  pgx/v5 5.9.x; modernc.org/sqlite 1.50.0; pgvector-go updated.
- **Go toolchain** bumped to 1.25.10 across releases for stdlib CVE
  fixes.

### Deprecated

- `metadata.related_facts` JSON convention. Use explicit links via
  `memory_link` / `memory_get_links` instead. Old writes are not
  migrated automatically; query paths ignore the JSON field.

### Removed

- **`memory_learn` tool** and the entire codebase-ingestion subsystem.
- **`memory_check_drift` tool** and the drift-detection surface
  (`source_files` metadata convention, `GitRunner`, `Config.RepoPaths`,
  inline drift warning in `memory_get_context`).
- **SQLite mode in `memstored`.** The daemon is Postgres-only. SQLite
  remains available for the local CLI binary.
- **`AppConfig.Model`** field. Replaced by env-driven embedder config.
- **Post-session hook orchestration.** The compact-before-exit gate and
  related machinery were proven unnecessary and removed.
- **Embed-call batching in the queue.** Per-fact only.

### Fixed

- Session transcript upload race resolved via per-session state files.
- Nil embedder panic in daemon mode.
- Cross-encoder rerank silently degrading on oversized documents (the
  ubatch-overflow bug; see #67/#68).
- pgstore V3 quarantine columns prevent permanently-failing facts from
  consuming embed-queue cycles forever.
- Session store migration catches `duplicate_table` (42P07) alongside
  `duplicate_object`.
- Setup `--force` no longer clobbers user config.
- Hooks registered to `settings.json` (not `settings.local.json`) so
  they version with the project.

### Security

- Tokens stored as SHA-256 hashes; plaintext shown once at issue.
- Constant-time comparison on token verification
  (`crypto/subtle.ConstantTimeCompare`).
- Native TLS 1.2+ (1.3 recommended) on the daemon; optional mTLS.
- Stdlib-only CA in `internal/caetl` for self-signed bootstrap.
- MCP Go SDK 1.4.1 fixes GO-2026-4773 and GO-2026-4770.
- Go toolchain bumped to 1.25.7/1.25.8/1.25.9/1.25.10 across the cycle
  for stdlib CVE fixes (GO-2026-4601, GO-2026-4602, others).

### Notice

**Memstore is single-user by deployment, not by enforcement.** The
`Identity` plumbing exists end-to-end on the request path, but no read
or write path filters by user yet. Two clients with two different tokens
see the same facts. v0.4.0 is the milestone where the enforcement
catches up to the architecture. Until then, don't deploy memstore as a
shared multi-user service. *(Resolved in 0.4.0, above.)*

## [0.2.0] - 2026-02-18

The polished portfolio release. Test coverage reporting, CI pipeline
template, README rewrite, hook examples for Claude Code session-start
integration.

### Added

- Test coverage reporting via Codecov.
- CI pipeline + template infrastructure; Go toolchain bumped to 1.25.
- README rewritten as a portfolio piece.
- Architecture documentation under `docs/`.
- `memstore` CLI with `tasks`, `store`, `list` subcommands.
- `SearchFTS` interface method and `memstore search` subcommand.
- Claude Code hook examples for session-start integration.
- Explicit graph links between facts (schema V7).
- `UserPromptSubmit` hook and slash commands for automatic retrieval.

## [0.1.0] - 2025

Initial public release. SQLite-backed fact store with hybrid FTS5 +
vector search, supersession chains, namespace isolation, MCP server with
the original CRUD tool set.
