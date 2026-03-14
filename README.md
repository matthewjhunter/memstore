# memstore

[![CI](https://github.com/matthewjhunter/memstore/actions/workflows/ci.yml/badge.svg)](https://github.com/matthewjhunter/memstore/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/matthewjhunter/memstore)](https://goreportcard.com/report/github.com/matthewjhunter/memstore)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod-go-version/matthewjhunter/memstore)](go.mod)
[![codecov](https://codecov.io/gh/matthewjhunter/memstore/graph/badge.svg)](https://codecov.io/gh/matthewjhunter/memstore)

Persistent memory system for AI agents with hybrid semantic search, fact supersession, and cross-session task tracking.

## Quick Start

```bash
# Install binaries
go install github.com/matthewjhunter/memstore/cmd/memstore@latest
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest

# Pull an embedding model
ollama pull embeddinggemma

# Set up everything: hooks, MCP registration, config
memstore setup
```

`memstore setup` detects your environment, installs Claude Code hooks, registers the MCP server, and creates a config file. Run it again after updating to deploy the latest hooks. See [docs/installation.md](docs/installation.md) for manual setup and troubleshooting.

## What It Does

Memstore gives AI agents durable memory that persists across sessions. It stores facts in SQLite with automatic vector embeddings, enabling hybrid full-text and semantic search. Facts form supersession chains — when knowledge evolves, the old version is preserved in history while the new version takes precedence. An integrated task system with startup surfacing ensures pending work survives session boundaries.

## Key Features

- Hybrid search combining BM25 full-text (FTS5) and cosine similarity vector search
- Fact supersession chains with full history preservation
- Automatic embedding via Ollama (configurable model, locked on first use)
- Temporal decay with per-category half-life tuning
- Task tracking with scoped ownership (user / agent / collaborative)
- Startup surfacing for pending tasks across sessions
- Namespace isolation for multi-tenant deployments
- Metadata filtering with typed operators (`=`, `!=`, `<`, `<=`, `>`, `>=`)
- LLM-based fact extraction with auto-deduplication and auto-supersession
- Fact linking with directed graph edges and neighbor traversal
- Context curation and injection ranking
- Export/import for backup and migration (embeddings excluded; re-generated on import)

## Architecture

```
Claude Code
  ├── Hooks (8 scripts)              ← context injection + event capture
  │     ├── SessionStart             ← inject tasks + project facts
  │     ├── UserPromptSubmit         ← recall relevant facts per prompt
  │     ├── PreToolUse (Read/Edit)   ← inject file/symbol constraints
  │     ├── PostToolUse (Write/Bash) ← nudge to store decisions
  │     ├── Stop                     ← session tracking + transcript upload
  │     └── SessionEnd               ← record activity + task reminders
  │
  └── MCP Server (26 tools)          ← stdio
        │
        ├─ Local mode ──→ SQLite + Ollama
        │                   ├─ Facts table
        │                   ├─ FTS5 index
        │                   ├─ Vector embeddings
        │                   └─ Schema migrations
        │
        └─ Daemon mode ──→ memstored (HTTP API)
                            ├─ /v1/recall (context injection)
                            ├─ /v1/context/hints (proactive nudges)
                            ├─ /v1/sessions/transcript (extraction pipeline)
                            └─ Same SQLite/Postgres backend
```

The MCP server exposes the store as tools over stdio. Hooks run alongside Claude Code to automatically inject relevant context and capture session events. In daemon mode, hooks communicate with `memstored` via HTTP for recall, hints, and transcript processing.

The `Store` interface (`store.go`) separates the storage engine from the MCP layer, so memstore can also be used as a Go library directly. See [docs/architecture.md](docs/architecture.md) for a detailed breakdown.

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
| `memory_list_project` | Load project-surface and package-surface facts for a directory |
| `memory_list_file` | Load file-surface and symbol-surface facts for a file |
| `memory_check_drift` | Check whether code-documenting facts are stale via git history |
| `memory_curate_context` | Filter candidate facts to the most relevant subset for a task |
| `memory_learn` | Ingest a Go codebase into structured facts with containment graph |
| `memory_suggest_agent` | Recommend specialist agents based on stored domain mappings |
| `memory_rate_context` | Rate injected context usefulness (feeds injection ranking) |

## Hooks

Hooks wire memstore into Claude Code's session lifecycle so context surfaces automatically. They are embedded in the `memstore` binary and installed by `memstore setup`.

| Hook | Event | Purpose |
|------|-------|---------|
| `memstore-startup.mjs` | SessionStart | Inject pending tasks and project facts |
| `memstore-prompt.mjs` | UserPromptSubmit | Recall relevant facts for each prompt |
| `memstore-read.mjs` | PreToolUse:Read | Inject file/symbol constraints before reads |
| `memstore-edit.mjs` | PreToolUse:Edit | Inject file/symbol constraints before edits |
| `store-nudge.mjs` | PostToolUse:Write/Bash | Nudge to store decisions after key actions |
| `stop-hook.mjs` | Stop | Track sessions, upload transcripts |
| `memstore-session-end.mjs` | SessionEnd | Record activity, remind about open tasks |

Hooks that communicate with `memstored` (prompt recall, context touch, stop hook) silently no-op when the daemon is unavailable, so they are safe to install in local-only mode.

## Daemon Mode

For lower-latency context injection and background processing, run `memstored`:

```bash
go install github.com/matthewjhunter/memstore/cmd/memstored@latest
memstored
```

The daemon provides HTTP APIs for recall, context hints, and session transcript processing. `memstore setup` auto-detects a running daemon and configures hooks accordingly. Without the daemon, memstore operates in local-only mode using the CLI binary for hook operations.

## Installation

```bash
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest
go install github.com/matthewjhunter/memstore/cmd/memstore@latest
```

**Prerequisites:** [Ollama](https://ollama.ai) running locally with an embedding model pulled:

```bash
ollama pull embeddinggemma
```

Then run `memstore setup` to configure everything, or see [docs/installation.md](docs/installation.md) for manual setup.

## Usage

An AI agent stores, searches, and evolves facts through the MCP tools:

```
# Store a fact
memory_store(
  content="Matthew prefers small, logical commits — never bundle unrelated changes",
  subject="matthew",
  category="preference"
)
→ Stored (id=42, subject="matthew", category="preference").

# Search for related facts
memory_search(query="matthew commit style", limit=5)
→ [1] (id=42, score=0.847) matthew | preference
      Matthew prefers small, logical commits — never bundle unrelated changes

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
      Matthew prefers small, logical commits — never bundle unrelated changes
  [2/2] (id=43) ACTIVE | 2026-02-15
      Matthew prefers small commits and squashes fixup commits before merging
```

## Search

Search runs two passes in parallel, then merges the results:

1. **FTS5 full-text search** — each query word is individually double-quoted (preventing injection of FTS5 syntax) and matched with BM25 ranking. Raw BM25 scores are negative; memstore negates and normalizes them to [0, 1].

2. **Vector search** — the query is embedded via Ollama, then cosine similarity is computed against every stored embedding. Only positive-similarity results are kept, sorted descending.

3. **Score merging** — facts appearing in both result sets are deduplicated. The combined score is:

   ```
   combined = (FTSWeight * fts_score) + (VecWeight * vec_score)
   ```

   Default weights: FTS 0.6, vector 0.4. Weights are configurable per query.

4. **Temporal decay** — an optional exponential decay can be applied per category. The MCP server configures `note` facts with a 30-day half-life; `preference` and `identity` facts do not decay.

`SearchBatch` amortizes embedding cost across multiple queries by making a single batched Ollama call instead of one per query.

## Supersession

Facts form linked chains. When knowledge evolves, the old version is preserved in history and the new version becomes active:

```
fact 42 (SUPERSEDED → 43)  "Matthew prefers small, logical commits"
        ↓
fact 43 (ACTIVE)            "Matthew prefers small commits and squashes fixup commits before merging"
```

**Explicit supersession** — pass `supersedes=<id>` to `memory_store`, or call `memory_supersede` after storing the replacement separately.

**Automatic supersession** — the `FactExtractor` pipeline embeds each new fact immediately after insert, then searches for same-subject active facts. If cosine similarity reaches the 0.85 threshold, the closest match is automatically superseded. A metadata conflict check prevents facts from different contexts (different projects, sources, etc.) from superseding each other across context boundaries.

`memory_history` walks the full chain in either direction — useful for auditing how a piece of knowledge has changed over time.

## License

MIT
