# memstore

[![CI](https://github.com/matthewjhunter/memstore/actions/workflows/ci.yml/badge.svg)](https://github.com/matthewjhunter/memstore/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/matthewjhunter/memstore)](https://goreportcard.com/report/github.com/matthewjhunter/memstore)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/matthewjhunter/memstore)](go.mod)

Persistent memory system for AI agents with hybrid semantic search, fact supersession, and cross-session task tracking.

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
- Export/import for backup and migration (embeddings excluded; re-generated on import)

## Architecture

```
MCP Client (Claude, etc.)
        ↓ stdio
MCP Server (12 tools)
        ↓
Store Interface
        ↓
┌─────────────────────┐
│ SQLite               │
│ ├─ Facts table       │
│ ├─ FTS5 index        │
│ ├─ Vector embeddings │
│ └─ Schema migrations │
└─────────────────────┘
        ↓
Ollama (embeddings)
```

The MCP server exposes the store as tools over stdio. The `Store` interface (`store.go`) separates the storage engine from the MCP layer, so memstore can also be used as a Go library directly. See [docs/architecture.md](docs/architecture.md) for a detailed breakdown.

## MCP Tools

| Tool | Description |
|------|-------------|
| `memory_store` | Store a fact with automatic embedding and optional supersession |
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

## Installation

```bash
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest
```

Register with Claude Code at user scope so it's available in every project:

```bash
claude mcp add memstore -s user -- memstore-mcp
```

With explicit options:

```bash
claude mcp add memstore -s user -- memstore-mcp \
  --model embeddinggemma \
  --ollama http://localhost:11434 \
  --db ~/.local/share/memstore/memory.db
```

**Prerequisites:** [Ollama](https://ollama.ai) running locally with an embedding model pulled:

```bash
ollama pull embeddinggemma
```

See [docs/installation.md](docs/installation.md) for the full setup guide including Claude Code configuration and troubleshooting.

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
