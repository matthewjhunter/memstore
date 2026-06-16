# Plan 005: Guard the History chain walks against supersession cycles

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- sqlite.go pgstore/store.go sqlite_test.go pgstore/store_test.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

Both backends' `historyByID` walk the supersession chain by following
`superseded_by` pointers with no visited-set and no iteration cap. The
normal API cannot create a cycle (Supersede points an old fact at a newly
inserted one), but `Import` writes `superseded_by` values straight from an
export file, and direct SQL or a future bug can too. One cycle in the data
(A -> B -> A) and any `History` call on a fact in that chain loops forever,
issuing queries until the process is killed -- in daemon mode that hangs a
request goroutine and burns a connection indefinitely. The guard is a few
lines per walk and turns corrupt data into a truncated-but-terminating
answer.

## Current state

- `sqlite.go:1101-1142` -- `historyByID`. Backward walk (predecessors via
  `WHERE superseded_by = ?`) and forward walk (following `SupersededBy`),
  both unbounded:

  ```go
  // Walk backward: find predecessors (facts whose superseded_by points to us).
  var backward []Fact
  current := anchor.ID
  for {
      row := s.db.QueryRowContext(ctx,
          `SELECT `+factColumns+` FROM memstore_facts WHERE superseded_by = ? AND namespace = ?`,
          current, s.namespace)
      pred, err := scanFact(row)
      if err != nil {
          break // no more predecessors
      }
      backward = append(backward, *pred)
      current = pred.ID
  }
  ...
  // Walk forward: follow SupersededBy pointers.
  current = anchor.ID
  if anchor.SupersededBy != nil {
      next := *anchor.SupersededBy
      for {
          row := s.db.QueryRowContext(ctx, `SELECT `+factColumns+` ... WHERE id = ?...`, next, s.namespace)
          succ, err := scanFact(row)
          if err != nil {
              break
          }
          chain = append(chain, *succ)
          if succ.SupersededBy == nil {
              break
          }
          next = *succ.SupersededBy
      }
  }
  ```

- `pgstore/store.go:700-753` -- structurally identical walks with `$1`
  placeholders and `s.pool.QueryRow`.
- Test setups:
  - SQLite: `openTestStore(t)` at `sqlite_test.go:17`; tests that need raw
    SQL open their own `sql.Open("sqlite", ":memory:")` handle and pass it
    to `memstore.NewSQLiteStore` (see `TestNewSQLiteStore_TablesExist` at
    `sqlite_test.go:42` for the pattern).
  - Postgres: `newTestStore(t)` at `pgstore/store_test.go:50`, env-gated by
    `MEMSTORE_TEST_PG` (`testDSN`, line 41). For raw SQL the test can build
    its own `pgxpool.New(ctx, testDSN(t))` exactly as `newTestStore` does.

## Commands you will need

| Purpose     | Command                                                            | Expected on success |
|-------------|--------------------------------------------------------------------|---------------------|
| Build       | `GOWORK=off go build ./...`                                        | exit 0              |
| Tests       | `GOWORK=off go test -race -count=1 ./...`                          | all packages ok     |
| Focused     | `GOWORK=off go test -race -count=1 -run History ./...`             | ok                  |
| Tests (pg)  | `MEMSTORE_TEST_PG=<dsn> GOWORK=off go test -race -count=1 -run History ./pgstore/` | ok (or rely on CI) |

## Scope

**In scope** (the only files you should modify):
- `sqlite.go` (`historyByID` only)
- `pgstore/store.go` (`historyByID` only)
- `sqlite_test.go`
- `pgstore/store_test.go`

**Out of scope** (do NOT touch, even though they look related):
- `historyBySubject` in either backend -- it is a single ORDER BY query, no
  walk, no cycle exposure.
- `Supersede`, `Import`, or any attempt to *prevent* cycles at write time --
  worthwhile separately, but this plan only makes reads terminate.
- `httpapi/` history handlers -- unchanged semantics for valid data.

## Git workflow

- Branch off `main`: `advisor/005-history-cycle-guard`
- Suggested commits: one per backend or one combined, e.g.
  `history: terminate chain walks on supersession cycles`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add a visited set to the SQLite walks

In `sqlite.go` `historyByID`, after scanning the anchor:

```go
visited := map[int64]bool{anchor.ID: true}
```

- Backward walk: after a successful `scanFact`, check
  `if visited[pred.ID] { break }` before appending; then
  `visited[pred.ID] = true`.
- Forward walk: at the top of the loop body, check
  `if visited[next] { break }` before querying; after a successful scan,
  `visited[succ.ID] = true`.

Behavior on a cycle: the walk stops where the chain repeats and returns the
entries collected so far. No new error return -- valid data is unaffected
and corrupt data degrades to a finite chain.

**Verify**: `GOWORK=off go test -race -count=1 -run History ./` -> ok
(existing history tests unchanged and passing)

### Step 2: Mirror the guard in pgstore

Apply the identical guard to `pgstore/store.go:700-753`. Same variable
names so the two implementations stay diffable.

**Verify**: `GOWORK=off go build ./...` -> exit 0

### Step 3: Tests

See test plan.

**Verify**: `GOWORK=off go test -race -count=1 -run History ./...` -> ok,
including the new cycle tests (sqlite always; pgstore when
`MEMSTORE_TEST_PG` is set)

## Test plan

- `sqlite_test.go` -- `TestHistory_CycleTerminates`: open a raw
  `sql.Open("sqlite", ":memory:")` handle (model on
  `TestNewSQLiteStore_TablesExist`), build the store over it, insert two
  facts A and B normally, then force a cycle with raw SQL:
  `UPDATE memstore_facts SET superseded_by = <B> WHERE id = <A>` and
  `UPDATE memstore_facts SET superseded_by = <A> WHERE id = <B>`.
  Call `store.History(ctx, A, "")`. Assert it returns (the old code would
  hang -- wrap in a `t.Deadline`-aware context or rely on the package test
  timeout), errors nil, and the chain length is finite (2 or 3 entries
  depending on where the walk enters; assert `len(entries) <= 3` rather
  than an exact number).
- `pgstore/store_test.go` -- same test, env-gated, using a `pgxpool` for
  the raw UPDATEs (model the pool construction on `newTestStore`).
- A three-node cycle (A -> B -> C -> A) variant is optional; the two-node
  case exercises both walks.
- Run: `GOWORK=off go test -race -count=1 ./...` -> all pass.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `TestHistory_CycleTerminates` exists in both `sqlite_test.go` and `pgstore/store_test.go` and passes (pg via CI if no local Postgres)
- [ ] Existing History tests pass without modification
- [ ] `grep -n "visited" sqlite.go pgstore/store.go` shows the guard in both `historyByID` functions
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- Either `historyByID` no longer matches the excerpts.
- An existing History test fails -- the guard must be invisible for acyclic
  data; a failure means the visited logic is misplaced.
- The fix appears to need a Store interface change or a new error type --
  out of scope; report instead.

## Maintenance notes

- This makes reads safe but does not prevent cycles from being written.
  If Import gains validation someday, it should reject exports whose
  `superseded_by` graph contains a cycle; that check belongs there, not in
  History.
- Plan 006 (conformance suite) should absorb the cycle test so both
  backends stay covered from one place.
- Reviewers: confirm the forward walk checks `visited[next]` *before*
  issuing the query (saves the extra round-trip and is the actual
  termination point).
