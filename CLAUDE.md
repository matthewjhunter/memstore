# Memstore

Persistent memory system for Claude, backed by SQLite with hybrid FTS5 + vector search.

## Architecture

- `store.go` — `Fact` struct, `Store` interface, `MetadataFilter`, `SearchOpts`, `QueryOpts`, `HistoryEntry`, `Generator`/`JSONGenerator`
- `sqlite.go` — `SQLiteStore` implementation, schema migrations (V1–V6), CRUD, embedder model validation
- `search.go` — Hybrid FTS5 + cosine similarity search, score merging, temporal decay, `SearchBatch`
- `embedding.go` — `Embedder` interface, `Single`, `embedWithRetry`, `CosineSimilarity`, encode/decode helpers
- `ollama.go` — `OllamaEmbedder` (Ollama HTTP API `/api/embed` implementation)
- `extract.go` — LLM-based fact extraction with auto-supersession, `MetadataConflicts`
- `transfer.go` — Export/import for backup and migration (embeddings excluded, re-embed after import)
- `mcpserver/server.go` — MCP tool handlers that bridge tool calls to the Store
- `cmd/memstore-mcp/` — MCP server binary entry point
- `cmd/memstore/` — CLI binary with export/import subcommands

## Key patterns

- All Store methods are namespace-scoped. The namespace is set at construction time.
- `scanFact` and `factColumns` must stay in sync — if you add a column, update both.
- `searchFTS` has its own column list (prefixed with `f.`) — update it too.
- Transfer (`Export`/`Import`) has its own scan — update `ExportedFact` and the query.
- Schema changes go in a new `migrateVN()` function, bump `schemaVersion`, wire in `migrate()`.
- The `mu` mutex protects all DB access. Reads use `RLock`, writes use `Lock`.
- Schema version is currently 6. Migrations are cumulative (V1–V6).
- Embedder model is validated at store open: `NewSQLiteStore` records the model on first use and rejects mismatches on subsequent opens.
- Bidirectional relationships: relationship facts are directional by subject. Store both directions at insert time to ensure reliable lookup from either side (see `embedding.go` package doc).

## MCP tools

Twelve tools registered in `mcpserver/server.go`:

- `memory_store` — persist a fact with subject, category, optional metadata and supersession
- `memory_search` — hybrid FTS5 + vector search with metadata filters; auto-touches results (bumps `use_count`)
- `memory_list` — browse facts by subject/category/metadata without a query
- `memory_delete` — remove a fact by ID (prefer supersession)
- `memory_supersede` — mark an existing fact as replaced by another
- `memory_history` — show supersession chain (by ID) or all facts for a subject
- `memory_confirm` — increment confirmation count (explicit trust signal)
- `memory_status` — active fact count with subject/category breakdown
- `memory_update` — patch metadata on an existing fact (merge keys, delete with nil)
- `memory_task_create` — create a task with enforced metadata schema (kind, scope, status, priority, surface)
- `memory_task_update` — transition task status; completing/cancelling removes startup surface flag
- `memory_task_list` — list tasks filtered by scope, status, and/or project

Search defaults in the MCP layer: limit 10 (max 50), `CategoryDecay` of 30 days for "note" category (stable categories like preference/identity don't decay), FTS weight 0.6 / vector weight 0.4.

## Task metadata conventions

Tasks are facts with `subject="todo"`, `category="note"`, and structured metadata:

| Key | Values | Description |
|-----|--------|-------------|
| `kind` | `"task"` | Distinguishes tasks from regular facts |
| `scope` | `"matthew"`, `"claude"`, `"collaborative"` | Who owns/drives the task |
| `status` | `"pending"`, `"in_progress"`, `"completed"`, `"cancelled"` | Current state |
| `priority` | `"high"`, `"normal"`, `"low"` | Urgency (default: normal) |
| `surface` | `"startup"` or absent | When set, task appears in startup surfacing queries |
| `project` | any string | Optional grouping key |
| `due` | free-form date string | Optional due date |
| `note` | any string | Optional transition context (set by `memory_task_update`) |

**Startup surfacing pattern:** At session start, query `memory_list(metadata: {surface: "startup"})` to retrieve all pending/in-progress tasks. Completing or cancelling a task removes the `surface` flag automatically.

## Supersession

- Facts are linked via `superseded_by` pointers forming chains (oldest → newest).
- `trySupersedeExisting()` runs after insert during extraction: finds same-subject active facts with cosine similarity ≥ 0.85 and auto-supersedes them.
- `MetadataConflicts()` prevents auto-supersession when shared metadata keys have different values — facts from different contexts won't accidentally replace each other.
- Explicit supersession (`memory_supersede` tool, `supersedes` param on `memory_store`) bypasses metadata conflict checks.
- `History()` walks chains by ID or lists all facts by subject including superseded ones.

## Usage tracking

- `use_count` / `last_used_at` — incremented automatically when facts appear in search results (via `Touch()`). Passive relevance signal.
- `confirmed_count` / `last_confirmed_at` — incremented explicitly via `memory_confirm`. Active trust signal ("I verified this is still true").

## Metadata

- Stored as JSON in the `metadata` column. Used for attribution, context, temporal info, etc.
- `MetadataFilter` supports operators: `=`, `!=`, `<`, `<=`, `>`, `>=`. Keys are validated (alphanumeric + underscore only) to prevent SQL injection via `json_extract` paths.
- `IncludeNull` option on filters: when true, rows with missing keys also match (useful for unscoped facts that apply universally).
- MCP tools expose metadata filters as key-value equality matches on `memory_search` and `memory_list`.
- `MetadataConflicts(a, b)` compares shared top-level keys — used by auto-supersession to avoid cross-context replacement.

## Conventions

- Subjects: lowercase, singular entity names ("matthew", "memstore", "home-server")
- Categories: preference, identity, project, capability, relationship, world, note
- Supersession over deletion: prefer `Supersede()` to preserve history
- Embeddings: computed at insert time in MCP handlers; `Extract` pipeline handles its own

## Setup

### Prerequisites

- **Ollama** running locally with an embedding model pulled (default: `embeddinggemma`).

### Build and install

```bash
cd /path/to/memstore
GOWORK=off go install ./cmd/memstore-mcp
```

This places the binary in `$GOPATH/bin/memstore-mcp` (typically `~/go/bin/memstore-mcp`).

### Register with Claude Code

Claude Code manages MCP servers via `claude mcp add`. User-scoped servers are stored in `~/.claude.json` and available in all projects.

```bash
claude mcp add memstore -s user -- memstore-mcp
```

With explicit flags:

```bash
claude mcp add memstore -s user -- memstore-mcp --model embeddinggemma --ollama http://localhost:11434
```

Verify the server is connected:

```bash
claude mcp list        # should show memstore as ✓ Connected
claude mcp get memstore  # show full config details
```

To remove:

```bash
claude mcp remove memstore -s user
```

**Important:** Do not manually create `~/.claude/.mcp.json` or edit `~/.claude.json` by hand. The `claude mcp` CLI is the supported way to manage MCP server registrations. The `~/.claude/.mcp.json` file is not read for user-scoped servers.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | `~/.local/share/memstore/memory.db` | Path to SQLite database |
| `--namespace` | `default` | Namespace for fact isolation |
| `--ollama` | `http://localhost:11434` | Ollama base URL |
| `--model` | `embeddinggemma` | Embedding model name |

### Suggested startup usage

Add instructions to your global CLAUDE.md (or equivalent) to search memory at session start and store environmental context proactively:

- **Startup recall**: search for the user's profile/preferences, the current system's hardware, and a project inventory so the assistant has context without asking.
- **Repo inventory**: maintain a single consolidated fact (e.g., subject: the user's name, category: project) listing all known repos — name, path, and one-line description. When a new repo is encountered, supersede the old list with an updated version. Store detailed per-project information separately when working inside a repo.
- **Hardware profile**: store CPU, memory, and GPU info once per machine (subject: hostname, category: world). Supersede if hardware changes.

## Testing

```bash
GOWORK=off go test ./... -count=1
```

Tests use in-memory SQLite with mock embedders. The `mockEmbedder` in `embedding_test.go` is the canonical test helper; `mcpserver/server_test.go` has its own copy.

## Build

```bash
GOWORK=off go build ./cmd/memstore-mcp
```

The `GOWORK=off` is needed because this repo may be referenced by a parent go.work file.
