# Memstore

Persistent memory system for Claude, backed by SQLite with hybrid FTS5 + vector search.

## Build and test

```bash
GOWORK=off go test ./... -count=1
GOWORK=off go build ./cmd/memstore-mcp
GOWORK=off go install ./cmd/memstore-mcp
```

`GOWORK=off` is needed because this repo may be referenced by a parent go.work file.

## Critical invariants

- `scanFact` and `factColumns` must stay in sync — if you add a column, update both.
- `searchFTS` has its own column list (prefixed with `f.`) — update it too.
- Transfer (`Export`/`Import`) has its own scan — update `ExportedFact` and the query.
- Schema changes go in a new `migrateVN()` function, bump `schemaVersion`, wire in `migrate()`.
- The `mu` mutex protects all DB access. Reads use `RLock`, writes use `Lock`.
- All Store methods are namespace-scoped (set at construction time).

## Where to find details

Architecture, conventions, MCP tool reference, links schema, trigger facts, task metadata,
source provenance, config/setup, and naming conventions are stored as memstore facts
(subject: "memstore", category: "project"). They are injected automatically via recall
when relevant, or search with `memstore search --query <topic> --subject memstore`.

Key subsystems with trigger-based auto-loading: `storage`, `search`, `mcp`, `extraction`,
`embedding`, `learn`, `links`, `triggers`, `provenance`.

To populate these facts after a fresh clone:

```bash
memstore learn --repo . --subject memstore
```
