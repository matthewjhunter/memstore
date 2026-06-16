# Plan 009: Multi-user isolation -- design spec (v0.4.0)

> **This is a design spec for maintainer review, not an executor plan.**
> Nothing gets built from this file directly. After approval, the
> "Implementation phasing" section becomes plans 010-013, each written to
> the normal executor standard. Disagreements with this spec are edits to
> this file, not to code.

## Status

- **Priority**: P1 (published v0.4.0 commitment; closes the authn-without-authz gap)
- **Effort**: L (across the follow-on plans; this spec itself is review-only)
- **Risk**: HIGH if rushed -- isolation bugs are IDOR-class; mitigated by phasing and conformance contracts
- **Depends on**: none (008 and earlier all merged)
- **Category**: direction / security
- **Planned at**: commit `a1b701e` (main), 2026-06-12
- **Normative inputs**: `docs/tier3-permissions.md` Phase 0 (identity schema -- adopted, see D1); `docs/multi-user-data-model.md` (partially superseded, see D2); `~/git/infodancer/infodancer/docs/oidc-federation-design.md` (governs any future OIDC wiring; not touched here)

## Decisions from the maintainer (2026-06-12)

These are inputs to this spec, not open questions:

1. **True multi-user, no shared memory.** Each user is fully independent;
   no fact, link, session, or hint is ever visible to another user.
2. **One database, many users.** Users are isolated *within* a shared
   Postgres store. Separate databases remain an *organizational* boundary
   (one org = one deployment/database), never the user-isolation mechanism.

## Why this matters

memstored authenticates end to end but authorizes nothing: every valid
token sees and writes everything (`httpapi/handler.go:115-153` sets an
Identity on the context; nothing downstream ever reads it --
`IdentityFromContext` has zero call sites outside `identity.go`). The
README and `docs/MIGRATING.md` promise v0.4.0 closes this. For a project
whose bar is visibly secure by design, authn-without-authz on a network
daemon is the design smell, and it compounds: every feature shipped before
isolation (graph queries especially -- recursive SQL over links) widens
the retrofit surface.

## Current state (recon, verified at `a1b701e`)

**Identity exists but is decorative.**
- `api_tokens` (`pgstore/tokens.go:69-77`): token_hash, name, scopes
  TEXT[], timestamps. No user binding. `VerifyResult{Name, Scopes}`.
- `httpapi/identity.go`: `Identity{Name, Scopes, Source}`, `HasScope`
  helper -- defined, never called for enforcement.
- Token admin is CLI-only (`cmd/memstore/admin_cmd.go`: issue-token,
  list-tokens, revoke-token, rotate-token) against the DB directly.
- Clients send a single bearer token (`httpclient/client.go:517`); it
  lives in config (`config.go`).

**Namespace is the only data scoping, and it is deployment-wide.**
- `memstore_facts` and `memstore_links` carry namespace columns with
  indexes (`pgstore/store.go:129-183`); every Store query filters on the
  construction-time namespace (documented invariant). memstored runs ONE
  store instance with ONE namespace for all requests
  (`cmd/memstored/main.go:57,103`).
- `SearchOpts.AllNamespaces` is exposed through the HTTP API verbatim
  (`httpapi/handler.go:503,524`).

**The session layer has no ownership axis at all.**
- `session_turns`, `session_hooks`, `context_hints`,
  `context_injections`, `context_feedback`
  (`pgstore/session_store.go:28-100`) key on session_id/cwd only. Two
  users' session_ids can collide; hints and feedback are globally
  readable/writable through their endpoints.

**Background workers run in the single store scope.**
- embedqueue polls `store.NeedingEmbedding` (`httpapi/embedqueue.go:73`);
  extractqueue inserts via the shared store and `FactExtractor`. Both
  inherit whatever scoping the store has.

**Global state.**
- `memstore_meta` (key/value, `pgstore/store.go:162`) holds the embedder
  fingerprint per database -- not per namespace or user.

## Design

### D1. Identity schema: adopt tier 3 Phase 0 as written

`docs/tier3-permissions.md` Phase 0 is adopted as the normative identity
design. In brief (the doc is authoritative; this is a summary):

- `memstore_users` (id, namespace, name, created_at; UNIQUE(namespace,
  name)). A user belongs to exactly one namespace.
- `api_tokens` gains `user_id BIGINT NOT NULL REFERENCES memstore_users
  ON DELETE RESTRICT`. Token names move to the `<user>@<host>` convention.
- `memstore_facts` gains `user_id` (NOT NULL after backfill, FK).
  **Delta from Phase 0 (maintainer decision, 2026-06-12): the speculative
  `group_id`/`role_id` slots are NOT shipped.** The sharing design may
  change before it is ever built; dead columns would codify a stale
  guess. If sharing is ever designed, it pays for its own migration
  against a then-current design.
- Backfill and default-user resolution exactly per the doc: sqlite uses
  the OS user; pgstore infers from token-name prefixes, erroring into
  `memstore admin tier3-init --default-user <name>` when inference is
  empty or ambiguous.
- The four scan sites (factColumns, scanFact, searchFTS's f.-list,
  ExportedFact/transfer) all gain the new columns. The conformance
  suite's InsertGetRoundTrip extends to pin user_id round-tripping --
  drift between scan sites is now CI-visible, which Phase 0 predates.

Two further deltas from the doc as written:

- `memstore_links` also gains `user_id` (NOT NULL, FK, backfilled like
  facts). Phase 0 scoped identity to facts; owner-only enforcement (D2)
  needs links owned too, and `LinkFacts` (`pgstore/store.go`) currently
  writes namespace-scoped rows whose source/target are not validated
  against the namespace -- the user predicate closes that hole at the
  same time.
- The backfill's subject rewrite frees ownership-marking subjects to
  **empty string, not NULL**. Phase 0's `subject = NULL` requires
  dropping the NOT NULL constraint, which on SQLite means a full table
  rebuild; `''` needs zero DDL on either backend, is the schema's
  existing "unset" convention (`kind`, `subsystem` default to `''`), and
  pg's generated FTS column already coalesces. Everything else about the
  rewrite rule (only `subject = <default user name>`, only categories
  outside identity/preference) holds as written.

### D2. Visibility: owner-only, everywhere

> **Rule: every read and every write path filters `user_id = <caller's
> user>`. There is no cross-user visibility of any kind.**

This is the maintainer's no-sharing decision applied as the entire
authorization model. Consequences:

- `docs/multi-user-data-model.md` (project principals, project tokens,
  shared project memory) is **superseded for v0.4.0**. The doc remains in
  the tree as a record of the discussion, with a status header saying it
  is not the current plan and that no schema provision is made for it.
- No roles, no grants, no ACL machinery. The only privileged distinction
  is the daemon's own internal service scope (D5) and the `admin` scope
  on tokens (D6).
- Cross-user reads of a known fact ID must be indistinguishable from the
  fact not existing (the same `not found` error), so IDs do not leak
  existence.

### D3. Enforcement mechanism: construction-time scoping (the namespace pattern)

Scoping lives where namespace scoping already lives: in the store
constructor, not sprinkled through handlers.

- `PostgresStore` gains a `userID int64` field. All queries add the
  `user_id` predicate through the same helpers that append the namespace
  filter today; inserts stamp it. A userID of 0 is invalid in scoped
  instances (see service scope, D5).
- A cheap derivation method -- `ForUser(userID int64) *PostgresStore` --
  returns a clone sharing the pool, embedder, query cache, and vecDim,
  *without re-running migrations* (constructor-runs-migrate is today's
  behavior, `pgstore/store.go:53`; the clone path must skip it).
- The `memstore.Store` interface is **unchanged**. httpapi reaches the
  per-user store via a small capability interface it already needs to
  define for the daemon path:

  ```go
  // UserScoper is implemented by backends that support per-user scoping.
  type UserScoper interface {
      ForUser(userID int64) memstore.Store
  }
  ```

  This is deliberately the same capability-interface pattern
  `docs/tier1-graph-basics.md` proposes for graph features: pgstore
  implements it, SQLite does not need to (see D8).
- `httpapi.Handler` resolves the scope once per request, immediately
  after auth: token -> `VerifyResult` (now carrying user_id) ->
  `Identity{UserID,...}` -> `store := h.scoped(identity)`. Handlers below
  that line are untouched -- they already take a store-shaped thing and
  never re-derive scope.
- `SessionStore` gets the same treatment (D4): per-user scoping at
  construction, `ForUser` clone.

What this buys: enforcement is decided in exactly one place per request,
the pattern is already proven by namespace, and a handler that forgets to
"check the user" *cannot exist* because handlers never see an unscoped
store.

### D4. Session layer: gains ownership

All five session tables gain `user_id BIGINT NOT NULL REFERENCES
memstore_users ON DELETE RESTRICT`, backfilled to the default user.
Uniqueness keys widen to include it (`UNIQUE(user_id, session_id, uuid)`
on turns; `UNIQUE(ref_id, ref_type, user_id, session_id)` on feedback;
etc.) so two users' Claude sessions with colliding session_ids cannot
interleave. Every SessionStore method filters on the scoped user. The
hint/recall/feedback endpoints inherit scoping from D3's per-request
resolution.

### D5. Background workers: explicit service scope

Embedding is user-agnostic (a fact needs a vector regardless of owner),
and the extract queue must write as the session's owner. Two rules:

- The daemon constructs ONE internal, unscoped "service" store at startup
  (current behavior, renamed and documented as privileged). It is handed
  ONLY to embedqueue and BackfillFeedback-style maintenance. It is never
  reachable from a request handler. `NeedingEmbedding`/`SetEmbedding`/
  `MarkEmbedFailed` run service-scoped across all users.
- extractqueue jobs gain the owning `user_id`, captured at enqueue time
  from the authenticated request that posted the session hook/transcript.
  The job's extraction, supersession search, summary persistence, hint
  generation, and A-MEM linking all run on `ForUser(job.UserID)` stores.

### D6. API and admin surface

- **Token verification** returns user_id; the legacy single
  `MEMSTORE_API_KEY` bootstrap token maps to the default user (Phase 0's
  backfill covers it). Existing deployments upgrade with zero client
  changes.
- **Token management**: stays CLI-first (`memstore admin issue-token`
  gains `--user`; names take the `<user>@<host>` shape per Phase 0).
  Self-service HTTP token endpoints are deferred to the web-UI work
  (`docs/web-ui-brief.md`) -- they are UI surface, not isolation surface.
- **`admin` scope**: the one privileged scope. Required for: issuing
  tokens for other users, per-user export of someone else's data, and
  any future cross-user maintenance endpoint. `Identity.HasScope`
  (`httpapi/identity.go:29`) finally gets call sites.
- **`all_namespaces` / `namespaces`** request fields remain. Under D1 a
  user belongs to one namespace, and under D3 every query carries the
  user predicate regardless -- so these fields can no longer cross a user
  boundary; they become user-internal organization, which is what
  namespace now means below the org level (per maintainer decision 2).
- **Export/Import**: API-level export is scoped to the caller. Whole-
  database export remains a CLI/DB-level admin operation.

### D7. Global state that stays global

`memstore_meta`'s embedder fingerprint stays per-database: the embedding
model is daemon configuration, shared by all users of a deployment. Two
users cannot use different models on one memstored -- accepted and
documented (an org that needs different models runs another deployment,
which is the organizational boundary by decision 2).

### D8. SQLite backend

Per Phase 0: SQLite gets the identity schema, the OS-user default, and
write-time stamping -- so transfer round-trips and the scan-site
invariants stay aligned across backends -- but no `ForUser` and no
enforcement predicates. A local single-process SQLite file is owned by
its OS user; its isolation mechanism is file permissions. The conformance
suite's user-isolation family therefore keys off the `UserScoper`
capability: backends that implement it get the full cross-user
invisibility battery; backends that do not, skip it (the same
capability-gated pattern the suite already uses for `SetSupersededBy`).

### Conformance contracts (the enforcement proof)

New `internal/conformance` subtest family, `UserIsolation`, gated on a
`NewStoreUser(t, namespace, userID)` factory option. Two users, same
namespace, then for EVERY read path: `Get` by the other user's fact ID
returns not-found (same error as nonexistent); `List`, `BySubject`,
`Exists`, `ActiveCount`, `Search`, `SearchBatch`, `SearchFTS`, `History`,
`ListSubsystems`, `GetLinks`/`GetLink`, `NeedingEmbedding` return only
the caller's rows; `Supersede`/`Confirm`/`Touch`/`Delete`/
`UpdateMetadata`/`LinkFacts` against the other user's IDs fail or no-op
without side effects. Plus: `History` chains cannot cross users even with
hand-forged `superseded_by` pointers (reuses the `SetSupersededBy`
escape hatch). The httpapi layer gets parallel tests: two tokens, every
endpoint, zero bleed -- including the session/hint/feedback endpoints.

## Open questions -- RESOLVED by the maintainer, 2026-06-12

1. **OIDC subject mapping**: later. Nothing ships now; when the web UI
   lands, `memstore_users` gains an `oidc_subject` mapping in its own
   migration, designed against the federation doc.
2. **`group_id`/`role_id` reservation slots**: NOT shipped. The sharing
   design might change; no speculative schema. (Diverges from Phase 0 as
   written -- see D1.)
3. **Per-user rate/abuse controls**: none. The answer to a misbehaving
   user is human: talk to them, or disable their account. Account
   disablement = revoke all of the user's tokens (`memstore admin
   disable-user <name>` ships in plan 013 as a convenience wrapping
   bulk revocation; no new schema -- a user with zero active tokens
   cannot reach the daemon).

## Implementation phasing (becomes plans 010-013 after this spec is approved)

| Plan | Scope | Key risk contained |
|------|-------|--------------------|
| 010 | Identity schema + migration + backfill + default-user CLI, both backends; scan-site updates; conformance round-trip of identity columns | The hot-table migration and the four scan sites |
| 011 | pgstore enforcement: userID scoping in every query, `ForUser` clone, links ownership; conformance `UserIsolation` family | The owner-only predicates themselves |
| 012a | Session-layer schema: `user_id` on the 5 session tables (idempotent `ADD COLUMN` + backfill to default user), scoped `SessionStore` with its own `ForUser` capability, session-isolation conformance/test battery. Inert for the single-user daemon. | The session migration + the second scoping surface |
| 012b | Request wiring: `Identity.UserID`, per-request scoped store + session store resolved at the auth boundary, queues (embed = ServiceScope, extract jobs carry owner), recall scoping, end-to-end two-token httpapi isolation tests | The privileged/service path and the live request boundary |
| 013 | Admin/CLI surface (`--user` token issuance, `user-add`, `disable-user`), docs (MIGRATING for v0.4.0, README de-warning, status headers on superseded design docs) | Operator experience and the upgrade story |

Order is strict: each plan's conformance/test additions gate the next.
CI's Postgres job runs the isolation battery on every PR from 011 on.

**Phase 3 split (decided during 012 recon, 2026-06-12):** the spec listed
"daemon wiring" as one plan (012). Recon against post-#87 main showed the
blast radius -- 26 store call sites, 18 session-store sites, 5 session
tables with no `user_id`, two queues, the recall path -- and this is the
phase where a missed predicate leaks across users on the live wire. So it
is split into 012a (session schema + scoped SessionStore, provable in
isolation like 010 was) and 012b (request wiring on top of a proven
session layer, like 011 was on top of 010). 012b depends on 012a.

## STOP conditions for the eventual executors (preview)

These will be elaborated per-plan, but the spec-level invariants are: any
path discovered where a request-handler-reachable store lacks the user
predicate is a stop-and-report, not a patch-in-place; any migration step
that cannot infer the default user halts with the documented CLI
instruction rather than guessing; and the `UserIsolation` conformance
family is never weakened to make a backend pass.

## Maintenance notes

- Tier 1 graph features land AFTER this: `GraphReader` methods get the
  user predicate in their recursive CTEs from day one, and the
  `UserScoper`/capability pattern is already established for them.
- The deferred search-by-vector capability (#85 follow-up) composes the
  same way.
- When the web UI brief is picked up, token self-service endpoints and
  OIDC mapping extend this model; nothing here should need reshaping.
