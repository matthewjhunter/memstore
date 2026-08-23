# Changelog

All notable changes to memstore are documented here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0,
breaking changes can land in minor releases (and have).

## [Unreleased]

### Added -- prompt-injection defense

Not part of a release yet. The screening half is deliberately unfinished: it
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

## [0.4.0] - unreleased

Work in progress for v0.4.0: per-user data isolation. See
[`docs/tier3-permissions.md`](docs/tier3-permissions.md). v0.3.0 ships
authentication plumbing only -- facts are not scoped per user yet. Two
tokens see the same facts. Don't deploy memstore as a multi-tenant
service until v0.4.0 lands.

## [0.3.0] - 2026-05-?? (unreleased)

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
shared multi-user service.

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
