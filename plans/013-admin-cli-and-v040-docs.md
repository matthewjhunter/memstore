# Plan 013: Admin CLI (user-add, disable-user) + flip the v0.4.0 docs to shipped

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 9cede52..HEAD -- cmd/memstore/ pgstore/tokens.go README.md docs/`
> Expected: empty. On a mismatch, compare "Current state" against live code;
> treat contradictions as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: LOW (additive CLI + docs; no isolation logic, no migration)
- **Depends on**: plans 010-012b (all merged: #86, #87, #88, #89)
- **Category**: dx + docs (phase 4 of 4 -- the final v0.4.0 phase)
- **Planned at**: commit `9cede52` (main), 2026-06-13

## Why this matters

Phases 1-3 made the daemon fully user-isolated. What's missing is the
operator surface to *use* multiple users and the documentation to tell
people it's safe: there's no command to create a user or disable one, and
the README still warns "single-user by deployment, don't run as a shared
service until v0.4.0." This plan adds the two missing admin commands
(`user-add`, `disable-user`) and flips the docs from "v0.4.0 promised" to
"v0.4.0 shipped." It introduces no isolation logic -- the boundary is done
and proven (#87/#88/#89); this is the handle and the label on it.

## Current state (post-#89)

- `cmd/memstore/admin_cmd.go` -- `runAdmin` dispatches subcommands
  (`admin_cmd.go:20-35`): `tier3-init`, `issue-token` (has `--user`),
  `list-tokens`, `revoke-token`, `rotate-token`. `printAdminUsage`
  (`:~45`) lists them. `openTokenStore(pgFlag)` (`:~53`) opens a pg pool +
  TokenStore. The commands connect directly to Postgres (run on the daemon
  host), NOT through the HTTP API.
- Token / user primitives:
  - `pgstore.EnsureUser(ctx, pool, namespace, name) (int64, error)`
    (`pgstore/store.go:146`) -- resolves OR CREATES a user row, returns id.
    Idempotent.
  - `TokenStore.Revoke(ctx, name string) (int, error)`
    (`pgstore/tokens.go:332`) -- revokes by token NAME (`UPDATE api_tokens
    SET revoked_at = now() WHERE name = $1 AND revoked_at IS NULL`).
    Returns count. There is NO revoke-by-user yet.
  - `issue-token --user <name>` resolves an existing user and errors with
    a tier3-init hint if absent (`admin_cmd.go:148`).
- `memstore_users (id, namespace, name)`; tokens carry `user_id` NOT NULL
  (#86). The daemon's namespace comes from the `--pg`/config; admin
  commands take a `--namespace` flag where relevant (mirror `issue-token`
  /`tier3-init`'s flag handling -- read them before writing).
- Docs needing the flip:
  - `README.md:12-15` -- the `> **⚠ Single-user notice (v0.3.0).**` block:
    "single-user by deployment, not by enforcement ... two tokens see the
    same facts. Don't deploy as a shared multi-user service until v0.4.0."
    This is now FALSE post-#89 -- it must be rewritten to describe the
    shipped isolation.
  - `docs/MIGRATING.md` -- records breaking changes + security caveats per
    release; needs a v0.3.0 -> v0.4.0 section.
  - `docs/tier3-permissions.md:3` -- `Status: Phase 0 design ready ...
    Phase 1 deferred`. Phase 0 (identity) is SHIPPED; the implementation
    diverged from the doc (no group/role columns -- owner-only model;
    `subject` rewritten to '' not NULL; links got user_id). Needs a
    status header noting shipped-in-v0.4.0 + the divergences.
  - `docs/multi-user-data-model.md:3` -- `Status: Proposed.` Plan 009 D2
    superseded this for v0.4.0 (owner-only, no project principals, no
    group/role schema). It needs the status header plan 009 specified:
    "not the current plan; v0.4.0 is owner-only with no schema provision
    for this; retained as a record of the discussion."
- The repo's voice for docs is the maintainer's (see `~/.claude/CLAUDE.md`
  conventions: plain ASCII punctuation, no LLM-tell phrasing, direct and
  concrete). Match the surrounding prose in each file.

## Design

### CLI additions (cmd/memstore/admin_cmd.go + one pgstore method)
1. `pgstore.TokenStore.RevokeByUser(ctx, userID int64) (int, error)` --
   `UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1 AND
   revoked_at IS NULL`; returns rows affected. Mirrors `Revoke`'s shape.
2. A resolve-only user lookup (do NOT use EnsureUser for disable -- it
   would create the user you're trying to disable). Add
   `pgstore.LookupUserID(ctx, pool, namespace, name) (int64, error)`
   returning a not-found error when absent (or reuse an existing internal
   resolver if one is exported-able without scope creep -- check for a
   resolveUser/resolveSessionUser-style helper first; if none is exported,
   add LookupUserID).
3. `admin user-add <name> [--namespace ns] [--pg dsn]` -- calls
   `EnsureUser`; prints the user name + id; idempotent (re-running prints
   the existing id). Wire into `runAdmin` switch + `printAdminUsage`.
4. `admin disable-user <name> [--namespace ns] [--pg dsn]` -- resolves the
   user via LookupUserID (error if absent), calls `RevokeByUser`, prints
   "disabled user <name>: revoked N token(s)". A user with zero active
   tokens cannot authenticate, which is the account-disable semantics from
   spec 009 (open question 3 resolution). Wire into switch + usage.
5. (Optional, include if cheap) `admin list-users [--namespace ns]` --
   `SELECT name, id FROM memstore_users WHERE namespace = $1 ORDER BY
   name`; tabular like `list-tokens`. Skip if it balloons the diff.

### Docs flip
6. `README.md` -- replace the v0.3.0 single-user warning block with an
   accurate v0.4.0 statement: memstored enforces per-user isolation
   end to end (every read/write filtered by the token's user; facts,
   links, sessions, hints, feedback all owned); a fresh pg deployment runs
   `memstore admin tier3-init --default-user <name>` once; existing
   single-user deployments migrate automatically (default user inferred
   from token names). Keep it tight and factual; match the README's voice.
7. `docs/MIGRATING.md` -- add a `## v0.3.0 -> v0.4.0` section: the breaking
   changes (token names now `<user>@<host>`; fresh pg deployments require
   `tier3-init`; `api_tokens`/facts/links/session tables gain `user_id`),
   the automatic-vs-manual upgrade paths (inference vs tier3-init), the new
   admin commands (`user-add`, `disable-user`, `issue-token --user`), and
   that isolation is now enforced (removing the v0.3.0 caveat).
8. `docs/tier3-permissions.md` -- update the Status header: Phase 0
   (identity) shipped in v0.4.0; note the divergences from the doc as
   built (owner-only model, NO group_id/role_id columns, subject rewritten
   to '' not NULL, links carry user_id). Do NOT rewrite the whole doc --
   a status header + a short "as-built divergences" note is enough.
9. `docs/multi-user-data-model.md` -- add the status header per plan 009
   D2: superseded for v0.4.0 (owner-only; no project principals; no
   group/role schema shipped); retained as a record of the discussion.

## Commands you will need

| Purpose | Command                                                        | Expected on success |
|---------|----------------------------------------------------------------|---------------------|
| Build   | `GOWORK=off go build ./...`                                    | exit 0              |
| Tests   | `GOWORK=off go test -race -count=1 ./...`                      | all packages ok (pg gated) |
| Vet     | `GOWORK=off go vet ./...`                                      | exit 0              |
| Lint    | `GOWORK=off /home/matthew/go/bin/golangci-lint run ./...`      | 0 issues (reviewer runs if denied) |
| Format  | `go fmt ./...`                                                 | no output           |
| Usage   | `GOWORK=off go run ./cmd/memstore admin 2>&1`                  | usage lists user-add + disable-user |

## Scope

**In scope** (the only files you should modify):
- `cmd/memstore/admin_cmd.go` (+ `cmd/memstore/admin_cmd_test.go` if it
  exists; else add tests where the existing admin tests live)
- `pgstore/tokens.go` (RevokeByUser)
- `pgstore/store.go` (LookupUserID, only if no exported resolver exists)
- `pgstore/tokens_test.go` / `pgstore/store_test.go` (tests for the new
  methods, pg-gated)
- `README.md`, `docs/MIGRATING.md`, `docs/tier3-permissions.md`,
  `docs/multi-user-data-model.md`

**Out of scope** (do NOT touch):
- Any isolation/scoping logic (done in #87/#88/#89) -- this plan adds no
  predicates and changes no request path.
- HTTP API endpoints -- admin stays CLI-only (self-service token
  endpoints are deferred to the web-UI work, spec D6).
- The migrations -- no schema change.
- Other docs (architecture.md, installation.md, the tier1/2/4 design docs,
  web-ui-brief, local-llm-features, training-data-design).

## Git workflow

- Branch: `advisor/013-admin-cli-docs` from `origin/main` (9cede52).
  Create it explicitly: `git checkout -b advisor/013-admin-cli-docs origin/main`
  (do NOT commit onto the worktree's auto-branch).
- Suggested commits: `pgstore: RevokeByUser + LookupUserID`,
  `cmd/memstore: admin user-add and disable-user`,
  `docs: flip README/MIGRATING/design-doc status to v0.4.0 shipped`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: pgstore methods
RevokeByUser + LookupUserID (design 1-2). **Verify**: `go build ./...` ->
exit 0.

### Step 2: CLI commands
user-add + disable-user (+ optional list-users), wired into runAdmin and
printAdminUsage (design 3-5). Model flag parsing and `openTokenStore`
usage on the existing `issue-token`/`revoke-token` handlers. **Verify**:
`go build ./...` -> exit 0; `go run ./cmd/memstore admin 2>&1` lists the
new commands.

### Step 3: Tests
pg-gated tests for RevokeByUser (issue two tokens for a user, revoke by
user, both inactive; a second user's token untouched) and LookupUserID
(found / not-found). If the admin commands have a testable seam (they
print to an io.Writer -- `runIssueToken(args, out)`), add a CLI-level test
for user-add idempotency and disable-user count, modeled on any existing
admin command test. **Verify**: `go test -race ./...` -> ok (pg parts skip
without MEMSTORE_TEST_PG; reviewer runs them).

### Step 4: Docs flip
Design 6-9. Match each file's voice; plain ASCII punctuation; no LLM-tell
phrasing (the maintainer is strict about this -- no "seamless",
"robust" as filler, em-dash glyphs, etc.; use `--` for em-dashes). The
README change is the load-bearing one: a reader must come away knowing the
daemon is safe to run multi-user. **Verify**: `grep -n "until v0.4.0"
README.md` -> no matches (the old warning is gone); `grep -rn "v0.4.0"
docs/MIGRATING.md` -> the new section exists.

### Step 5: Full suite + lint
**Verify**: `go test -race -count=1 ./...` -> all ok; `go fmt ./...` ->
no output; lint per table.

## Test plan

- `pgstore`: `TestRevokeByUser` (two tokens for user A both revoked; user
  B's token still active -- proves it's scoped by user_id, not global),
  `TestLookupUserID` (existing user returns id; missing returns a
  not-found error).
- CLI: if an admin-command test harness exists, `disable-user` on a user
  with N tokens prints/returns N and leaves them revoked; `user-add` twice
  is idempotent (same id). If no harness exists and adding one is
  disproportionate, cover the logic via the pgstore method tests and note
  it.
- Existing tests pass unmodified (this plan is purely additive to code).

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `go run ./cmd/memstore admin 2>&1` usage lists `user-add` and `disable-user`
- [ ] `grep -n "RevokeByUser" pgstore/tokens.go` returns a match
- [ ] `grep -n "until v0.4.0" README.md` returns NO matches (old warning removed)
- [ ] `docs/MIGRATING.md` has a v0.3.0 -> v0.4.0 section (grep `v0.4.0`)
- [ ] `tier3-permissions.md` and `multi-user-data-model.md` status headers updated
- [ ] No LLM-tell phrasing or non-ASCII punctuation introduced in the docs (reviewer reads)
- [ ] Lint clean (executor or reviewer)
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated (reviewer maintains if dispatched)

## STOP conditions

Stop and report back (do not improvise) if:

- An existing exported resolver already does LookupUserID's job under a
  different name -- use it instead of adding a duplicate; report which.
- `disable-user` would need to touch anything beyond revoking tokens to
  actually lock the account out (it should not -- zero active tokens = no
  auth; if you find a path that authenticates without an active token,
  STOP, that's a security finding).
- A doc you're editing contradicts the as-built behavior in a way this
  plan's notes don't cover -- report rather than guessing at the truth.
- The admin commands have no existing test seam and building one would
  require restructuring runAdmin -- report; cover via pgstore tests and
  leave the CLI seam for a follow-up.

## Maintenance notes

- This completes v0.4.0 multi-user isolation. After it lands, the
  README/MIGRATING/design-doc state matches the code.
- Self-service token endpoints over HTTP and OIDC subject mapping are the
  next user-facing layer (web-ui-brief) -- explicitly out of v0.4.0.
- The follow-up test-robustness finding (concurrency-safe migrations +
  uniform per-package test-DB helper, recorded in plans/README.md) is
  independent of this plan.
- Reviewer: read the README and MIGRATING prose for voice and accuracy --
  these are the user-facing face of the whole v0.4.0 effort, and the
  maintainer cares about voice. Verify no "until v0.4.0" / single-user
  caveat survives anywhere it would now be false.
