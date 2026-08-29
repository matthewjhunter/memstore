# Memstore

Persistent memory system for Claude, backed by Postgres with hybrid full-text + vector search. The SQLite backend and the stdio `memstore-mcp` binary were removed in 0.6.0; the only piece of SQLite left is the read-only export reader (`memstore export --db`), which goes in 0.7.0.

## Purpose

Memstore exists for **cross-session, cross-repo continuity**: the slice of context that should follow the user across every session and every working directory. Repo-specific details (architecture, invariants, conventions) belong in each repo's code and CLAUDE.md -- those are authoritative there. Memstore's job is everything *else* a fresh session would otherwise have to relearn.

The primary layer is **person-shaped facts**: who the user is, their preferences, their durable interests (authors, hobbies, ongoing reading), people in their life, their hardware, the cross-repo project landscape. Project-specific facts are secondary -- stored when useful, but they should not crowd out the durable layer.

When deciding whether something belongs in memstore vs a repo's CLAUDE.md, ask: "does this travel with the user across repos?" If yes → memstore. If it's only meaningful inside one project tree → that project's CLAUDE.md or code.

## Build and test

```bash
GOWORK=off go test ./... -count=1
GOWORK=off go build ./cmd/memstored ./cmd/memstore
GOWORK=off go install ./cmd/memstored ./cmd/memstore
```

`GOWORK=off` is needed because this repo may be referenced by a parent go.work file.

Every store-backed suite opens its store through `internal/teststore`, which creates a private PostgreSQL database per test on the server `MEMSTORE_TEST_PG` names and skips when it is unset -- so an unset variable means most of the suite did not run. A throwaway server: `docker run -d --rm --name pg -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=test -p 55432:5432 pgvector/pgvector:pg17`, then `MEMSTORE_TEST_PG='postgres://test:test@localhost:55432/test?sslmode=disable' GOWORK=off go test ./...`. Tests take a `teststore.Store` and compare JSON by value rather than substring (jsonb reflows it). Mock embedders produce `teststore.VecDim` (4) wide vectors; the schema is `vector(4)`, so a test vector of any other width is rejected by Postgres.

## Critical invariants

- `scanFact` and `factColumns` must stay in sync -- if you add a column, update both.
- `searchFTS` has its own column list (prefixed with `f.`) -- update it too.
- Transfer (`Export`/`Import`) has its own scan -- update `ExportedFact` and the query.
- Schema changes go in a new `migrateVN()` function, bump `schemaVersion`, wire in `migrate()`.
- The `mu` mutex protects all DB access. Reads use `RLock`, writes use `Lock`.
- All Store methods are namespace-scoped (set at construction time).
- `Fact.Embedding` is tagged `json:"-"` and must stay that way -- it is daemon-side only. Nothing across the API consumes it: the client's `NeedingEmbedding`/`SetEmbedding`/`SetFactVectors` are no-ops, `ExportedFact` carries no vector and transfer re-embeds after import. Serializing it put 768 float32s on every fact in every response (`memstore tasks --format json` was 688 KB for 42 tasks, ~95% vectors). `TestFactJSONKeysAreGoFieldNames` pins this along with the Go-field-name convention.
- Embedder construction is env-driven via `embedding.ConfigFromEnvPrefix("MEMSTORE_EMBED")` (cascades through `EMBEDDING_*` to `embedding.DefaultConfig()`). The `Embedder` type, helpers (`Single`, `EmbedWithRetry`, `CosineSimilarity`, `EncodeFloat32s`, `DecodeFloat32s`), and `Fingerprint` come from `github.com/matthewjhunter/go-embedding`. Memstore no longer ships an `OpenAIEmbedder` -- only `OpenAIGenerator` for chat.

## Where to find details

Architecture, conventions, MCP tool reference, links schema, trigger facts, task metadata,
source provenance, config/setup, and naming conventions are stored as memstore facts
(subject: "memstore", category: "project"). They are injected automatically via recall
when relevant, or search with `memstore search --query <topic> --subject memstore`.

Key subsystems with trigger-based auto-loading: `storage`, `search`, `mcp`, `extraction`,
`embedding`, `links`, `triggers`, `provenance`.

## Auth / OIDC (HTTP API)

Today the HTTP API authenticates with static bearer tokens (`api_tokens` in pgstore), a
legacy single key, and mTLS peer identity. There is no OIDC code in the tree and
`oidclient` is not a dependency.

The planned direction is an OAuth 2.1 **resource server**: memstore validates an access
token minted by **webauth** and maps it to an `Identity`. That is not a relying party --
it runs no authorization-code flow, holds no client secret, and never talks to auth-oidc
or any upstream. The client (Claude Code, Cursor, Zed) runs the flow; webauth remains the
only federation client. Scope and design: `docs/mcp-oauth-scope.md`. The federation
invariants in `~/git/infodancer/infodancer/docs/oidc-federation-design.md` govern webauth
and the websites rather than this role, but read them before touching anything that does
authenticate users.
