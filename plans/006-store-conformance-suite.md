# Plan 006: Add a shared conformance test suite that runs against both Store backends

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- store.go sqlite_test.go pgstore/store_test.go internal/`
> If the Store interface in store.go changed since this plan was written,
> compare the "Current state" excerpt against the live code before
> proceeding; on a mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: M
- **Risk**: LOW
- **Depends on**: plans/003-metadata-filter-parity.md, plans/005-history-cycle-guard.md
- **Category**: tests
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

memstore has two implementations of one `Store` interface and zero shared
tests. Each backend has its own suite (`sqlite_test.go`, ~1800 lines;
`pgstore/store_test.go`), written independently, so the backends can -- and
demonstrably do -- drift: plan 003 exists because pgstore silently dropped
invalid metadata filters while SQLite errored, with a comment claiming they
matched. The maintainer's CLAUDE.md already concedes the SQLite backend
"may lag features". A conformance suite that takes a store factory and runs
identical assertions against both backends turns this class of drift from
"discovered in production" into "red CI". It also gives every future Store
method a place where cross-backend parity is the default instead of an
afterthought.

## Current state

- `store.go:202-260` -- the `Store` interface. Method groups: writes
  (`Insert`, `InsertBatch`, `Supersede`, `Confirm`, `Touch`, `Delete`,
  `UpdateMetadata`), reads (`Get`, `List`, `BySubject`, `Exists`,
  `ActiveCount`, `History`), search (`Search`, `SearchBatch`, `SearchFTS`,
  `ListSubsystems`), embedding pipeline (`NeedingEmbedding`, `SetEmbedding`,
  `MarkEmbedFailed`, `EmbedFacts`), and links (continues past line 260 --
  read the full interface before starting).
- `sqlite_test.go:17-40` -- SQLite test factory pattern:

  ```go
  func openTestStore(t *testing.T) *memstore.SQLiteStore { ... }
  func openTestStoreWith(t *testing.T, embedder embedding.Embedder) *memstore.SQLiteStore {
      // sql.Open("sqlite", ":memory:") + memstore.NewSQLiteStore(db, embedder, "test")
  }
  ```

- `pgstore/store_test.go:38-60` -- Postgres factory: `testDSN(t)` skips
  unless `MEMSTORE_TEST_PG` is set; `newTestStore(t)` builds a
  `pgxpool` + `pgstore.PostgresStore`, with cleanup. CI provides a
  pgvector container (`.github/workflows/ci.yml` sets
  `MEMSTORE_TEST_PG=postgres://test:test@localhost:5432/test?sslmode=disable`).
- Both test files define an identical `mockEmbedder` (deterministic vectors,
  `dim` field) -- the conformance package will need its own copy or an
  exported helper.
- `internal/caetl/` exists, so the module already uses `internal/` --
  put the suite in `internal/conformance/`. Test packages in the root
  (`package memstore_test`) and in `pgstore_test` can both import it
  because `internal/` is visible module-wide.
- Postgres test isolation: read how `newTestStore` isolates state between
  tests (it uses a unique namespace and/or cleanup -- match whatever it
  does) before assuming truncation is safe.

## Commands you will need

| Purpose     | Command                                                            | Expected on success |
|-------------|--------------------------------------------------------------------|---------------------|
| Build       | `GOWORK=off go build ./...`                                        | exit 0              |
| Tests       | `GOWORK=off go test -race -count=1 ./...`                          | all packages ok     |
| Conformance | `GOWORK=off go test -race -count=1 -run Conformance ./...`         | ok (sqlite runs; pgstore skips without DSN) |
| Tests (pg)  | `MEMSTORE_TEST_PG=<dsn> GOWORK=off go test -race -count=1 -run Conformance ./pgstore/` | ok (or rely on CI) |
| Vet         | `GOWORK=off go vet ./...`                                          | exit 0              |
| Format      | `test -z "$(gofmt -l .)"`                                          | exit 0              |

## Scope

**In scope** (the only files you should modify/create):
- `internal/conformance/conformance.go` (create)
- `sqlite_test.go` (add the wiring test only)
- `pgstore/store_test.go` (add the wiring test only)

**Out of scope** (do NOT touch, even though they look related):
- Deleting or refactoring existing backend tests -- the conformance suite
  is additive; dedup against existing tests is a later cleanup.
- `httpapi/`, `mcpserver/` -- they test through the Store, not the
  interface contract.
- Any behavior change in either backend. If the suite exposes a divergence
  beyond the known ones, that is a STOP condition, not a fix-in-place.

## Git workflow

- Branch off `main`: `advisor/006-store-conformance`
- Suggested commits: `internal/conformance: shared Store contract tests`,
  then `sqlite,pgstore: wire conformance suite`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Create the conformance package

`internal/conformance/conformance.go`, package `conformance`. Core entry
point:

```go
// Run executes the shared Store contract tests against the store produced
// by newStore. Each subtest receives a fresh store.
func Run(t *testing.T, newStore func(t *testing.T) memstore.Store) {
    t.Run("InsertGetRoundTrip", func(t *testing.T) { ... })
    t.Run("ExistsAndDedup", func(t *testing.T) { ... })
    ...
}
```

Design constraints:
- It imports only `memstore` (and stdlib + `testing`); it must not import
  `pgstore` or database drivers, or the import graph knots.
- Every subtest calls `newStore(t)` for isolation -- never share a store
  across subtests.
- Embedding-dependent subtests need an embedder inside the store already;
  the factory owns that. Subtests that require `Search` (embedder-backed)
  should tolerate a store whose embedder is a deterministic mock --
  assert on result membership, not on score values.

Initial subtests (the contract worth pinning now):
1. `InsertGetRoundTrip` -- insert a fact with content, subject, category,
   kind, subsystem, metadata; `Get` returns every field intact (metadata
   compared as parsed JSON, not raw bytes -- SQLite stores TEXT, Postgres
   JSONB, and key order may differ).
2. `ExistsAndDedup` -- `Exists` true for exact content+subject, false
   otherwise.
3. `SupersedeAndHistory` -- A superseded by B superseded by C: `History`
   on any of the three returns the same 3-entry chain, oldest first, with
   correct `Position`/`ChainLength`; `OnlyActive` listings exclude A and B.
4. `HistoryCycleTerminates` -- lift the raw-SQL cycle test from plan 005
   if a portable formulation exists; if forcing a cycle requires raw SQL
   (it does), have the factory signature optionally expose an
   `ExecRaw(ctx, query string, args ...any) error` capability via a second
   interface the factory's return value may implement, and skip the subtest
   when it does not. Keep this simple; skipping on pgstore is acceptable
   for the first iteration.
5. `InvalidMetadataFilterErrors` -- `List` and `SearchFTS` with a bad key
   (`"bad-key!"`) and a bad op (`"LIKE"`) return non-nil errors (the plan
   003 contract).
6. `MetadataFilterMatches` -- valid filter returns exactly the matching
   fact on both backends.
7. `UpdateMetadataMergeSemantics` -- non-nil values set, nil values delete,
   untouched keys survive (the documented contract at `store.go:210-213`).
8. `EmbedQuarantine` -- `MarkEmbedFailed` removes a fact from
   `NeedingEmbedding` results.
9. `NamespaceIsolation` -- a fact inserted in namespace X is invisible to
   a store scoped to namespace Y (factories must accept or default
   distinct namespaces; if the factory signature makes this awkward, take
   a `newStoreNS(t, namespace)` second parameter instead -- decide once,
   keep it minimal).

**Verify**: `GOWORK=off go build ./internal/conformance/` -> exit 0
(note: a file containing only test helpers compiles as a normal package
because it imports `testing` but contains no `func TestXxx` -- this is the
same pattern as any exported test-suite package).

### Step 2: Wire SQLite

In `sqlite_test.go`:

```go
func TestConformance(t *testing.T) {
    conformance.Run(t, func(t *testing.T) memstore.Store {
        return openTestStoreWith(t, &mockEmbedder{dim: 4})
    })
}
```

(Adjust to the factory-with-namespace decision from Step 1.)

**Verify**: `GOWORK=off go test -race -count=1 -run Conformance ./` -> ok

### Step 3: Wire pgstore

In `pgstore/store_test.go`, the same wiring built on `newTestStore(t)` (it
already skips without `MEMSTORE_TEST_PG`).

**Verify**: `GOWORK=off go test -race -count=1 -run Conformance ./pgstore/`
-> ok with a local DSN, or `SKIP` lines without one (then CI is the gate).

### Step 4: Full suite

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> all packages ok;
`test -z "$(gofmt -l .)"` -> exit 0.

## Test plan

The plan IS tests. Acceptance: `TestConformance` exists in both backend
test files, runs all subtests on SQLite locally, and runs on Postgres in CI
(confirm by reading the CI run if no local Postgres -- the suite must not
be skipped there, since `MEMSTORE_TEST_PG` is set in
`.github/workflows/ci.yml`).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `internal/conformance/conformance.go` exists with at least the 9 subtests above (cycle subtest may skip on pgstore)
- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `grep -n "conformance.Run" sqlite_test.go pgstore/store_test.go` shows one wiring call in each
- [ ] CI run (or local `MEMSTORE_TEST_PG` run) shows the conformance subtests executing against Postgres, not skipping
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- Plans 003 or 005 have not landed (their status in `plans/README.md` is
  not DONE) -- subtests 4 and 5 encode their contracts and will fail
  against the unfixed backends.
- A conformance subtest reveals a NEW divergence between backends (beyond
  the plan 003/005 contracts). Do not change backend code to make the
  suite pass -- report the divergence with evidence; it is a fresh finding.
- The factory signature decision (namespace handling, raw-SQL capability)
  balloons past ~20 lines of interface -- the design is wrong; report.

## Maintenance notes

- Every new Store method should gain a conformance subtest in the same PR
  that adds the method -- reviewers should start asking for it.
- The existing per-backend tests overlap with this suite; pruning the
  duplicates is a separate, later cleanup (do not do it now -- the overlap
  is harmless and the old tests carry edge cases not yet ported).
- When v0.4.0 multi-user filtering lands, this suite is the natural place
  to pin "two identities cannot see each other's facts" across backends.
