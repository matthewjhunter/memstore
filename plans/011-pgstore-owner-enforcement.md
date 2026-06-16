# Plan 011: Owner-only enforcement in pgstore -- user predicates, ForUser, UserIsolation conformance

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 5e6b7d1..HEAD -- store.go pgstore/ internal/conformance/`
> Expected: empty. On a mismatch, compare "Current state" against live code;
> treat contradictions as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: HIGH (this IS the isolation boundary; a missed predicate is an
  IDOR-class bug -- the conformance battery is the proof, not the diff)
- **Depends on**: plan 010 (merged, PR #86)
- **Category**: security (phase 2 of 4 for v0.4.0 multi-user isolation)
- **Planned at**: commit `5e6b7d1` (main), 2026-06-12

## Why this matters

Plan 010 made every fact, link, and token carry an owner; nothing reads it
yet. This plan makes pgstore enforce it: every query gains the owner
predicate, scoped store instances are derived per user, and a conformance
battery proves -- on every read and write path the Store interface has --
that two users sharing one database cannot see or touch each other's data.
After this plan the daemon is still single-identity (plan 012 wires
tokens to scopes), but the storage layer it will call is already safe.
Spec: plan 009 D2 (owner-only), D3 (construction-time scoping), D5
(service scope), D8 (SQLite exempt from enforcement).

## Current state (post-#86)

- `pgstore/store.go` -- `PostgresStore` carries `userID int64`, resolved
  at construction (`resolveUser`: reads `memstore_meta['default_user']`,
  resolve-or-creates the row for the store's namespace, errors with the
  tier3-init instruction when no default user is recorded). The field is
  used ONLY to stamp writes (`Insert`, `InsertBatch`, `LinkFacts`).
  No read or mutation path filters on it.
- Query construction: `queryBuilder` (`b.q` + `b.args`, `b.write`) with
  helper `appendNamespaceFilter` (`pgstore/store.go`, near line 1100) used
  by List/search paths; many simpler methods inline their SQL with
  `namespace = $N` conditions. `appendMetadataFilters` returns error
  (plan 003).
- `pgstore/search.go` -- `searchFTS` (alias `f.`), `searchVector`,
  `SearchBatch`, plus `Search` composing them.
- `memstore_links` rows carry namespace AND user_id (NOT NULL, FK, from
  #86); link queries filter namespace only. `LinkFacts` stamps user_id
  but does NOT validate that source/target facts belong to the store's
  user.
- `historyByID` (`pgstore/store.go`, ~line 700) walks `superseded_by`
  with namespace filters and the plan-005 visited guard; no user filter.
- The embedding pipeline methods (`NeedingEmbedding`, `SetEmbedding`,
  `MarkEmbedFailed`, `EmbedFacts`) filter namespace only.
- Root `store.go` -- the `Store` interface (line ~202); no user concepts.
  `Fact.UserID` exists (#86).
- `internal/conformance/conformance.go` -- `Options{NewStore, NewStoreNS,
  SetSupersededBy}`; capability-gated subtests skip when an option is nil.
  `pgstore/store_test.go` wires it env-gated; `sqlite_test.go` wires it
  unconditionally.
- `pgstore.InitIdentity(ctx, pool, namespace, defaultUser)` exists
  (#86) and is idempotent at V4.
- Postgres tests gate on `MEMSTORE_TEST_PG`; CI provides pgvector. The
  executor sandbox historically cannot set the env var -- the reviewer
  runs the pg battery against a container.

## Design (from spec 009, restated as build instructions)

1. **Scoped vs service stores.** `PostgresStore.userID > 0` = scoped:
   every fact/link query carries `AND user_id = $N`; every write stamps
   it (already true). `userID == 0` = SERVICE scope: no user predicate
   anywhere; used only by daemon-internal workers (plan 012) and only
   obtainable via an explicit constructor -- never the default.
   - `New(...)` keeps returning a store scoped to the default user
     (current behavior, now enforcing).
   - `ForUser(userID int64) (*PostgresStore, error)` -- cheap clone:
     shares pool/embedder/queryCache/vecDim, swaps userID; errors on
     userID <= 0; skips migrations.
   - `ServiceScope() *PostgresStore` -- clone with userID 0. Document it
     as privileged in the doc comment.
2. **Capability interface** in root `store.go`, next to `Store`:

   ```go
   // UserScoper is implemented by backends that support per-user scoping.
   // ForUser returns a store whose every read and write is scoped to the
   // given user. Backends without multi-user support (SQLite) do not
   // implement it.
   type UserScoper interface {
       ForUser(userID int64) (Store, error)
   }
   ```

   NOTE the return type is `(Store, error)` -- pgstore's concrete
   `ForUser` returning `(*PostgresStore, error)` does NOT satisfy it;
   either make the concrete method return `(memstore.Store, error)` or
   add a tiny adapter method. Pick one, keep it boring.
3. **Predicate placement.** Add `appendUserFilter` beside
   `appendNamespaceFilter` (no-op when `s.userID == 0`), and use it at
   every queryBuilder site. For inline SQL, add `AND user_id = $N`
   directly. EVERY method that touches memstore_facts or memstore_links
   gets the predicate -- reads, mutations, and the embedding pipeline:
   Get, List, BySubject, Exists, ActiveCount, History (anchor query,
   both walks, and historyBySubject), ListSubsystems, searchFTS,
   searchVector, SearchBatch's internals, Supersede (BOTH facts must be
   the user's), Confirm, Touch, Delete, UpdateMetadata, NeedingEmbedding,
   SetEmbedding, MarkEmbedFailed, EmbedFacts, GetLink, GetLinks,
   UpdateLink, DeleteLink, LinkFacts. Method of record: `grep -n
   "memstore_facts\|memstore_links" pgstore/*.go` and account for every
   hit in your report -- the done criteria require the enumeration.
4. **Semantics on cross-user access** (spec D2): reads return the same
   not-found/empty result as nonexistent data -- `Get` returns the
   identical error for "other user's id" and "no such id"; mutations
   affect 0 rows and surface whatever the method already does for a
   missing id (do NOT invent a new "forbidden" error -- existence must
   not leak). `LinkFacts` must reject (or 0-row no-op) when source or
   target is not the user's: enforce in SQL with
   `INSERT ... SELECT $vals WHERE (SELECT count(*) FROM memstore_facts
   WHERE id IN ($src,$tgt) AND user_id = $u AND namespace = $ns) = 2`
   or an equivalent pre-check inside a transaction; a link to another
   user's fact must be impossible, and the error indistinguishable from
   "fact does not exist".
5. **History cannot cross users**: with the user predicate on the anchor
   and both walk queries, a forged `superseded_by` pointing at another
   user's fact terminates the walk exactly like a dangling pointer.

## Commands you will need

| Purpose | Command                                                        | Expected on success |
|---------|----------------------------------------------------------------|---------------------|
| Build   | `GOWORK=off go build ./...`                                    | exit 0              |
| Tests   | `GOWORK=off go test -race -count=1 ./...`                      | all packages ok (pg gated) |
| Vet     | `GOWORK=off go vet ./...`                                      | exit 0              |
| Lint    | `GOWORK=off /home/matthew/go/bin/golangci-lint run ./...`      | 0 issues (reviewer runs if denied) |
| Format  | `go fmt ./...`                                                 | no output           |

## Scope

**In scope** (the only files you should modify):
- `store.go` (UserScoper interface ONLY -- no Store interface changes)
- `pgstore/store.go`, `pgstore/search.go`
- `internal/conformance/conformance.go`
- `pgstore/store_test.go`
- `pgstore/tokens.go` ONLY if compilation forces it (it should not)

**Out of scope** (do NOT touch):
- `sqlite.go` / `search.go` (root) -- SQLite does not enforce (spec D8);
  it must NOT implement UserScoper.
- `httpapi/` entirely -- queues keep using the default-scoped store until
  plan 012; that is the planned transitional state.
- `pgstore/session_store.go` -- plan 012.
- `sqlite_test.go` -- the conformance Options zero-value already skips
  the new family there; no edit needed.

## Git workflow

- Branch: `advisor/011-owner-enforcement` from `origin/main` (5e6b7d1)
- Suggested commits: `pgstore: scope every fact and link query to the
  store's user`, `store: add UserScoper capability interface`,
  `conformance: UserIsolation battery`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: UserScoper + ForUser/ServiceScope

Root `store.go`: add the interface (design point 2). `pgstore/store.go`:
add `ForUser` and `ServiceScope` clones (design point 1), and a compile
guard `var _ memstore.UserScoper = (*PostgresStore)(nil)`.

**Verify**: `GOWORK=off go build ./...` -> exit 0

### Step 2: Predicates everywhere

Design points 3-5. Work file by file; after each, re-run the build. Keep
the predicate form uniform so review can grep it.

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go test -race -count=1 ./...` -> all ok (sqlite-side
conformance and unit tests prove no regression for the single-user path;
pg execution is the reviewer's)

### Step 3: Conformance UserIsolation family

`internal/conformance/conformance.go`: Options gains

```go
// NewTwoUserStores returns two stores over the SAME underlying database
// and namespace, scoped to two DIFFERENT users. nil skips the
// UserIsolation family (backends without per-user scoping).
NewTwoUserStores func(t *testing.T) (memstore.Store, memstore.Store)
```

Subtests (each builds fresh stores; A seeds, B probes):
1. `GetCrossUserNotFound` -- B's `Get(A's id)` error is identical
   (errors.Is or string-compare) to `Get(nonexistent id)`.
2. `ListAndSearchIsolated` -- `List`, `BySubject`, `Exists`,
   `ActiveCount`, `SearchFTS`, `Search`, `SearchBatch`,
   `ListSubsystems` for B return zero of A's rows (and vice versa with
   roles swapped at least once).
3. `MutationsIsolated` -- B's `Confirm`/`Touch`/`Delete`/
   `UpdateMetadata`/`Supersede`/`SetEmbedding`/`MarkEmbedFailed` against
   A's ids leave A's data byte-identical (A re-reads and compares) and
   return the same outcome those methods give for nonexistent ids.
4. `LinksIsolated` -- B cannot `LinkFacts` to or from A's facts; B's
   `GetLinks`/`GetLink`/`UpdateLink`/`DeleteLink` cannot see or affect
   A's links.
5. `HistoryCannotCrossUsers` -- requires `SetSupersededBy`: forge A's
   fact's superseded_by to point at B's fact; A's `History` terminates
   without ever returning B's fact; B's `History` on its own fact does
   not include A's.
6. `EmbedPipelineIsolated` -- A inserts a fact without embedding
   (factory permitting; otherwise SetEmbedding-then-clear via raw hook is
   overkill -- insert via a store whose embedder is nil if the factory
   supports it, else SKIP this subtest with a comment).
   B's `NeedingEmbedding` does not return it.

Wire in `pgstore/store_test.go`: build two users with
`pgstore.InitIdentity` for the default plus a second user row -- if no
exported helper exists to create a second user, add
`pgstore.EnsureUser(ctx, pool, namespace, name string) (int64, error)`
to `pgstore/store.go` (it is needed by plan 013's user-add anyway), then
`ForUser` for each. SQLite wiring: untouched -- the nil option skips.

**Verify**: `GOWORK=off go test -race -count=1 -run Conformance ./...`
-> sqlite passes with UserIsolation SKIPPED; pg compiles (reviewer runs)

### Step 4: Full suite + lint

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> all ok;
`go fmt ./...` -> no output; lint per table.

## Test plan

The conformance family above IS the test plan, plus:
- `pgstore/store_test.go`: `TestForUser_InvalidID` (0 and negative ->
  error), `TestServiceScope_SeesAllUsers` (service store lists both
  users' facts -- the ONE place cross-user visibility is correct),
  `TestLinkFacts_CrossUserRejected` (direct unit test of design point 4's
  SQL, asserting the not-found-shaped error).
- Existing tests must pass unmodified. Watch specifically: plan 005/006
  cycle and conformance tests, the migration fixtures from #86, and
  `TestList_NumericMetadataFilter` -- all run through scoped stores now;
  if any fails, the predicate broke single-user behavior somewhere
  (STOP condition).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] Report enumerates EVERY `memstore_facts`/`memstore_links` query
      site in pgstore and states its predicate disposition (scoped /
      service-conditional / deliberately-namespace-only-with-reason)
- [ ] `var _ memstore.UserScoper = (*PostgresStore)(nil)` compiles
- [ ] UserIsolation family exists with the 6 subtests; sqlite run shows
      them SKIPPED, not absent
- [ ] `TestServiceScope_SeesAllUsers` exists and passes (pg)
- [ ] Lint clean (executor or reviewer)
- [ ] No files outside the in-scope list are modified (`git status`)

## STOP conditions

Stop and report back (do not improvise) if:

- Any existing test fails -- the default-user scope must be invisible to
  single-user deployments; a failure means a predicate changed behavior.
- A query site cannot take the user predicate without changing the Store
  interface or a method signature -- report it; do not change the
  interface.
- The `Get`-error-indistinguishability requirement conflicts with how an
  existing caller distinguishes errors -- enumerate the caller, report.
- You find a fact/link query site reachable from handlers that
  structurally cannot be scoped (e.g. an aggregate the design missed) --
  that is a spec gap, not an improvisation opportunity.

## Maintenance notes

- Plan 012 consumes `UserScoper` and `ServiceScope` -- their shapes are
  load-bearing for the next phase; flag any signature deviation in the
  report.
- Tier 1 graph queries must use the same predicate pattern in their
  recursive CTEs; this plan's uniform `appendUserFilter` form is the
  template.
- Reviewers: the enumeration in the report is the review artifact --
  verify it against `grep -n "memstore_facts\|memstore_links"
  pgstore/*.go` yourself, then run the pg battery twice (fresh DB, then
  re-run) to shake out state-dependence.
