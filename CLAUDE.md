# Memstore

Persistent memory system for Claude, backed by SQLite with hybrid FTS5 + vector search.

## Architecture

- `store.go` — `Fact` struct, `Store` interface, search/query option types
- `sqlite.go` — `SQLiteStore` implementation, schema migrations, CRUD
- `search.go` — Hybrid FTS5 + cosine similarity search, score merging
- `embedding.go` — `Embedder` interface, `OllamaEmbedder`, `CosineSimilarity`
- `extract.go` — LLM-based fact extraction with auto-supersession
- `transfer.go` — Export/import for backup and migration
- `mcpserver/server.go` — MCP tool handlers that bridge tool calls to the Store

## Key patterns

- All Store methods are namespace-scoped. The namespace is set at construction time.
- `scanFact` and `factColumns` must stay in sync — if you add a column, update both.
- `searchFTS` has its own column list (prefixed with `f.`) — update it too.
- Transfer (`Export`/`Import`) has its own scan — update `ExportedFact` and the query.
- Schema changes go in a new `migrateVN()` function, bump `schemaVersion`, wire in `migrate()`.
- The `mu` mutex protects all DB access. Reads use `RLock`, writes use `Lock`.

## Conventions

- Subjects: lowercase, singular entity names ("matthew", "memstore", "home-server")
- Categories: preference, identity, project, capability, relationship, world, note
- Supersession over deletion: prefer `Supersede()` to preserve history
- Embeddings: computed at insert time in MCP handlers; `Extract` pipeline handles its own

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
