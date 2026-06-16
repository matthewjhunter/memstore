# Plan 003: Make pgstore reject invalid metadata filters the way SQLite does

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- pgstore/ sqlite.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

The two backends disagree about invalid metadata filters. SQLite returns an
error; pgstore silently drops the filter and runs the query without it --
returning MORE results than the caller asked for. A caller filtering by a
metadata key that fails validation gets the unfiltered superset with no
signal anything went wrong. The pgstore comment even claims the silent skip
exists "to match SQLite behavior", which is the opposite of what SQLite
does. Since pgstore is the production backend and SQLite is what gets tested
locally, this is exactly the class of silent drift the repo's invariants
warn about. While in there, the pgstore key interpolation gets parameterized
so the validation allowlist is no longer the only thing standing between a
metadata key and the SQL string.

## Current state

- `sqlite.go:1036-1058` -- the authoritative behavior. SQLite's
  `appendMetadataFilters` returns an error for a bad key or operator:

  ```go
  func appendMetadataFilters(q *string, args *[]any, alias string, filters []MetadataFilter) error {
      for _, mf := range filters {
          if !validMetadataKey(mf.Key) {
              return fmt.Errorf("memstore: invalid metadata filter key: %q", mf.Key)
          }
          if !validMetadataOps[mf.Op] {
              return fmt.Errorf("memstore: invalid metadata filter operator: %q", mf.Op)
          }
          ...
  ```

- `pgstore/store.go:1143-1158` -- the divergent implementation:

  ```go
  // appendMetadataFilters adds jsonb-based WHERE clauses using the ->> operator.
  func appendMetadataFilters(b *queryBuilder, alias string, filters []memstore.MetadataFilter) {
      for _, mf := range filters {
          if !validMetadataKey(mf.Key) || !validMetadataOps[mf.Op] {
              continue // silently skip invalid filters to match SQLite behavior
          }
          extract := fmt.Sprintf("%smetadata->>'%s'", alias, mf.Key)
          if mf.IncludeNull {
              b.args = append(b.args, mf.Value)
              b.q += fmt.Sprintf(` AND (%s IS NULL OR %s %s $%d)`, extract, extract, mf.Op, len(b.args))
          } else {
              b.args = append(b.args, mf.Value)
              b.q += fmt.Sprintf(` AND %s %s $%d`, extract, mf.Op, len(b.args))
          }
      }
  }
  ```

- Call sites of the pgstore version (all must propagate the new error):
  - `pgstore/search.go:153` (FTS search, alias `"f."`)
  - `pgstore/search.go:233` (vector search, alias `""`)
  - `pgstore/store.go:484` (List, alias `""`)
- `validMetadataKey` (`pgstore/store.go:1125-1135`) restricts keys to
  `[a-zA-Z0-9_]`, and `validMetadataOps` (`pgstore/store.go:1137-1141`)
  restricts operators to `= != < <= > >=`. Identical logic exists on the
  SQLite side. Keep both validators -- the parameterization below is
  defense in depth, not a replacement.
- The pgstore `queryBuilder` (`b.q` string + `b.args` slice, see
  `pgstore/store.go` around line 1110 for `b.write` usage) numbers
  placeholders `$N` by `len(b.args)`. Postgres allows reusing the same
  `$N` placeholder twice in one statement.
- Postgres tests are env-gated: they skip unless `MEMSTORE_TEST_PG` is set
  (`pgstore/store_test.go:41-48`); CI provides a pgvector container.

## Commands you will need

| Purpose        | Command                                                            | Expected on success |
|----------------|--------------------------------------------------------------------|---------------------|
| Build          | `GOWORK=off go build ./...`                                        | exit 0              |
| Tests (all)    | `GOWORK=off go test -race -count=1 ./...`                          | all packages ok     |
| Tests (pg)     | `MEMSTORE_TEST_PG=<dsn> GOWORK=off go test -race -count=1 ./pgstore/` | ok (or rely on CI if no local Postgres) |
| Vet            | `GOWORK=off go vet ./...`                                          | exit 0              |
| Format         | `test -z "$(gofmt -l .)"`                                          | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `pgstore/store.go`
- `pgstore/search.go`
- `pgstore/store_test.go`
- `sqlite_test.go` (only if the parity test described below is missing there)

**Out of scope** (do NOT touch, even though they look related):
- `sqlite.go` -- its behavior is the contract; do not change it.
- `httpapi/` -- mapping these errors to HTTP 400 instead of 500 is a noted
  follow-up, not this plan.
- Relaxing `validMetadataKey` to support nested keys -- explicitly not now.

## Git workflow

- Branch off `main`: `advisor/003-metadata-filter-parity`
- Suggested commit: `pgstore: error on invalid metadata filters to match sqlite`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Change pgstore appendMetadataFilters to validate and return error

Rewrite `pgstore/store.go:1144-1158` to return `error`, with the same
semantics and message shape as SQLite (use the `pgstore:` prefix to match
the package's other errors):

```go
// appendMetadataFilters adds jsonb-based WHERE clauses for each metadata
// filter. Returns an error for invalid keys or operators, matching the
// SQLite backend's behavior.
func appendMetadataFilters(b *queryBuilder, alias string, filters []memstore.MetadataFilter) error {
    for _, mf := range filters {
        if !validMetadataKey(mf.Key) {
            return fmt.Errorf("pgstore: invalid metadata filter key: %q", mf.Key)
        }
        if !validMetadataOps[mf.Op] {
            return fmt.Errorf("pgstore: invalid metadata filter operator: %q", mf.Op)
        }
        b.args = append(b.args, mf.Key)
        extract := fmt.Sprintf("jsonb_extract_path_text(%smetadata, $%d)", alias, len(b.args))
        b.args = append(b.args, mf.Value)
        if mf.IncludeNull {
            b.q += fmt.Sprintf(` AND (%s IS NULL OR %s %s $%d)`, extract, extract, mf.Op, len(b.args))
        } else {
            b.q += fmt.Sprintf(` AND %s %s $%d`, extract, mf.Op, len(b.args))
        }
    }
    return nil
}
```

Notes on the target shape:
- The key becomes a bound parameter via `jsonb_extract_path_text(metadata, $K)`,
  which is equivalent to `metadata->>'key'` for top-level keys and removes
  the key from the SQL string entirely. Reusing `$K` twice in the
  IncludeNull branch is valid Postgres.
- `mf.Op` is still interpolated -- it is allowlisted against six literal
  operators, which is safe. Do not parameterize it (operators cannot be
  bound parameters).

**Verify**: `GOWORK=off go build ./pgstore/` -> fails listing exactly the
three call sites (expected -- fixed next step).

### Step 2: Propagate the error at the three call sites

At `pgstore/search.go:153`, `pgstore/search.go:233`, and
`pgstore/store.go:484`, change each bare call to:

```go
if err := appendMetadataFilters(&b, "f.", opts.MetadataFilters); err != nil {
    return nil, err
}
```

(adjust alias and return values to each function's signature -- check what
each enclosing function returns and match it).

**Verify**: `GOWORK=off go build ./...` -> exit 0

### Step 3: Fix or remove the misleading comment

Ensure no comment claiming "silently skip ... to match SQLite behavior"
survives.

**Verify**: `grep -rn "silently skip" pgstore/` -> no matches

### Step 4: Tests

See test plan.

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> ok. If a local
Postgres DSN is available, also run the pg-gated tests; otherwise note in
the index that CI must confirm.

## Test plan

- In `pgstore/store_test.go` (env-gated like its neighbors, using
  `newTestStore(t)`): `TestList_InvalidMetadataFilterErrors` -- call
  `List` (and `SearchFTS`) with `memstore.MetadataFilter{Key: "bad-key!",
  Op: "=", Value: "x"}` and with a valid key but `Op: "LIKE"`; expect a
  non-nil error both times, and expect a valid filter to still work
  (insert a fact with metadata, filter on it, get exactly that fact back --
  this also proves `jsonb_extract_path_text` is equivalent to `->>`).
- In `sqlite_test.go`: check whether an equivalent invalid-filter test
  already exists (`grep -n "invalid metadata" sqlite_test.go`); if not, add
  the same two assertions against `openTestStore(t)` so the contract is
  pinned on both backends.
- Pattern to model on: any existing `List`/`Search` test in
  `pgstore/store_test.go`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go build ./...` exits 0
- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `grep -rn "silently skip" pgstore/` returns no matches
- [ ] `grep -n "jsonb_extract_path_text" pgstore/store.go` returns at least one match
- [ ] New invalid-filter tests exist in `pgstore/store_test.go` and pass under CI (or under a local `MEMSTORE_TEST_PG`)
- [ ] `test -z "$(gofmt -l .)"` exits 0
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The code at `pgstore/store.go:1144` no longer matches the excerpt.
- More than three call sites of `appendMetadataFilters` exist in pgstore
  (`grep -n "appendMetadataFilters" pgstore/*.go` finding a fourth caller
  means this plan's survey is stale).
- A valid-filter test fails after switching to `jsonb_extract_path_text` --
  that would mean the operator semantics differ from `->>`; report, do not
  paper over it.

## Maintenance notes

- HTTP callers currently see these errors as 500s via
  `writeError(w, http.StatusInternalServerError, ...)` in httpapi handlers.
  Mapping invalid-filter errors to 400 is a sensible follow-up once a
  typed error exists.
- Plan 006 (conformance suite) should absorb the parity test so both
  backends are checked from one place; keep the test logic simple enough to
  lift.
- If nested metadata keys are ever supported, extend
  `jsonb_extract_path_text` with one parameter per path segment -- never
  interpolate the segments.
