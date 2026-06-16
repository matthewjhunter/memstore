# Plan 010: Identity schema -- memstore_users, user_id columns, backfill, default-user CLI

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat a1b701e..HEAD -- store.go sqlite.go search.go transfer.go pgstore/ internal/conformance/ cmd/memstore/admin_cmd.go`
> If anything changed since this plan was written, compare the "Current
> state" excerpts against the live code before proceeding; on a mismatch,
> treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: L
- **Risk**: MED (hot-table migration + the four scan sites; no behavior
  change for reads, so existing tests are the safety net)
- **Depends on**: plans/009-multi-user-isolation-spec.md (approved spec; D1/D8 govern this plan)
- **Category**: security (phase 1 of 4 for v0.4.0 multi-user isolation)
- **Planned at**: commit `a1b701e` (main), 2026-06-12

## Why this matters

This is the schema phase of v0.4.0 user isolation (spec: plan 009). After
this plan: users are first-class rows, every fact and link records its
owner, tokens are bound to users, and all new writes are stamped -- but
reads are NOT yet filtered (that is plan 011) and the daemon still serves
a single identity (plan 012). The increment is deliberately inert: every
existing test must pass unchanged, because nothing about visibility moves
yet. What this plan must get exactly right is the migration -- it runs
once against real data.

The normative design is `docs/tier3-permissions.md` Phase 0, as amended by
plan 009 D1: NO `group_id`/`role_id` columns (maintainer decision -- no
speculative schema), `memstore_links` gains `user_id` too, and the subject
rewrite frees subjects to `''` (empty string), not NULL.

## Current state

- `pgstore/store.go:19` -- `const schemaVersion = 3`; `migrate()` at
  lines 79-127 runs versioned steps; V3 added quarantine columns
  (`pgstore/store.go:219`). Facts schema at 129-160 (namespace TEXT,
  subject TEXT NOT NULL, generated `fts` column coalescing subject);
  links schema at 167-183 (namespace column, no user).
- `sqlite.go:15` -- `const schemaVersion = 11`; `migrateVN()` pattern,
  V11 added `embed_failed_at`/`embed_error` (lines ~195-199).
- `pgstore/tokens.go:69-77` -- `api_tokens` (token_hash, name, scopes
  TEXT[], timestamps; no user binding). `TokenStore.migrate` is separate
  from the store migration and runs in `NewTokenStore`
  (`pgstore/tokens.go:62`).
- The four scan sites (CLAUDE.md invariant): `factColumns` + `scanFact`
  (root `sqlite.go:19` area and duplicated in `pgstore/store.go:1009`
  area), `searchFTS`'s `f.`-prefixed list (root `search.go:79+` and
  `pgstore/search.go:127+`), and `ExportedFact` + the transfer scan
  (`transfer.go:24-110`, sqlite-only, deliberately excludes embeddings).
- `memstore.Fact` (root `store.go`) has no user field. `Insert`/
  `InsertBatch` in both backends stamp namespace from the store, nothing
  else.
- Token admin CLI: `cmd/memstore/admin_cmd.go` (issue-token,
  list-tokens, revoke-token, rotate-token; `openTokenStore` at line 53).
- Conformance suite: `internal/conformance/conformance.go`,
  `InsertGetRoundTrip` pins field round-tripping; wired from
  `sqlite_test.go` and (env-gated) `pgstore/store_test.go`.
- `memstore_meta` (pg, `pgstore/store.go:162`) and the sqlite meta table
  are key/value stores already used for the embedder fingerprint.

Read `docs/tier3-permissions.md` sections "Phase 0 -- Identity schema",
"Backfill rule", "Default user resolution", and "Token-name convention"
in full before starting -- this plan implements them and only deviates
where it says so explicitly.

## Commands you will need

| Purpose | Command                                                        | Expected on success |
|---------|----------------------------------------------------------------|---------------------|
| Build   | `GOWORK=off go build ./...`                                    | exit 0              |
| Tests   | `GOWORK=off go test -race -count=1 ./...`                      | all packages ok     |
| Vet     | `GOWORK=off go vet ./...`                                      | exit 0              |
| Lint    | `GOWORK=off /home/matthew/go/bin/golangci-lint run ./...`      | 0 issues (if sandbox denies execution, say so; reviewer runs it) |
| Format  | `go fmt ./...`                                                 | no output           |
| PG tests| `MEMSTORE_TEST_PG=<dsn> GOWORK=off go test -race -count=1 ./pgstore/` | ok (reviewer runs if sandbox denies env var) |

## Scope

**In scope** (the only files you should modify):
- `store.go` (Fact struct gains UserID)
- `sqlite.go` (migrateV12, factColumns/scanFact, insert stamping, default-user resolution)
- `search.go` (searchFTS f.-list)
- `transfer.go` + `transfer_test.go` (ExportedFact carries user by NAME; import resolves/creates)
- `pgstore/store.go` (migrateV4, scan sites, insert stamping, InitIdentity)
- `pgstore/search.go` (f.-list)
- `pgstore/tokens.go` (user_id on Issue/Verify/List; IssueOpts gains UserID)
- `cmd/memstore/admin_cmd.go` (tier3-init subcommand; issue-token gains --user)
- `internal/conformance/conformance.go` (extend InsertGetRoundTrip)
- Corresponding test files (`sqlite_test.go`, `search_test.go`,
  `pgstore/store_test.go`, `pgstore/tokens_test.go`, `cmd/memstore/*_test.go`)

**Out of scope** (do NOT touch, even though they look related):
- ANY read-path filtering by user (`WHERE user_id = ...` on
  List/Search/Get/etc.) -- that is plan 011. This plan stamps writes and
  migrates schema only.
- `httpapi/` entirely -- per-request scoping, session tables, queues are
  plan 012. The daemon keeps working exactly as today.
- `ForUser` / `UserScoper` -- plan 011.
- `memory_*` MCP tool schemas -- no new tool inputs (spec D6: identity is
  transport-derived, never tool input).

## Git workflow

- Branch: `advisor/010-identity-schema` from `origin/main`
- Suggested commits: one per logical unit, e.g. `store: add Fact.UserID
  and memstore_users schema (sqlite V12)`, `pgstore: identity schema
  migration V4 with backfill`, `tokens: bind api_tokens to users`,
  `transfer: round-trip fact ownership by user name`,
  `cmd/memstore: tier3-init and --user on issue-token`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Fact gains UserID; both backends' schemas and scan sites

- `store.go`: add `UserID int64 \`json:"user_id,omitempty"\`` to `Fact`.
- sqlite `migrateV12` (bump `schemaVersion` to 12, wire in `migrate()`):
  1. `CREATE TABLE IF NOT EXISTS memstore_users (id INTEGER PRIMARY KEY
     AUTOINCREMENT, namespace TEXT NOT NULL, name TEXT NOT NULL,
     created_at TEXT NOT NULL DEFAULT (datetime('now')), UNIQUE(namespace, name))`
  2. `ALTER TABLE memstore_facts ADD COLUMN user_id INTEGER` (nullable in
     DDL -- SQLite cannot add NOT NULL without rebuild; enforcement is
     write-time stamping, per spec D8)
  3. `ALTER TABLE memstore_links ADD COLUMN user_id INTEGER`
  4. Backfill: resolve the OS user (`os/user.Current().Username`,
     lowercased), insert into memstore_users for the store's... NOTE:
     migration runs once per DATABASE, not per namespace -- create the
     user row with namespace = '' sentinel? NO: create one user row PER
     DISTINCT namespace present in memstore_facts (plus the store's own
     namespace if absent), same name, and backfill each namespace's rows
     to its user row. This keeps UNIQUE(namespace,name) meaningful and
     matches "a user belongs to exactly one namespace".
  5. Subject rewrite per Phase 0, amended: `UPDATE memstore_facts SET
     subject = '' WHERE subject = <default user name> AND category NOT IN
     ('identity','preference')` -- empty string, not NULL.
  6. Record `default_user` = the resolved name in the sqlite meta table.
- pg `migrateV4` (bump `schemaVersion` to 4): same shape with Postgres
  DDL -- memstore_users per the Phase 0 SQL (BIGSERIAL etc.), facts and
  links gain `user_id BIGINT`; backfill; then `ALTER ... SET NOT NULL`
  and FKs (`REFERENCES memstore_users(id) ON DELETE RESTRICT`); indexes
  `idx_memstore_facts_user (namespace, user_id)`,
  `idx_memstore_facts_user_subj (namespace, user_id, subject)`, and an
  equivalent on links. Subject rewrite to `''`. Record `default_user`
  in `memstore_meta`. Default-user resolution for pg is Step 3.
- Update ALL scan sites for the new column: root `factColumns` +
  `scanFact` (sqlite.go), `searchFTS` f.-list (search.go), pgstore's
  `factColumns`/`scanFact` (pgstore/store.go) and f.-list
  (pgstore/search.go). `Insert`/`InsertBatch` in both backends stamp
  `user_id` from the store's resolved default user (Step 2).

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go test -race -count=1 ./...` -> existing tests pass (scan
sites consistent or scans error loudly)

### Step 2: Store-level default user resolution and stamping

- `SQLiteStore`: after migration, resolve-or-create the OS user row for
  the store's namespace at construction; keep the id in a `userID` field;
  stamp it on every fact and link insert. Zero-config local story
  preserved (spec D8).
- `PostgresStore`: at construction (post-migration), read
  `memstore_meta['default_user']`, resolve the user row for the store's
  namespace (create if the namespace is new); keep `userID`; stamp
  inserts. This field is plumbing for plan 011's `ForUser`; in this plan
  it only feeds write stamping.

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> ok; a quick
manual check in a test: inserted fact's `Get` returns `UserID != 0`.

### Step 3: pg default-user inference + tier3-init CLI

Implement Phase 0's "Default user resolution" for pg exactly:

- In `migrateV4`, infer the default user from existing non-legacy
  `api_tokens.name` values split on the first hyphen. Unanimous prefix ->
  that name. Empty or ambiguous -> the migration FAILS with exactly the
  documented instruction: `tier 3 migration cannot infer default user;
  run 'memstore admin tier3-init --default-user <name>' before starting
  memstored`.
- Add `pgstore.InitIdentity(ctx, pool, defaultUser string) error`: runs
  the V4 migration with the user supplied explicitly (idempotent: no-op
  at version >= 4). `New()` keeps its signature.
- `cmd/memstore/admin_cmd.go`: new `tier3-init` subcommand calling
  InitIdentity (model flag/connection handling on `openTokenStore`,
  line 53).
- Fresh empty database (no tokens at all, nothing to infer): create the
  default user lazily is NOT allowed for pg (Phase 0: no implicit user at
  request time) -- but a fresh DB has no facts to backfill either, so:
  if api_tokens is empty AND memstore_facts is empty, migrate the schema
  with NO user rows; `PostgresStore` construction then requires that a
  default user exist OR creates one named from `memstore_meta` if
  tier3-init recorded it. If neither exists, construction fails with the
  tier3-init instruction. (Practical effect: new pg deployments run
  tier3-init once; existing single-user deployments migrate
  automatically via inference.)

**Verify**: `MEMSTORE_TEST_PG=<dsn> GOWORK=off go test -race -count=1
-run 'Migrat|Identity|Init' ./pgstore/` -> ok (or note for reviewer);
plus the unit tests in the test plan

### Step 4: Tokens bind to users

- `api_tokens` V4 changes live in `TokenStore.migrate`
  (`pgstore/tokens.go:66`): add `user_id BIGINT`; backfill per Phase 0's
  token-name parsing WITH the name rewrite (`matthew-laptop` ->
  `matthew@laptop`; bare `legacy` -> `<default>@legacy`; non-matching ->
  `<default>@<sanitized>` with a logged warning); then SET NOT NULL + FK
  + index. NOTE: TokenStore.migrate must therefore run AFTER the store's
  migrateV4 has created memstore_users -- enforce by checking the users
  table exists and erroring with a clear message if not (memstored
  constructs the store before the token store, `cmd/memstored/main.go:103`
  before `:165`, so the order holds in practice; the check is a guard).
- `IssueOpts` gains `UserID int64` (required); `Issue` stores it;
  `Verify`/`VerifyResult` gains `UserID`; `List` returns it.
- `issue-token` CLI gains `--user <name>` (resolves the user row,
  errors if absent -- user creation is `tier3-init`'s job in this plan;
  the full `user-add` surface is plan 013). New token names must match
  `<user>@<host>` shape; enforce at issuance.

**Verify**: `GOWORK=off go build ./...` -> exit 0; token tests pass
(`pgstore/tokens_test.go` -- update constructions for the new required
UserID; that is an allowed test change since the API deliberately grew)

### Step 5: Transfer round-trips ownership by name

`ExportedFact` gains `User string` (the owner's NAME, joined from
memstore_users -- ids do not transfer across databases). Export populates
it; Import resolves-or-creates the user by (namespace, name) and stamps
the id; an export file without the field (pre-V12) imports as the
target's default user. Update the transfer scan and `transfer_test.go`
round-trip assertions.

**Verify**: `GOWORK=off go test -race -count=1 -run Transfer ./` -> ok

### Step 6: Conformance + full suite

Extend `InsertGetRoundTrip` in `internal/conformance/conformance.go`:
inserted facts come back with `UserID != 0`, and two facts inserted via
the same store share the same UserID. (Cross-user isolation tests are
plan 011's `UserIsolation` family -- do NOT add them here; reads are not
filtered yet and they would fail.)

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> all ok;
`go fmt ./...` -> no output; lint per command table.

## Test plan

- Migration tests, both backends, modeled on existing migration tests
  (`TestNewSQLiteStore_TablesExist`, `sqlite_test.go:42`): fresh DB has
  memstore_users and user_id columns; a fixture DB built at V11/V3 with
  facts (including `subject = <user>` rows in identity, preference, and
  project categories), links, and hyphen-named tokens migrates with:
  correct user row, all facts/links backfilled, subject rewrite applied
  ONLY outside identity/preference (and to `''`, not NULL), token names
  rewritten to `@` shape, token user_id bound.
- pg inference: unanimous-prefix fixture migrates; ambiguous fixture
  fails with the documented message; `InitIdentity` then succeeds.
- Stamping: Insert and InsertBatch produce rows with the store's user id
  on both backends; LinkFacts stamps links.
- Transfer: export -> import into a fresh DB preserves owner by name;
  legacy export (no User field) imports as default user.
- Conformance: extended round-trip passes on sqlite locally and pg in CI.
- EVERYTHING that exists today must pass unmodified, with two sanctioned
  exceptions: token tests touching `Issue`'s grown signature, and any
  test asserting the exact factColumns/scan column count.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `grep -n "user_id" sqlite.go pgstore/store.go pgstore/search.go search.go transfer.go | wc -l` shows the column wired through all scan sites (executor lists the exact sites touched in the report)
- [ ] sqlite `schemaVersion` == 12; pgstore `schemaVersion` == 4
- [ ] Migration fixture tests exist and pass for both backends, including the subject-rewrite and token-rename assertions
- [ ] `memstore admin tier3-init --default-user x` exists (`go run ./cmd/memstore admin 2>&1` usage text lists it)
- [ ] Conformance InsertGetRoundTrip asserts UserID round-trip
- [ ] Lint clean (executor or reviewer)
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated (reviewer maintains if dispatched)

## STOP conditions

Stop and report back (do not improvise) if:

- Any existing test fails in a way that requires changing its assertions
  beyond the two sanctioned exceptions -- this plan must be behaviorally
  inert for reads.
- The sqlite nullable-user_id compromise (D8) breaks a NOT NULL
  assumption somewhere unexpected (e.g. a scan reading user_id as int64
  panics on NULL from a pre-backfill row path) -- report, do not bolt on
  COALESCE patches.
- The pg migration ordering between store.migrate (creates users) and
  TokenStore.migrate (needs users) cannot be guaranteed for some caller
  of NewTokenStore other than memstored -- enumerate the callers and
  report.
- Phase 0's doc contradicts this plan anywhere not covered by the three
  documented deltas (no group/role columns; links get user_id; subject
  rewrite to '') -- report the contradiction rather than picking a side.
- The backfill would modify more than one namespace's facts with
  different inferred users on pg -- the inference design assumes one;
  report what the fixture shows.

## Maintenance notes

- Plan 011 builds directly on this: the `userID` field added in Step 2
  becomes the scoping predicate and `ForUser` clone source.
- The four-scan-site invariant now covers user_id; the conformance
  round-trip is the regression net.
- Reviewers should scrutinize the migration fixtures hardest -- this is
  the one plan in the series that rewrites existing rows.
