# Plan 004: Stop holding the SQLite write lock across embedding HTTP calls in EmbedFacts

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- sqlite.go sqlite_test.go`
> If either file changed since this plan was written, compare the "Current
> state" excerpts against the live code before proceeding; on a mismatch,
> treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

`SQLiteStore.EmbedFacts` takes the store-wide write lock at entry and holds
it -- via `defer` -- through every batched embedding HTTP call. Embedding a
backlog of N facts means the entire store (every search, insert, recall) is
frozen for the full duration of N/batchSize network round-trips, which can
be tens of seconds against a local Ollama and worse against a remote
endpoint. The repo already knows this rule: `search.go:35-36` documents
"Hold the read lock only for the DB queries; rerank fusion ... makes a
network call and must not run while blocking writers", and `Search` computes
its query embedding before taking the lock. EmbedFacts is the one method
that violates its own store's locking discipline.

## Current state

- `sqlite.go:899-994` -- `EmbedFacts`. The shape today:

  ```go
  func (s *SQLiteStore) EmbedFacts(ctx context.Context, batchSize int) (int, error) {
      ...
      s.mu.Lock()
      defer s.mu.Unlock()          // <- held for the whole function

      rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM memstore_facts WHERE embedding IS NULL AND namespace = ? ORDER BY id`, s.namespace)
      ...collect into pending...

      for i := 0; i < len(pending); i += batchSize {
          ...
          embeddings, err := embedding.EmbedWithRetry(ctx, s.embedder, texts)  // network I/O under the write lock
          ...
          if total == 0 && i == 0 && len(embeddings[0]) > 0 {
              if err := s.recordEmbedder(len(embeddings[0])); err != nil { ... }
          }
          tx, err := s.db.BeginTx(ctx, nil)
          ...per-fact UPDATE memstore_facts SET embedding = ? WHERE id = ?...
          tx.Commit()
          ...
      }
      return total, nil
  }
  ```

- `s.mu` is a `sync.RWMutex`; the documented discipline is RLock for reads,
  Lock for writes, and never across network calls.
- `recordEmbedder` (`sqlite.go`, directly below EmbedFacts around line
  1006+) reads and writes `memstore_meta` via `s.db` -- it must run under
  the write lock.
- The lock-scope exemplar to match: `search.go:30-58` -- embedding happens
  before `s.mu.RLock()`, and the lock is released before `ScoreResults`
  (which can make a rerank network call).
- Existing coverage: `grep -n "EmbedFacts" sqlite_test.go` lists the tests
  that pin current behavior; they must keep passing unchanged.

## Commands you will need

| Purpose | Command                                                       | Expected on success |
|---------|---------------------------------------------------------------|---------------------|
| Build   | `GOWORK=off go build ./...`                                   | exit 0              |
| Tests   | `GOWORK=off go test -race -count=1 ./...`                     | all packages ok     |
| Focused | `GOWORK=off go test -race -count=1 -run EmbedFacts ./...`     | ok                  |
| Vet     | `GOWORK=off go vet ./...`                                     | exit 0              |
| Format  | `test -z "$(gofmt -l .)"`                                     | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `sqlite.go` (the `EmbedFacts` method only)
- `sqlite_test.go`

**Out of scope** (do NOT touch, even though they look related):
- `pgstore/` -- Postgres has no store mutex (MVCC); its EmbedFacts has no
  equivalent problem.
- `httpapi/embedqueue.go` -- the queue calls EmbedFacts; it needs no change.
- `recordEmbedder`, `Search`, or any other locking in sqlite.go.

## Git workflow

- Branch off `main`: `advisor/004-embedfacts-lock-scope`
- Suggested commit: `sqlite: release store lock during embedding network calls`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Restructure EmbedFacts lock scope

Target shape (three lock windows instead of one):

1. **Read phase** -- take `s.mu.RLock()`, run the pending-facts SELECT and
   collect `pending`, then `s.mu.RUnlock()`. (It is a pure read; the
   current code takes the write lock only because the whole function did.)
2. **Embed phase** -- for each batch, call `embedding.EmbedWithRetry` with
   NO lock held.
3. **Write phase** -- per batch, take `s.mu.Lock()`, run `recordEmbedder`
   (first successful batch only, preserving the existing `total == 0 && i == 0`
   condition) and the existing transaction of per-fact UPDATEs, then
   `s.mu.Unlock()` before the next batch's embed call.

Implementation notes:
- Replace the single `defer s.mu.Unlock()` with explicit lock/unlock pairs;
  a small closure per phase keeps the unlock paths honest, e.g. wrap the
  write phase in a func so `defer s.mu.Unlock()` scopes to one batch.
- Error returns inside the write phase must not return while holding the
  lock without unlocking -- using a per-batch closure with deferred unlock
  handles every path.
- Do not change the SQL, the batch size logic, the mismatch check, or the
  return values. Behavior is identical except for lock granularity.
- The TOCTOU window this opens is benign and worth a comment in code: a
  fact deleted between read and write phases makes its UPDATE a no-op; a
  fact inserted after the read phase is picked up by the next EmbedFacts
  run. State that in one comment line, since it is the kind of constraint
  the code cannot show.

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go test -race -count=1 -run EmbedFacts ./...` -> ok

### Step 2: Add a concurrency regression test

See test plan.

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> ok (race detector
clean)

## Test plan

- Existing `EmbedFacts` tests in `sqlite_test.go` must pass unchanged --
  they are the characterization suite.
- Add `TestEmbedFacts_ConcurrentWithInsert` in `sqlite_test.go`, modeled on
  `openTestStoreWith(t, embedder)` (`sqlite_test.go:22`): insert ~10 facts
  with a nil-embedding path, then run `EmbedFacts` in one goroutine while a
  second goroutine performs `Insert` and `SearchFTS` calls, joined by a
  `sync.WaitGroup`. The assertion is completion without deadlock and a
  clean `-race` run -- do not assert on timing.
  - The mock embedder for this test should sleep ~10ms per call so the
    embed phase genuinely overlaps the writer goroutine.
- Run: `GOWORK=off go test -race -count=1 ./...` -> all pass including the
  new test.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0, including `TestEmbedFacts_ConcurrentWithInsert`
- [ ] In `EmbedFacts`, no call to `embedding.EmbedWithRetry` occurs between a `Lock()`/`RLock()` and its release (verify by reading the final diff)
- [ ] Existing EmbedFacts tests pass without modification
- [ ] `test -z "$(gofmt -l .)"` exits 0
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `EmbedFacts` no longer matches the "Current state" shape (someone
  restructured it already).
- An existing EmbedFacts test fails in a way that requires changing the
  test's assertions -- behavior must not change, so a failing assertion
  means the restructure is wrong.
- The race detector flags `recordEmbedder` or the meta table -- that means
  the write-phase lock placement is wrong; report with the race trace.

## Maintenance notes

- Reviewers should scrutinize the error paths in the write phase: every
  return must release the lock (the per-batch closure pattern makes this
  visible).
- If a vector index or embedding cache is ever added to the SQLite path,
  the same three-phase shape (read / network / write) should be preserved.
- The same audit found no other network-under-lock sites in sqlite.go, but
  any future method that both touches `s.db` and calls an embedder should
  copy the `search.go:30-58` pattern.
