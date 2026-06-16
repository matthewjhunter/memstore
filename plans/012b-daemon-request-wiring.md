# Plan 012b: Daemon request wiring -- per-user scoped stores at the auth boundary, queues, e2e isolation

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 50b1ddd..HEAD -- httpapi/ cmd/memstored/ store.go session.go`
> Expected: empty. On a mismatch, compare "Current state" against live code;
> treat contradictions as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: HIGH (this is where isolation reaches the live wire; a handler
  or queue path left on the unscoped store leaks across users -- the
  two-token e2e battery is the proof, not the diff)
- **Depends on**: plans 011 (PR #87, merged) and 012a (PR #88, merged)
- **Category**: security (phase 3b of 4 for v0.4.0 multi-user isolation)
- **Planned at**: commit `50b1ddd` (main), 2026-06-12

## Why this matters

Phases 1-3a built the isolated storage layer: scoped `PostgresStore`
(`ForUser`/`ServiceScope`, #87) and scoped `SessionStore`
(`SessionUserScoper`, #88). But the daemon still serves every request from
ONE default-scoped store and ONE default-scoped session store
(`cmd/memstored/main.go:103-107,155`), and `httpapi.Identity` does not even
carry a user id -- it is set on the request context and never read. This
plan connects the wire to the storage: the token's user becomes the
request's scope, every handler and the recall path operate on a per-user
store, the extract queue runs each job as its session's owner, and the
embed queue runs as an explicit cross-user service. Two tokens, two users,
zero bleed -- proven end to end. Spec: plan 009 D2/D3/D5/D6.

## Current state (post-#88)

- **Identity is decorative.** `httpapi/identity.go` -- `Identity{Name,
  Scopes, Source}`, NO `UserID`. `cmd/memstored/main.go` `tokenVerifier.
  VerifyToken` builds `httpapi.Identity{Name: r.Name, Scopes: r.Scopes,
  Source: "bearer"}` -- it DROPS `r.UserID` (pgstore `VerifyResult.UserID`
  exists and is populated, #86). `ServeHTTP` attaches the identity via
  `WithIdentity` (`httpapi/handler.go:136,148`); `IdentityFromContext` has
  no consumer.
- **One store, one session store, all requests.** `Handler{store
  memstore.Store, sessionStore memstore.SessionStore, ...}`
  (`httpapi/handler.go:32`). Handlers call `h.store.X(...)` /
  `h.sessionStore.X(...)` directly. Counts on main:
  - `h.store.` -- 26 call sites across `httpapi/handler.go`,
    `httpapi/recall.go`, `httpapi/session_archive.go`,
    `httpapi/context_archive.go`.
  - `h.sessionStore.` -- 9 call sites across the same files (incl.
    `recall.go:234` FeedbackScorer assertion, `recall.go:426`
    RecordInjection).
- **Scoping primitives exist (just unused by the daemon).**
  - `memstore.UserScoper{ ForUser(userID int64) (Store, error) }`
    (`store.go`); `PostgresStore.ForUser` (`pgstore/store.go:122`) and
    `PostgresStore.ServiceScope() *PostgresStore` (`:137`).
  - `memstore.SessionUserScoper{ ForUser(userID int64) (SessionStore,
    error) }` (`session.go`); `SessionStore.ForUser` and
    `SessionStore.ServiceScope() *SessionStore`
    (`pgstore/session_store.go:106`).
  - `pgstore.EnsureUser(ctx, pool, namespace, name) (int64, error)`.
  - `New()` returns a store ALREADY scoped to the default user
    (resolveUser at construction), so `h.store` today == default-user
    store. Same for `NewSessionStore`.
- **Queues hold one store.** `NewExtractQueue(store, embedder, gen,
  sessionStore)` (`httpapi/extractqueue.go:167`); `processJob` uses
  `q.store.X` / the hint store throughout (Get :118, SearchBatch :294,
  LinkFacts :316, Search :347, Get :621, Insert :958). `extractJob{
  SessionID, CWD, Persona, Turns}` (`:23`) -- NO owner. Enqueued at
  `httpapi/session_archive.go:54` inside `handleSessionTranscript` (which
  runs in a request context -- the identity is reachable).
  `NewEmbedQueue(store, ...)` (`:220` in main.go); `ProcessOnce` calls
  `eq.store.NeedingEmbedding(context.Background(), ...)`.
  `BackfillFeedback` runs at startup (`cmd/memstored/main.go:~203`) across
  all sessions.
- **memstored holds the concrete pgStore** (`cmd/memstored/main.go:103`
  `pgStore, _ := pgstore.New(...)`; `:107` `var store memstore.Store =
  pgStore`) -- so it CAN call `pgStore.ServiceScope()` for the embed queue
  and backfill.
- Handler tests (`httpapi/handler_test.go`) run over an in-memory SQLite
  store, which does NOT implement `UserScoper` -- so the request-scoping
  must FALL BACK to the unscoped store when the backend is not a
  `UserScoper` (and the existing single-user handler tests must keep
  passing unchanged). The two-token isolation tests are pg-gated.

## Design (spec 009 D2/D3/D5/D6)

### D-wire-1. Identity carries the user
- Add `UserID int64` to `httpapi.Identity`. The `tokenVerifier` adapter
  in `cmd/memstored/main.go` sets it from `r.UserID`. The legacy
  single-key path (`handler.go:148`) leaves it 0 (maps to the default
  user via the fallback in D-wire-2).

### D-wire-2. Per-request scoped store + session, resolved once at the boundary
- In `ServeHTTP`, immediately after the identity is built and BEFORE
  dispatch, resolve a per-request store and session store:
  - `scopedStore := h.store`; if `us, ok := h.store.(memstore.UserScoper);
    ok && id.UserID != 0 { if s, err := us.ForUser(id.UserID); err == nil
    { scopedStore = s } else { 500 and return } }`.
  - Same for `h.sessionStore` via `memstore.SessionUserScoper`.
  - Stash both on the request context with unexported keys; add
    accessors `storeFromCtx(ctx) memstore.Store` and
    `sessionFromCtx(ctx) memstore.SessionStore` in httpapi. When no scoped
    value is present (e.g. a future caller bypassing ServeHTTP), the
    accessor returns the handler's base store -- but in practice ServeHTTP
    always sets it. To keep the accessor able to reach the base, store the
    Handler's bases as the fallback (pass them in, or have ServeHTTP
    always set the keys -- even to the unscoped base when not a
    UserScoper, so the accessor is total).
- Migrate ALL `h.store.X` and `h.sessionStore.X` call sites in
  `handler.go`, `recall.go`, `session_archive.go`, `context_archive.go`
  to `storeFromCtx(ctx).X` / `sessionFromCtx(ctx).X` (the ctx is the
  request context already in scope in every handler). The
  `recall.go:234` capability assertion (`h.sessionStore.(memstore.
  FeedbackScorer)`) asserts on `sessionFromCtx(ctx)` instead.
- Rationale (do not deviate): scope is decided in exactly ONE place
  (ServeHTTP). A handler that reads `storeFromCtx` cannot accidentally
  reach an unscoped store; a handler that still says `h.store` is a review
  failure (a grep in the done criteria enforces zero `h.store.`/`h.
  sessionStore.` in handler bodies).

### D-wire-3. Extract queue runs each job as its owner
- `extractJob` gains `UserID int64`. `handleSessionTranscript`
  (`session_archive.go`) stamps it from `IdentityFromContext(r.Context())`
  before `Enqueue`. (A job with UserID 0 -- legacy path -- falls back to
  the queue's base store, i.e. default user.)
- `processJob` derives a per-job store and session store at the top:
  `jobStore := q.store`; if `us, ok := q.store.(memstore.UserScoper); ok
  && job.UserID != 0 { jobStore, _ = us.ForUser(job.UserID) }`; same for
  the hint/session store. ALL `q.store.X` and hint-store writes inside
  processJob (and the helpers it calls: extraction, supersession search,
  A-MEM linking, summary persistence, hint generation) use the job-scoped
  store. This is the leak that forces 012b to be one plan: extraction
  WRITES facts, so an unscoped extract job would write user B's facts as
  the default user.
- `BackfillFeedback` (startup maintenance, all users) uses a SERVICE
  scope -- see D-wire-4.

### D-wire-4. Queues that legitimately span users use ServiceScope (spec D5)
- Embedding is user-agnostic (a fact needs a vector regardless of owner).
  In `cmd/memstored/main.go`, construct the embed queue with
  `pgStore.ServiceScope()` (concrete call -- main holds pgStore) instead
  of the default-user `store`. `NeedingEmbedding`/`SetEmbedding`/
  `MarkEmbedFailed` then span all users.
- `BackfillFeedback` likewise runs on a service-scoped store + service-
  scoped session store (it rates historical facts across all sessions).
  Wire the service scopes in main and hand them to the backfill call.
- The service scope is privileged and constructed ONLY in main; it is
  NEVER placed on a request context and NEVER reachable from a handler.

### D-wire-5. Semantics
- Cross-user request: the scoped store returns not-found/empty exactly as
  the storage layer already enforces (#87/#88). No handler-level
  forbidden error; existence does not leak.
- A `ForUser` failure (should not happen for a valid token's user) is a
  500, not a fallback to another user's data.

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
- `httpapi/identity.go` (UserID field)
- `httpapi/handler.go` (ServeHTTP scoping + context accessors + call-site migration)
- `httpapi/recall.go`, `httpapi/session_archive.go`, `httpapi/context_archive.go` (call-site migration; session_archive also stamps the job)
- `httpapi/extractqueue.go` (extractJob.UserID + per-job scoping in processJob)
- `cmd/memstored/main.go` (tokenVerifier carries UserID; embed queue + backfill on ServiceScope)
- Test files: `httpapi/handler_test.go`, `httpapi/recall_test.go`,
  `httpapi/extractqueue_test.go`, `httpapi/identity_test.go`, and a new
  `httpapi/isolation_test.go` (pg-gated two-token battery) and/or
  `cmd/memstored/main_test.go` if the two-token path is driven there.

**Out of scope** (do NOT touch):
- `pgstore/` and `store.go`/`session.go` storage internals -- the
  primitives are done (#87/#88); consume them, do not modify.
- The `memstore.Store` / `memstore.SessionStore` interface method sets.
- SQLite (`sqlite.go`) -- the daemon is pg; SQLite handler tests prove
  the no-UserScoper fallback keeps single-user behavior.
- Plan 013's admin/CLI/docs surface.

## Git workflow

- Branch: `advisor/012b-daemon-wiring` from `origin/main` (50b1ddd)
- Suggested commits: `httpapi: Identity.UserID + per-request scoped store
  and session`, `httpapi: migrate handler/recall call sites to
  request-scoped store`, `extractqueue: run each job as its session
  owner`, `memstored: embed queue and backfill on service scope`,
  `httpapi: two-token isolation battery`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Identity.UserID + tokenVerifier
Add the field; carry `r.UserID` in the adapter. **Verify**: `go build
./...` -> exit 0.

### Step 2: Request scoping + accessors in ServeHTTP
D-wire-2 plumbing only (do not migrate call sites yet): add the context
keys, the accessors, and the resolution block in ServeHTTP (set the keys
for EVERY authenticated request, scoped when UserScoper + UserID, base
otherwise). **Verify**: `go build ./...` -> exit 0.

### Step 3: Migrate handler/recall/archive call sites
Replace every `h.store.` / `h.sessionStore.` in the four files with
`storeFromCtx(ctx).` / `sessionFromCtx(ctx).`. **Verify**: `grep -rn
"h\.store\.\|h\.sessionStore\." httpapi/*.go | grep -v _test` -> ONLY the
ServeHTTP resolution block / the Handler-base fallback references remain
(enumerate the survivors in the report; there should be at most the
assignments in ServeHTTP). `go test -race -count=1 ./httpapi/` -> existing
SQLite handler tests pass (fallback keeps single-user behavior).

### Step 4: Extract queue per-job scoping
D-wire-3. `extractJob.UserID`, stamp at enqueue, scope in processJob.
**Verify**: `go build ./...` -> exit 0; `go test -race -count=1
./httpapi/` -> ok (existing extractqueue tests pass; the SQLite-backed
ones fall back to base store).

### Step 5: memstored service scopes
D-wire-4. Embed queue + backfill on `pgStore.ServiceScope()` /
session ServiceScope. **Verify**: `go build ./...` -> exit 0; `go test
-race -count=1 ./cmd/memstored/` -> ok.

### Step 6: Two-token isolation battery (pg-gated)
New `httpapi/isolation_test.go` (or in main_test.go), env-gated on
`MEMSTORE_TEST_PG`. Build a real `pgstore` store + token store + session
store; `EnsureUser` for two users; `Issue` a token per user; construct
the Handler with the real `tokenVerifier`. Then drive the handler with
each token and assert ZERO bleed on every surface:
- Facts: user A POSTs a fact; user B's `GET /v1/facts/{id}` (A's id) ->
  404/not-found; B's `POST /v1/search`, `GET /v1/facts`, history,
  subsystems, links -> none of A's; B's mutations on A's id -> not-found,
  A's fact unchanged.
- Recall: A's facts do not appear in B's `POST /v1/recall`.
- Session/hints/feedback: A's session turns/hints/injections/feedback
  (via the session endpoints) are invisible to B.
- Extraction: a transcript posted by B's token produces facts owned by B
  (assert via A's token seeing none, B's token seeing them) -- drive
  `POST /v1/sessions/transcript` with B's token and let the queue drain
  (or call the queue's processing synchronously if a test seam exists;
  if not, STOP and report rather than adding a sleep).
Model token issuance on `pgstore/tokens_test.go` and handler wiring on
`httpapi/handler_test.go`.

**Verify**: `go test -race -count=1 ./...` -> all ok (battery compiles;
reviewer runs it against pg).

### Step 7: Full suite + lint
**Verify**: `go test -race -count=1 ./...` -> all ok; `go fmt ./...` ->
no output; lint per table.

## Test plan

- The two-token battery (Step 6) is the proof; it is pg-gated.
- Existing SQLite handler/recall/extractqueue tests MUST pass unmodified
  (the no-UserScoper fallback preserves single-user behavior) -- if one
  needs changing, that is a STOP condition (the fallback is wrong).
- Add `TestIdentity_CarriesUserID` (unit, no pg) asserting the
  tokenVerifier-shaped translation preserves UserID, if it can be tested
  without pg (the adapter lives in main; a small table test on a fake
  VerifyResult->Identity is fine).
- `extractqueue_test.go`: extend a linking/processing test to assert a
  job with UserID scopes its writes (pg-gated) or at least that the
  per-job ForUser is invoked (a fake UserScoper store counting ForUser
  calls works without pg).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `grep -rn "h\.store\.\|h\.sessionStore\." httpapi/*.go | grep -v _test` returns ONLY the ServeHTTP resolution/fallback lines (report lists them)
- [ ] `httpapi.Identity` has a `UserID` field; the tokenVerifier sets it (grep)
- [ ] `extractJob` has `UserID`; processJob derives a per-job scoped store (grep `ForUser` in extractqueue.go)
- [ ] memstored constructs the embed queue and backfill on `ServiceScope()` (grep `ServiceScope` in main.go)
- [ ] `httpapi/isolation_test.go` exists with the two-token battery, pg-gated
- [ ] Existing SQLite handler/recall/extractqueue tests pass unmodified
- [ ] Lint clean (executor or reviewer)
- [ ] No files outside the in-scope list are modified (`git status`)

## STOP conditions

Stop and report back (do not improvise) if:

- Any existing test fails in a way needing assertion changes (other than
  additive) -- the single-user fallback must be invisible.
- A handler reaches the store through a path that is NOT the request
  context (e.g. a closure capturing `h` that outlives the request) so the
  accessor cannot scope it -- report the path.
- The extract queue has no synchronous drain seam for the e2e extraction
  assertion and the only option is a timing sleep -- report; do not add a
  sleep-based test.
- `ServiceScope()` (concrete pgstore method) is not reachable from where
  the embed queue is constructed without crossing a package boundary that
  would force an interface change -- report.
- You discover a fact/session WRITE path reachable from a request that
  cannot be scoped by the per-request store -- that is a leak the design
  missed; STOP and report, do not patch around it.

## Maintenance notes

- After this lands, the daemon is fully user-isolated end to end. Plan 013
  is operator surface + docs only -- no new isolation logic.
- Tier 1 graph endpoints, when built, must resolve their store via
  `storeFromCtx` like every other handler -- the pattern is now the law.
- Reviewers: the two-token battery is the artifact. Run it against a fresh
  pg container; then independently grep that no handler body references
  `h.store`/`h.sessionStore`. The embed-queue service scope is the one
  intentional cross-user path -- confirm it is constructed only in main
  and never on a request context.
