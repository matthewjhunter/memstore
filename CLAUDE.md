# Memstore

Persistent memory system for Claude, backed by SQLite with hybrid FTS5 + vector search.

## Purpose

Memstore exists for **cross-session, cross-repo continuity**: the slice of context that should follow the user across every session and every working directory. Repo-specific details (architecture, invariants, conventions) belong in each repo's code and CLAUDE.md — those are authoritative there. Memstore's job is everything *else* a fresh session would otherwise have to relearn.

The primary layer is **person-shaped facts**: who the user is, their preferences, their durable interests (authors, hobbies, ongoing reading), people in their life, their hardware, the cross-repo project landscape. Project-specific facts are secondary — stored when useful, but they should not crowd out the durable layer.

When deciding whether something belongs in memstore vs a repo's CLAUDE.md, ask: "does this travel with the user across repos?" If yes → memstore. If it's only meaningful inside one project tree → that project's CLAUDE.md or code.

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
`embedding`, `links`, `triggers`, `provenance`.
