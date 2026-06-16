# Plan 012a: Session-layer isolation -- user_id on session tables, scoped SessionStore

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 8fc8c4b..HEAD -- pgstore/session_store.go session.go internal/conformance/`
> Expected: empty. On a mismatch, compare "Current state" against live code;
> treat contradictions as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: MED (session migration + a second scoping surface; inert for
  the single-user daemon, so existing tests are the safety net -- reads are
  not yet wired to per-request users, that is 012b)
- **Depends on**: plan 011 (merged, PR #87)
- **Category**: security (phase 3a of 4 for v0.4.0 multi-user isolation)
- **Planned at**: commit `8fc8c4b` (main), 2026-06-12

## Why this matters

Phases 1-2 isolated facts and links. The session layer -- turns, hints,
injections, feedback -- still has NO ownership axis: two users' Claude
sessions with colliding session_ids interleave, and every hint/feedback
row is globally readable. This plan gives the session tables an owner and
makes `SessionStore` scope to it, mirroring exactly what 010 (schema +
stamping) and 011 (`ForUser` + predicates) did for facts. It is
deliberately inert for the single-user daemon: the daemon still constructs
one default-scoped SessionStore (012b makes it per-request). The proof is
a session-isolation conformance battery, not the diff. Spec: plan 009 D4.

## Current state (post-#87)

- `pgstore/session_store.go` -- `SessionStore` struct wraps a
  `*pgxpool.Pool`; `NewSessionStore(ctx, pool)` runs `migrate()`. The
  migration is NOT versioned -- it is a list of idempotent
  `CREATE TABLE IF NOT EXISTS` + `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`
  statements (e.g. the `ranker_version`/`cwd` columns were added this way,
  `pgstore/session_store.go:74`). Five tables, NONE with user_id:
  - `session_turns` (session_archive.go:28) -- UNIQUE(session_id, uuid)
  - `session_hooks` (:45)
  - `context_hints` (:54)
  - `context_injections` (:78) -- UNIQUE(session_id, ref_id, ref_type)
  - `context_feedback` (:90) -- UNIQUE(ref_id, ref_type, session_id)
- 15 `SessionStore` methods (`pgstore/session_store.go`): `SaveTurns`
  (119), `SaveHook` (142, extracts session_id from the JSON payload),
  `StoreHint` (166), `GetPendingHints` (215, OR semantics on
  session_id/cwd), `MarkHintConsumed` (233, keys on hint id only),
  `RecordInjection` (243), `WasInjected` (253), `RecordFeedback` (266),
  `GetInjectedFactIDs` (278), `GetInjectedHints` (301), `FeedbackScores`
  (321), `UnratedFactSessions` (348), `GetSessionTurns` (377),
  `FeedbackScore` (401).
- `session.go` -- the `memstore.SessionStore` interface (line 105;
  SaveTurns/SaveHook/StoreHint/GetPendingHints/MarkHintConsumed/
  RecordInjection/WasInjected/RecordFeedback) plus capability interfaces
  `FeedbackScorer` (line 100). `ContextHint`, `ContextFeedback`,
  `SessionTurn`, `FeedbackStat` types live here too.
- pgstore side patterns to mirror (from #86/#87, all present on main):
  - `memstore_meta['default_user']` holds the default user name; the
    user row lives in `memstore_users (id, namespace, name)`.
  - `pgstore.EnsureUser(ctx, pool, namespace, name) (int64, error)`
    (`pgstore/store.go:146`) resolves-or-creates a user row.
  - `PostgresStore.resolveUser` reads meta + users to get the default
    user id at construction; `userID` field; `ForUser(userID) (Store,
    error)` clone (`pgstore/store.go:122`); `ServiceScope()` clone with
    userID 0; helpers `appendUserFilter`/`userPredicate` no-op at userID 0.
  - `UserScoper` capability interface in root `store.go`.
- `internal/conformance/conformance.go` -- `Options{NewStore, NewStoreNS,
  SetSupersededBy, NewTwoUserStores}`; capability-gated families skip when
  their option is nil. No session coverage today (SessionStore is not a
  `memstore.Store`).
- Postgres tests gate on `MEMSTORE_TEST_PG`; CI provides pgvector. The
  executor sandbox cannot set the env var -- the reviewer runs the pg
  battery against a container.

## Design (mirror the facts layer, spec D4)

1. **Schema** -- add `user_id BIGINT` to all five session tables via the
   existing idempotent pattern (`ALTER TABLE <t> ADD COLUMN IF NOT EXISTS
   user_id BIGINT`). Backfill each table's existing rows to the default
   user (resolved from `memstore_meta['default_user']` + `memstore_users`
   for the daemon's namespace), THEN `ALTER COLUMN user_id SET NOT NULL`
   and add the FK `REFERENCES memstore_users(id) ON DELETE RESTRICT` and
   an index. If no default user is recorded AND any session table has
   rows, fail with the same tier3-init instruction pgstore uses -- but
   note: by the time `NewSessionStore` runs, the daemon has already
   constructed the PostgresStore (which records/requires the default
   user), so on the daemon path the default user always exists. On a
   fresh DB with empty session tables, add the column + constraints with
   no backfill (constraints hold trivially).
   - Widen the uniqueness keys to include user_id so two users' colliding
     session_ids cannot collide at the DB level:
     `UNIQUE(user_id, session_id, uuid)` on turns;
     `UNIQUE(user_id, session_id, ref_id, ref_type)` on injections;
     `UNIQUE(user_id, ref_id, ref_type, session_id)` on feedback. Use the
     `ADD CONSTRAINT ... ` / drop-old-add-new idempotent pattern already
     in the file for context_feedback (`pgstore/session_store.go:102-104`).
     STOP and report if dropping an old unique constraint is not safely
     idempotent across re-runs.
2. **Scoping** -- `SessionStore` gains a `userID int64` field, resolved at
   construction exactly like `PostgresStore` (default user from meta;
   error with tier3-init instruction only when a session table has rows
   and no default user exists). Every method:
   - WRITES (`SaveTurns`, `SaveHook`, `StoreHint`, `RecordInjection`,
     `RecordFeedback`) stamp `user_id = s.userID`.
   - READS / mutations (`GetPendingHints`, `MarkHintConsumed`,
     `WasInjected`, `GetInjectedFactIDs`, `GetInjectedHints`,
     `FeedbackScores`, `UnratedFactSessions`, `GetSessionTurns`,
     `FeedbackScore`) filter `AND user_id = s.userID` when `userID != 0`.
   - `userID == 0` = SERVICE scope: no predicate, no stamp-constraint
     issue (but a stamping write with userID 0 would violate NOT NULL --
     so writes are only valid on scoped instances; document that service
     scope is read/maintenance-only for sessions, which matches its use:
     `UnratedFactSessions`/`FeedbackScores` for BackfillFeedback).
   - `MarkHintConsumed` keys on hint id; scope it by `AND user_id =
     s.userID` so a user cannot consume another user's hint.
3. **Capability interface** in `session.go`, parallel to `UserScoper`:

   ```go
   // SessionUserScoper is implemented by session stores that support
   // per-user scoping. ForUser returns a session store whose reads and
   // writes are scoped to the given user.
   type SessionUserScoper interface {
       ForUser(userID int64) (SessionStore, error)
   }
   ```

   `pgstore.SessionStore` implements `ForUser(userID int64) (memstore.
   SessionStore, error)` (cheap clone sharing the pool, swaps userID,
   errors on userID <= 0) and a `ServiceScope() *SessionStore`. Compile
   guard: `var _ memstore.SessionUserScoper = (*SessionStore)(nil)`.
   NOTE: the concrete return type must be `(memstore.SessionStore, error)`
   to satisfy the interface -- same lesson as 011's UserScoper.
4. **Semantics**: cross-user reads return empty/not-found exactly like the
   facts layer; cross-user writes/mutations affect 0 rows. No "forbidden"
   error -- existence must not leak.

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
- `session.go` (SessionUserScoper interface ONLY -- the SessionStore
  interface itself is unchanged)
- `pgstore/session_store.go`
- `internal/conformance/conformance.go` (session isolation family)
- `pgstore/session_store_test.go` (create if absent; else extend)

**Out of scope** (do NOT touch):
- `httpapi/` entirely -- per-request session scoping, queue wiring, and
  Identity.UserID are plan 012b. The daemon keeps constructing ONE
  default-scoped SessionStore exactly as today.
- `cmd/memstored/main.go` -- 012b.
- `pgstore/store.go` (facts) -- done in #86/#87; reuse `EnsureUser` and
  the meta/users helpers, do not modify them.
- The `memstore.SessionStore` interface method set -- unchanged (handlers
  in 012b call the same methods on a scoped instance).

## Git workflow

- Branch: `advisor/012a-session-isolation` from `origin/main` (8fc8c4b)
- Suggested commits: `pgstore: add user_id to session tables and backfill`,
  `pgstore: scope SessionStore reads and writes to its user`,
  `session,conformance: SessionUserScoper + session isolation battery`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Session schema migration

Add user_id + backfill + NOT NULL + FK + index + widened unique keys to
all five tables, in the idempotent migration list (design point 1). The
default-user resolution helper can call `pgstore.EnsureUser` /
read `memstore_meta` directly (same pool). Verify the backfill ordering:
column add -> backfill -> SET NOT NULL, so a populated table never holds
NULL when the constraint applies.

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go test -race -count=1 ./...` -> existing tests pass (the
single-user daemon path is unchanged)

### Step 2: Scope every SessionStore method

Design point 2. Add `userID`, resolve at construction, stamp writes,
filter reads. Keep the predicate form uniform (a small
`userClause(args)`-style helper or inline `AND user_id = $N`) so review
can grep it.

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go test -race -count=1 ./...` -> all ok

### Step 3: ForUser/ServiceScope + capability interface

Design point 3. Interface in `session.go`, clones + compile guard in
`pgstore/session_store.go`.

**Verify**: `GOWORK=off go build ./...` -> exit 0

### Step 4: Session isolation conformance battery

Add to `internal/conformance/conformance.go` a session family gated on a
new option:

```go
// NewTwoUserSessionStores returns two SessionStores over the SAME
// database, scoped to two DIFFERENT users (with their fact-side stores
// so hints can reference real facts if needed). nil skips this family.
NewTwoUserSessionStores func(t *testing.T) (memstore.SessionStore, memstore.SessionStore)
```

Subtests (A seeds, B probes; swap roles at least once):
1. `TurnsIsolated` -- A `SaveTurns(sid, ...)`, B `SaveTurns(SAME sid,
   ...)`: B's `GetSessionTurns(sid)` returns only B's turns; A's returns
   only A's. (Proves the widened unique key and the read filter.)
2. `HintsIsolated` -- A `StoreHint`, B `GetPendingHints(sid, cwd)` for
   the same sid/cwd returns none of A's; B `MarkHintConsumed(A's hintID)`
   does not consume A's hint (A still sees it pending).
3. `InjectionsIsolated` -- A `RecordInjection`, B `WasInjected(SAME sid,
   ref, type)` is false; B `GetInjectedFactIDs(sid)` excludes A's.
4. `FeedbackIsolated` -- A `RecordFeedback`, B `FeedbackScores`/
   `FeedbackScore` for the same ref returns nothing of A's;
   B `UnratedFactSessions` excludes A's sessions.
5. `ServiceScopeSeesAll` (if a service-scoped session store is exposed
   to the test) -- a service SessionStore's `UnratedFactSessions` /
   `FeedbackScores` span both users. Skip if the factory does not expose
   service scope.

Wire in `pgstore/session_store_test.go` (env-gated) using `EnsureUser` +
`SessionStore.ForUser` for two users. No SQLite wiring (there is no
SQLite SessionStore).

**Verify**: `GOWORK=off go test -race -count=1 -run 'Session|Conformance'
./...` -> sqlite unaffected; pg compiles (reviewer runs)

### Step 5: Full suite + lint

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> all ok;
`go fmt ./...` -> no output; lint per table.

## Test plan

The session isolation battery above IS the test plan, plus in
`pgstore/session_store_test.go`:
- `TestSessionForUser_InvalidID` (0/negative -> error).
- A migration test: a fixture session DB built WITHOUT user_id (simulate
  pre-012a by creating the tables then dropping the columns, or assert
  on a fresh migrate) -> after `NewSessionStore`, user_id columns exist,
  are NOT NULL, FK present, existing rows backfilled to the default user.
- Existing session tests pass unmodified (the default-scoped path is
  behavior-identical for a single user).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] All five session tables have `user_id` NOT NULL + FK (report lists
      the migration statements)
- [ ] Every SessionStore method's predicate disposition is enumerated in
      the report (scoped-read / stamped-write / service-conditional)
- [ ] `var _ memstore.SessionUserScoper = (*SessionStore)(nil)` compiles
- [ ] Session isolation family exists (4-5 subtests) and is wired pg-gated
- [ ] Lint clean (executor or reviewer)
- [ ] No files outside the in-scope list are modified (`git status`)

## STOP conditions

Stop and report back (do not improvise) if:

- Any existing test fails -- the single-user default path must be
  behavior-identical.
- Dropping/replacing an existing UNIQUE constraint to widen it is not
  safely idempotent across migration re-runs -- report the constraint and
  the re-run failure.
- A SessionStore method's query cannot take the user predicate without a
  signature change -- report; do not change the `memstore.SessionStore`
  interface (012b depends on its shape).
- `SaveHook` (which parses session_id out of the JSON payload) has no
  clean place to stamp user_id from the store's scope -- report how it
  currently derives session_id and propose the minimal stamping point
  rather than guessing.

## Maintenance notes

- 012b consumes `SessionUserScoper.ForUser` and the scoped
  `SessionStore`; their shapes are load-bearing for the request wiring.
- The widened unique keys are the structural backstop: even if 012b's
  request scoping had a bug, two users' session_ids cannot collide at the
  DB level.
- Reviewers: verify the enumeration against
  `grep -n "session_turns\|session_hooks\|context_hints\|context_injections\|context_feedback" pgstore/session_store.go`
  and run the pg battery twice (fresh DB + re-run).
