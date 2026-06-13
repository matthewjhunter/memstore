# memstore

[![CI](https://github.com/matthewjhunter/memstore/actions/workflows/ci.yml/badge.svg)](https://github.com/matthewjhunter/memstore/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/matthewjhunter/memstore)](https://goreportcard.com/report/github.com/matthewjhunter/memstore)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod-go-version/matthewjhunter/memstore)](go.mod)
[![codecov](https://codecov.io/gh/matthewjhunter/memstore/graph/badge.svg)](https://codecov.io/gh/matthewjhunter/memstore)

Persistent memory system for AI agents with hybrid semantic search,
cross-encoder reranking, fact supersession, and cross-session task tracking.

> **Multi-user isolation (v0.4.0).** Memstore enforces per-user isolation
> end to end. Every read and write -- facts, links, sessions, hints, and
> feedback -- is filtered by the user the bearer token belongs to; two
> tokens for two users never see each other's data. A fresh PostgreSQL
> deployment runs `memstore admin tier3-init --default-user <name>` once to
> seed identity; an existing single-user deployment upgrades automatically
> (the default user is inferred from your token names). See
> [`docs/MIGRATING.md`](docs/MIGRATING.md) for the upgrade path.

## Quick Start

```bash
# Install binaries
go install github.com/matthewjhunter/memstore/cmd/memstore@latest
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest

# Pull an embedding model. Any Ollama or OpenAI-compatible embedding model
# works (nomic-embed-text, embeddinggemma, mxbai-embed-large, etc.). The
# chosen model and its vector dimension are locked on first use, so pick
# one and stick with it. See "Installation" for the env vars.
ollama pull nomic-embed-text

# Set up everything: hooks, MCP registration, config
memstore setup
```

`memstore setup` detects your environment, installs Claude Code hooks,
registers the MCP server, and creates a config file. Run it again after
updating to deploy the latest hooks. See
[docs/installation.md](docs/installation.md) for manual setup and
troubleshooting.

**Other harnesses.** `memstore-mcp` is a standard MCP stdio server and works
in any host that speaks MCP. The Taskfile has per-harness installers:

```bash
task install:claude     # Claude Code: hooks + MCP (calls memstore setup)
task install:cursor     # Cursor: writes ~/.cursor/mcp.json
task install:zed        # Zed: prints settings.json snippet (manual paste)
task install:codex      # Codex: installs experimental notify shim + prints config.toml snippet
task install:harnesses  # run all of the above
```

Only Claude Code has a full hook lifecycle; the others get tools-only or an
experimental adapter (Codex). See
[`examples/codex/README.md`](examples/codex/README.md) for the Codex
caveats.

## What It Does

Memstore gives AI agents durable memory that persists across sessions. It
stores facts with automatic vector embeddings, enabling hybrid full-text
and semantic search with an optional cross-encoder rerank pass. Facts form
supersession chains -- when knowledge evolves, the old version is preserved
in history while the new version takes precedence. An integrated task
system with startup surfacing ensures pending work survives session
boundaries.

Two deployment shapes: a **local-only** mode where the CLI library uses
SQLite directly, and a **daemon mode** where `memstored` exposes the store
over HTTPS for multiple clients (typically one human's machines).

## Key Features

- Three-stage search: BM25 full-text, cosine-similarity vector search, and
  optional cross-encoder reranking with four fusion modes
- Runtime-tunable retrieval via `memory_rerank_settings` -- the model can
  adjust mode, threshold, candidate pool sizes, and doc-byte budgets per
  session
- Fact supersession chains with full history preservation
- Automatic embedding via
  [go-embedding](https://github.com/matthewjhunter/go-embedding) (model +
  dimension locked on first use; quarantines permanent embed failures)
- Per-prompt context injection via the `UserPromptSubmit` hook +
  `/v1/recall` endpoint
- Session capture pipeline (turns, hooks, hints, injections, feedback) that
  feeds back into recall ranking
- Auto-rating of injected facts at session end -- self-improving recall
- Temporal decay with per-category half-life tuning
- Task tracking with scoped ownership (user / agent / collaborative)
- Startup surfacing for pending tasks across sessions
- Namespace isolation for multi-instance deployments
- Metadata filtering with typed operators (`=`, `!=`, `<`, `<=`, `>`, `>=`)
- LLM-based fact extraction with auto-deduplication and auto-supersession
- Fact linking with directed graph edges and neighbor traversal
- Context curation and injection feedback
- Native TLS with optional mTLS for the daemon; bearer-token auth with
  hashed storage and constant-time comparison
- Export/import for backup and migration (embeddings excluded; regenerated
  on import)

## Architecture

```
Claude Code
  ├── Hooks (7 scripts)              ← context injection + event capture
  │     ├── SessionStart             ← inject tasks + project facts
  │     ├── UserPromptSubmit         ← recall facts on every prompt
  │     ├── PreToolUse (Read/Edit)   ← inject file/symbol constraints
  │     ├── PostToolUse (Write/Bash) ← nudge to store decisions
  │     ├── Stop                     ← session tracking + transcript upload
  │     └── SessionEnd               ← record activity + task reminders
  │
  └── MCP Server (23 tools)          ← stdio
        │
        ├─ Local mode ──→ SQLite + embedder endpoint
        │                   ├─ Facts table
        │                   ├─ FTS5 index
        │                   ├─ Vector embeddings (sqlite-vec)
        │                   └─ Schema migrations
        │
        └─ Daemon mode ──→ memstored (HTTPS API)
                            ├─ /v1/health, /v1/recall, /v1/search, ...
                            ├─ Postgres + pgvector + tsvector
                            ├─ Async embed queue
                            ├─ Session capture pipeline
                            └─ Optional cross-encoder rerank sidecar
```

The MCP server exposes the store as tools over stdio. Hooks run alongside
Claude Code to inject relevant context and capture session events. In
daemon mode, hooks and the MCP server both talk to `memstored` over HTTPS
with bearer-token auth.

The `Store` interface (`store.go`) separates the storage engine from the
MCP layer, so memstore can also be used as a Go library directly. See
[docs/architecture.md](docs/architecture.md) for a detailed breakdown.

## MCP Tools

| Tool | Description |
|------|-------------|
| `memory_store` | Store a fact with automatic embedding and optional supersession |
| `memory_store_batch` | Store multiple facts in a single call (max 20 per batch) |
| `memory_search` | Hybrid full-text + semantic search with ranked results |
| `memory_list` | Browse facts by subject and category without a query |
| `memory_delete` | Delete a fact by ID (prefer supersession for history preservation) |
| `memory_supersede` | Mark an existing fact as replaced by a newer one |
| `memory_history` | Show the supersession chain for a fact or all facts for a subject |
| `memory_confirm` | Increment a fact's confirmation count to signal verified accuracy |
| `memory_update` | Merge a metadata patch into a fact without replacing it |
| `memory_status` | Show active fact count with breakdown by subject and category |
| `memory_task_create` | Create a scoped task with priority, project, and due date |
| `memory_task_update` | Transition a task's status (pending / in_progress / completed / cancelled) |
| `memory_task_list` | List tasks filtered by scope, status, and project |
| `memory_link` | Create a directed graph edge between two facts |
| `memory_unlink` | Remove a link by ID |
| `memory_get_links` | Get all links touching a fact with neighbor summaries |
| `memory_update_link` | Update a link's label and metadata |
| `memory_list_subsystems` | List all distinct subsystem values in the store |
| `memory_get_context` | Load relevant context for a task (invariants, failure modes, search) |
| `memory_curate_context` | Filter candidate facts to the most relevant subset for a task |
| `memory_suggest_agent` | Recommend specialist agents based on stored domain mappings |
| `memory_rate_context` | Rate injected context usefulness (feeds injection ranking) |
| `memory_rerank_settings` | Get/set per-session retrieval tunables (mode, threshold, candidates, doc bytes) |

## Hooks

Hooks wire memstore into Claude Code's session lifecycle so context surfaces
automatically. They are embedded in the `memstore` binary and installed by
`memstore setup`.

| Hook | Event | Purpose |
|------|-------|---------|
| `memstore-startup.mjs` | SessionStart | Inject pending tasks and project facts |
| `memstore-prompt.mjs` | UserPromptSubmit | Recall relevant facts for each prompt |
| `memstore-read.mjs` | PreToolUse:Read | Inject file/symbol constraints before reads |
| `memstore-edit.mjs` | PreToolUse:Edit | Inject file/symbol constraints before edits |
| `store-nudge.mjs` | PostToolUse:Write/Bash | Nudge to store decisions after key actions |
| `stop-hook.mjs` | Stop | Track sessions, upload transcripts |
| `memstore-session-end.mjs` | SessionEnd | Record activity, remind about open tasks |

Hooks that communicate with `memstored` (prompt recall, context touch, stop
hook) silently no-op when the daemon is unavailable, so they are safe to
install in local-only mode.

## Daemon Mode

For lower-latency context injection, multi-machine access, and background
processing (session capture, hint generation, feedback rating), run
`memstored`:

```bash
go install github.com/matthewjhunter/memstore/cmd/memstored@latest

# Required: Postgres with pgvector and an embedder endpoint
export MEMSTORE_PG='postgres://memstore:secret@host:5432/memstore?sslmode=require'
export MEMSTORE_EMBED_BACKEND=ollama
export MEMSTORE_EMBED_BASE_URL=http://localhost:11434
export MEMSTORE_EMBED_MODEL=nomic-embed-text

# Optional: TLS + mTLS
memstore tls init-ca
memstore tls issue-server --host memstored.lan
memstored --tls-cert server.crt --tls-key server.key

# Optional: cross-encoder reranker (separate sidecar)
export MEMSTORE_RERANK_BASE_URL=http://reranker:8080
export MEMSTORE_RERANK_MODEL=bge-reranker-v2-m3
```

Tokens for clients are issued via `memstore admin issue --name <client>`.
Each plaintext token is shown once; the daemon stores SHA-256 hashes and
verifies with constant-time comparison.

The daemon exposes:

- `/v1/health` (unauthenticated liveness)
- `/v1/recall` (per-prompt context injection)
- `/v1/search`, `/v1/facts/*` (full Store interface)
- `/v1/context/hints` (proactive nudges queued by background extractors)
- `/v1/sessions/turns/*` (session capture pipeline)

`memstore setup` auto-detects a running daemon and configures hooks
accordingly. Without the daemon, memstore operates in local-only mode using
the CLI binary for hook operations.

Container image is published to
[GHCR](https://github.com/matthewjhunter/memstore/pkgs/container/memstored)
on every push to main.

## Installation

```bash
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest
go install github.com/matthewjhunter/memstore/cmd/memstore@latest
```

**Prerequisites:** an OpenAI-compatible or Ollama embedding endpoint.
Locally, [Ollama](https://ollama.ai) with an embedding model pulled is the
simplest:

```bash
ollama pull nomic-embed-text
```

Embedding configuration is read from environment variables via
[go-embedding](https://github.com/matthewjhunter/go-embedding) -- set
`MEMSTORE_EMBED_BACKEND`, `MEMSTORE_EMBED_BASE_URL`, and
`MEMSTORE_EMBED_MODEL` (or the shared `EMBEDDING_*` defaults) before
launching `memstore-mcp` or `memstored`. A separate generator endpoint can
be wired via `MEMSTORE_GEN_URL` and `MEMSTORE_GEN_MODEL`.

Then run `memstore setup` to configure everything, or see
[docs/installation.md](docs/installation.md) for manual setup.

For upgrading from a previous release, see
[`docs/MIGRATING.md`](docs/MIGRATING.md).

## Usage

An AI agent stores, searches, and evolves facts through the MCP tools:

```
# Store a fact
memory_store(
  content="Matthew prefers small, logical commits -- never bundle unrelated changes",
  subject="matthew",
  category="preference"
)
→ Stored (id=42, subject="matthew", category="preference").

# Search for related facts
memory_search(query="matthew commit style", limit=5)
→ [1] (id=42, score=0.847, rerank=0.915) matthew | preference
      Matthew prefers small, logical commits -- never bundle unrelated changes

# Correct the fact when knowledge changes
memory_store(
  content="Matthew prefers small commits and squashes fixup commits before merging",
  subject="matthew",
  category="preference",
  supersedes=42
)
→ Stored (id=43, ...). Superseded fact 42.

# Retrieve the full history
memory_history(id=43)
→ [1/2] (id=42) SUPERSEDED by 43 | 2026-01-10
      Matthew prefers small, logical commits -- never bundle unrelated changes
  [2/2] (id=43) ACTIVE | 2026-02-15
      Matthew prefers small commits and squashes fixup commits before merging
```

When rerank is enabled, results include a `rerank=N.NNN` score alongside
the first-stage `score`. The model can adjust the rerank policy with
`memory_rerank_settings`.

## Search

Search runs three stages: two parallel first-stage passes that get merged,
then an optional cross-encoder rerank.

1. **FTS full-text search** -- in SQLite, FTS5 with BM25; in Postgres,
   tsvector with `ts_rank_cd`. Each query word is individually
   double-quoted to prevent FTS syntax injection. Raw scores are
   normalized to `[0, 1]` per query (min-max) so they're comparable to
   cosine.

2. **Vector search** -- the query is embedded via the configured embedder
   (or fetched from the in-memory cache on the daemon for repeat
   prompts), then cosine similarity is computed against every stored
   embedding. In Postgres, an HNSW index over the `vector(N)` column
   bounds the latency cost.

3. **Score merging** -- facts appearing in both result sets are
   deduplicated. The combined first-stage score is:

   ```
   combined = (FTSWeight * fts_score) + (VecWeight * vec_score)
   ```

   Default weights: FTS 0.6, vector 0.4. Weights are configurable per
   query.

4. **Cross-encoder reranking** (optional) -- the top-K candidates from
   the first stage are sent to a reranker sidecar (llama.cpp with
   `--reranking`, Cohere/Jina wire shape). Raw logits → sigmoid →
   `[0, 1]`. Four fusion modes:

   - `off`: no rerank, first-stage order
   - `balanced`: weighted blend of rerank + first-stage (default 0.7)
   - `dominant`: rerank drives order, first-stage breaks ties only
   - `gate`: first-stage order, rerank only filters by threshold

   A relevance threshold drops candidates below the cutoff in any
   enabled mode. The rerank stage gracefully degrades to first-stage
   order if the sidecar is unreachable.

5. **Temporal decay** -- an optional exponential decay can be applied
   per category. The MCP server configures `note` facts with a 30-day
   half-life; `preference` and `identity` facts do not decay.

`SearchBatch` amortizes embedding cost across multiple queries by making
a single batched embedding call instead of one per query.

## Supersession

Facts form linked chains. When knowledge evolves, the old version is
preserved in history and the new version becomes active:

```
fact 42 (SUPERSEDED → 43)  "Matthew prefers small, logical commits"
        ↓
fact 43 (ACTIVE)            "Matthew prefers small commits and squashes fixup commits before merging"
```

**Explicit supersession** -- pass `supersedes=<id>` to `memory_store`, or
call `memory_supersede` after storing the replacement separately.

**Automatic supersession** -- the `FactExtractor` pipeline embeds each new
fact immediately after insert, then searches for same-subject active
facts. If cosine similarity reaches the 0.85 threshold, the closest match
is automatically superseded. A metadata conflict check prevents facts
from different contexts (different projects, sources, etc.) from
superseding each other across context boundaries.

`memory_history` walks the full chain in either direction -- useful for
auditing how a piece of knowledge has changed over time.

## Documentation

- [Changelog](CHANGELOG.md)
- [Migrating between versions](docs/MIGRATING.md)
- [Architecture overview](docs/architecture.md)
- [Installation guide](docs/installation.md)
- [Tier 1 graph basics](docs/tier1-graph-basics.md) (links + neighborhoods)
- [Tier 2 graph analytics](docs/tier2-graph-analytics.md) (components, summary triggers)
- [Tier 3 permissions design](docs/tier3-permissions.md) (multi-user roadmap)
- [Local LLM features menu](docs/local-llm-features.md)
- [Training data design](docs/training-data-design.md)

## License

MIT
