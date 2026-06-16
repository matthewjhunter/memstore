# Plan 008: Support numeric metadata-filter comparisons in pgstore

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: this plan is written against branch
> `advisor/006-store-conformance` at commit `df14c88`. Run
> `git diff --stat df14c88..HEAD -- pgstore/ internal/conformance/` on your
> branch; if anything changed, compare the "Current state" excerpts against
> the live code before proceeding; on a mismatch, treat it as a STOP
> condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: MED
- **Depends on**: plans/003-metadata-filter-parity.md (PR #80), plans/006-store-conformance-suite.md (PR #83)
- **Category**: bug
- **Planned at**: branch `advisor/006-store-conformance`, commit `df14c88`, 2026-06-12

## Why this matters

A metadata filter with a numeric value -- `{Key: "chapter", Op: "<=",
Value: 3}` -- works on SQLite and errors on pgstore. SQLite's
`json_extract` returns a typed value, so the comparison is numeric.
pgstore's `jsonb_extract_path_text` yields text, the comparison is
text-typed, and pgx cannot encode a Go int against it: `unable to encode 3
into text format for text (OID 25)`. The plan 006 conformance suite found
this on its first real-Postgres run and currently documents it via a
skipping subtest (`NumericMetadataComparisonDivergence`). This plan makes
pgstore compare numerically when the filter value is numeric, after which
that subtest automatically starts enforcing parity instead of skipping.
JSON-decoded request values arrive as `float64`, so every MCP/HTTP caller
sending a numeric filter hits this path.

## Current state

- `pgstore/store.go:1145-1167` -- `appendMetadataFilters` after PR #80:

  ```go
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

- `internal/conformance/conformance.go` -- `testNumericMetadataComparisonDivergence`
  inserts `{"chapter":1}` ("low chapter") and `{"chapter":9}` ("high
  chapter"), runs `List` with `{chapter <= 3}`; on error it `t.Skipf`s, on
  success it asserts exactly the low-chapter fact. The `Run` function wires
  it after `MetadataFilterMatches`.
- `memstore.MetadataFilter.Value` is `any` (root package, store.go). Values
  from JSON arrive as `float64`; Go callers may pass any int/float type.
- The SQLite reference behavior (`sqlite.go` `appendMetadataFilters`,
  `json_extract`) for `{chapter <= 3}`:
  - JSON number 1 -> numeric compare, included; JSON number 9 -> excluded.
  - Missing key -> NULL -> excluded (included only with `IncludeNull`).
  - Present but non-numeric (e.g. `"abc"`) -> SQLite type ordering makes
    `'abc' <= 3` false -> excluded, even with `IncludeNull` (the value is
    not NULL).
- Postgres tests are env-gated on `MEMSTORE_TEST_PG`
  (`pgstore/store_test.go:41-48`); CI provides a pgvector container. The
  executor sandbox has previously been unable to set this env var; the
  reviewer runs the Postgres side.

## Target SQL shape

For a numeric filter value, the comparison must (a) compare numerically,
(b) never raise a cast error on rows whose value for that key is
non-numeric, and (c) reproduce the SQLite inclusion/exclusion semantics
above. The shape that does all three (Postgres only guarantees conditional
evaluation inside CASE, so the cast must live there):

```sql
-- caseExpr: NULL when the key is missing or its value is not a JSON number
CASE WHEN jsonb_typeof(jsonb_extract_path(metadata, $K)) = 'number'
     THEN (jsonb_extract_path_text(metadata, $K))::numeric
END

-- plain:        AND <caseExpr> <op> $V
-- IncludeNull:  AND (jsonb_extract_path(metadata, $K) IS NULL OR <caseExpr> <op> $V)
```

Semantics check against SQLite: numeric value -> numeric compare (a);
missing key -> caseExpr NULL -> excluded, and the IncludeNull arm tests key
absence specifically via `jsonb_extract_path(...) IS NULL`, so missing keys
are included under IncludeNull while present-but-non-numeric values fall to
the caseExpr arm, evaluate NULL, and stay excluded -- matching SQLite in
all three corners. Reusing `$K` multiple times in one statement is valid
Postgres (already done in the text path).

## Commands you will need

| Purpose     | Command                                                            | Expected on success |
|-------------|--------------------------------------------------------------------|---------------------|
| Build       | `GOWORK=off go build ./...`                                        | exit 0              |
| Tests       | `GOWORK=off go test -race -count=1 ./...`                          | all packages ok     |
| Conformance | `GOWORK=off go test -race -count=1 -run Conformance -v ./...`      | sqlite: all subtests PASS; pgstore: SKIP without DSN |
| Vet         | `GOWORK=off go vet ./...`                                          | exit 0              |
| Format      | `go fmt ./...`                                                     | no output           |

## Scope

**In scope** (the only files you should modify):
- `pgstore/store.go` (`appendMetadataFilters` and a small numeric-type
  helper next to it)
- `pgstore/store_test.go`
- `internal/conformance/conformance.go` (strengthen the numeric subtest
  only)

**Out of scope** (do NOT touch, even though they look related):
- `sqlite.go` / `sqlite_test.go` -- SQLite is the reference behavior;
  unchanged.
- Boolean or other non-string, non-numeric filter values -- they keep the
  text path (pre-existing behavior, separate question).
- `validMetadataKey` / `validMetadataOps` -- unchanged.

## Git workflow

- Branch: `advisor/008-pgstore-numeric-filters`, created FROM
  `advisor/006-store-conformance` (this work builds on the #80 + #83 code;
  the reviewer will stack the PR accordingly).
- Suggested commit: `pgstore: compare numerically when metadata filter value is numeric`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add a numeric-type check and the numeric SQL path

In `pgstore/store.go`, next to `appendMetadataFilters`, add:

```go
// numericFilterValue reports whether a metadata filter value should use
// numeric comparison. JSON-decoded values arrive as float64; Go callers may
// pass any integer or float type.
func numericFilterValue(v any) bool {
    switch v.(type) {
    case int, int8, int16, int32, int64,
        uint, uint8, uint16, uint32, uint64,
        float32, float64, json.Number:
        return true
    }
    return false
}
```

(Add the `encoding/json` import if absent.) Then branch inside the filter
loop: when `numericFilterValue(mf.Value)`, build the CASE expression from
"Target SQL shape" (key bound once as `$K`, reused; value bound as `$V`);
otherwise keep the existing text path verbatim. If `mf.Value` is
`json.Number`, bind its string form's parsed float (`n.Float64()`) or pass
it through -- check what pgx does with `json.Number` and prefer converting
to `float64` explicitly so encoding is deterministic.

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go vet ./...` -> exit 0

### Step 2: Unit tests in pgstore (env-gated)

See test plan.

**Verify**: `GOWORK=off go test -race -count=1 ./pgstore/` -> ok (skips
without DSN; reviewer runs with one)

### Step 3: Strengthen the conformance subtest

In `internal/conformance/conformance.go`, extend
`testNumericMetadataComparisonDivergence`:

- Insert two more facts in the same subtest: one whose metadata omits
  `chapter` entirely, and one with `{"chapter":"not-a-number"}`.
- After the existing low/high assertions (keep the `t.Skipf` on error so
  unfixed backends still skip rather than fail), assert that the
  missing-key and non-numeric facts are NOT in the `{chapter <= 3}` result.
- Add an IncludeNull variant: `{Key: "chapter", Op: "<=", Value: 3,
  IncludeNull: true}` must return the low-chapter fact AND the missing-key
  fact, but NOT the high-chapter or non-numeric facts.

Update the function's doc comment: it now pins the numeric contract; the
skip path remains only for backends that have not implemented numeric
comparison.

**Verify**: `GOWORK=off go test -race -count=1 -run Conformance -v ./` ->
all subtests PASS on SQLite including the strengthened numeric one (this
proves the assertions encode SQLite's reference semantics; if SQLite fails
a new assertion, the assertion is wrong -- see STOP conditions)

### Step 4: Full suite

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> all packages ok;
`go fmt ./...` -> no output

## Test plan

- `pgstore/store_test.go` (env-gated, model on
  `TestList_InvalidMetadataFilterErrors`): `TestList_NumericMetadataFilter`
  covering: `<=` returns only the low fact; `=` with an int matches; `>`
  excludes; a missing-key fact is excluded without IncludeNull and included
  with it; a `{"chapter":"not-a-number"}` fact is excluded in both modes
  and -- critically -- causes no query error (the CASE guard).
- Strengthened conformance subtest per Step 3 (runs on both backends; on
  SQLite it must pass as-is, which validates the contract encoding).
- Existing tests must pass unmodified, including the string-filter paths
  (`TestList_InvalidMetadataFilterErrors`, `MetadataFilterMatches`).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] SQLite conformance: all subtests PASS, including the strengthened numeric subtest (no skips)
- [ ] `grep -n "jsonb_typeof" pgstore/store.go` returns at least one match
- [ ] `TestList_NumericMetadataFilter` exists in `pgstore/store_test.go`
- [ ] Existing string-filter tests pass unmodified
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated (reviewer maintains if dispatched)

## STOP conditions

Stop and report back (do not improvise) if:

- `appendMetadataFilters` no longer matches the "Current state" excerpt.
- SQLite fails one of the strengthened conformance assertions -- that means
  the reference semantics described in "Current state" are wrong somewhere;
  report which assertion and what SQLite actually returned. Do not adjust
  SQLite, and do not weaken the assertion without reporting first.
- Matching SQLite's IncludeNull corner (missing key included, present
  non-numeric excluded) turns out to be impossible with the target SQL
  shape -- report the counterexample.
- The fix appears to require changing `MetadataFilter` itself or the SQLite
  backend.

## Maintenance notes

- Once this lands and CI's Postgres run shows the numeric conformance
  subtest passing (not skipping) on pgstore, the divergence entry in
  `plans/README.md` should be marked resolved.
- Boolean filter values still take the text path on pgstore
  (`'true'/'false'` comparison) -- if that ever matters, it needs the same
  treatment with `jsonb_typeof = 'boolean'`.
- Reviewers should scrutinize the IncludeNull SQL (it intentionally tests
  `jsonb_extract_path(...) IS NULL`, key absence, not the CASE expression)
  and confirm `$K` placeholder reuse counts stay correct when both filter
  paths mix in one query.
