# Tier 3 — Identity and Permissions

Status: Phase 0 (identity schema) SHIPPED in v0.4.0. Phase 1 (group/role
permission predicates) not built -- v0.4.0 is owner-only (see below).
Author: Matthew + Claude
Date: 2026-05-07 (supersedes 2026-05-05 placeholder)

> **As-built note (v0.4.0).** Phase 0 shipped, with deliberate divergences
> from this design:
> - **Owner-only, no groups or roles.** v0.4.0 enforces strict per-user
>   isolation: a fact is visible only to its owning user, full stop. The
>   `group_id` / `role_id` columns this doc reserved were NOT added -- the
>   sharing model may change before it is built, so no speculative schema
>   was shipped. If sharing is ever designed, it pays for its own migration.
> - **`subject` freed to empty string, not NULL.** The backfill rewrites an
>   ownership-marking `subject` to `''` (the schema's existing "unset"
>   convention) rather than NULL, avoiding a NOT NULL constraint drop.
> - **Links carry `user_id` too.** Enforcement needed `memstore_links` owned,
>   not just `memstore_facts`.
>
> [`docs/multi-user-data-model.md`](multi-user-data-model.md) (the Phase 1
> project-principal design) is superseded for v0.4.0 and retained only as a
> record of the discussion.

Tier 3 introduces multi-user access control to memstore. It splits into two
phases:

- **Phase 0 (this design):** formalize identity in the schema. Today the
  `subject` column is overloaded — it carries both the owning user
  (`subject=matthew`) and the topic of the fact (`subject=memstore`,
  `subject=jane-austen`). Phase 0 makes user identity its own first-class
  field, leaves `group`/`role` slots nullable for Phase 1, wires caller
  identity through the MCP layer, and rescopes `subject` to mean topic only.
- **Phase 1 (deferred):** permission predicates. With identity formalized,
  add the `WHERE fact_visible_to($caller, fact_id)` machinery that tier 1's
  graph layer is already shaped to accept (predicate-shaped recursive CTEs,
  `*Caller` parameter, in-engine-only filtering, no topology leaks). Detailed
  design deferred until Phase 0 ships and real multi-user usage materializes.

This doc covers Phase 0 in full and ends with a one-section stub for Phase 1.

## Phase 0 — Identity schema

### Goals

- Replace ad hoc `subject=<person>` ownership convention with a typed
  `user_id` FK to a `users` table.
- Add `group_id` and `role_id` slots (nullable) so Phase 1 can wire
  predicates against them without another schema migration.
- Make caller identity automatic at the MCP layer — bearer tokens already
  identify a token; tokens now identify a user.
- Free `subject` to mean "topic of the fact" only.
- Unblock task 3135 (labeled-prefix embed-text), whose prefix labels need to
  map 1:1 to schema field names.

### Non-goals (Phase 0)

- Permission predicates, visibility functions, row-level security. All
  Phase 1.
- Group / role table design. The columns ship nullable; the tables that
  back them are designed in Phase 1.
- Cross-user fact sharing or grants. Phase 1.
- Cross-namespace traversal. Out of scope across the whole tier.
- A web UI or self-service signup. Users are admin-provisioned via CLI.

### Background

Existing pieces that Phase 0 builds on:

- **`memstore_facts`** (`pgstore/store.go:149-162` schema, `factColumns` /
  `scanFact` cross-cut per invariant id=599) — has `subject TEXT NOT NULL`,
  `category TEXT`, `namespace TEXT NOT NULL`, no notion of owning user.
- **`api_tokens`** (`pgstore/tokens.go:69-78`, decision id=2491) — bearer
  tokens with a `name` field per (human, device), e.g. `matthew-laptop`,
  `matthew-zero`. No user FK; `name` carries identity by hyphen-splitting
  convention only. Phase 0 retires the hyphen convention in favor of an
  email-address shape (see *Token-name convention* below).
- **`httpapi.Identity`** (`cmd/memstored/main.go:236-242`) — already exists
  on the request layer with `Name`, `Scopes`, `Source`. Just doesn't
  propagate into facts.
- **`namespace`** — set at store construction
  (`pgstore.New(..., namespace, ...)`). Phase 0 keeps namespace as outer
  deployment scope; users live within a namespace.

### Scope

| Change                                               | Layer        |
|------------------------------------------------------|--------------|
| `users` table                                        | Schema       |
| `api_tokens.user_id` FK                              | Schema       |
| `memstore_facts` adds `user_id`, `group_id`, `role_id` | Schema     |
| `memstore_facts.subject` becomes nullable            | Schema       |
| Backfill rule for existing facts                     | Migration    |
| `memstore admin user-add`, `--user` on issue-token   | CLI          |
| Caller identity propagation into Store calls         | httpapi/mcp  |
| `memory_search` / `memory_list` filter on user       | MCP/Store    |

### Schema

#### `memstore_users`

```sql
CREATE TABLE memstore_users (
    id          BIGSERIAL   PRIMARY KEY,
    namespace   TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, name)
);
CREATE INDEX idx_memstore_users_namespace ON memstore_users (namespace);
```

`name` is the canonical user identifier within a namespace (e.g. `matthew`,
`alice`). Lowercase by convention, mirroring subject naming (id=882). The
namespace column denormalizes the deployment-scope concept onto users —
a user belongs to exactly one namespace.

`group`/`role` tables are not added in Phase 0 — only the FK columns on
facts. Their schema is Phase 1.

#### `api_tokens` gains `user_id`

```sql
ALTER TABLE api_tokens ADD COLUMN user_id BIGINT;
-- backfill (see Migration sequence below)
ALTER TABLE api_tokens
    ALTER COLUMN user_id SET NOT NULL,
    ADD CONSTRAINT api_tokens_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;
CREATE INDEX idx_api_tokens_user ON api_tokens (user_id);
```

Token `name` is now constrained to an email-address shape (see
*Token-name convention* below). `(user_id, name)` is unique within a
namespace; the `@`-separator means two users can both have a `laptop`-host
token (`matthew@laptop` and `alice@laptop` are distinct names) without
needing per-user namespacing on top.

#### `memstore_facts` gains identity columns

```sql
ALTER TABLE memstore_facts
    ADD COLUMN user_id  BIGINT,
    ADD COLUMN group_id BIGINT,
    ADD COLUMN role_id  BIGINT;
-- backfill (see Migration sequence)
ALTER TABLE memstore_facts
    ALTER COLUMN user_id SET NOT NULL,
    ADD CONSTRAINT memstore_facts_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT,
    ALTER COLUMN subject DROP NOT NULL;
CREATE INDEX idx_memstore_facts_user      ON memstore_facts (namespace, user_id);
CREATE INDEX idx_memstore_facts_user_subj ON memstore_facts (namespace, user_id, subject);
```

`group_id` and `role_id` ship nullable with no FK — Phase 1 adds the FKs
once the target tables exist. Adding the columns now means Phase 1 doesn't
need another schema migration on the hot facts table.

`subject` becoming nullable is required by the backfill rule (some facts
will lose their subject value entirely — see below). Search and FTS code
must handle NULL subject gracefully.

#### Cross-cutting invariants (id=599, id=3111)

A new column on `memstore_facts` triggers four mandatory updates:

1. `factColumns` — add `user_id`, `group_id`, `role_id`, and the
   `subject` change is type-only (still listed).
2. `scanFact` — add three pointer destinations.
3. `searchFTS` — the `f.`-prefixed column list needs the same additions.
4. `ExportedFact` + transfer scan — round-trip must preserve identity.

FTS index: `user_id`/`group_id`/`role_id` are integer FKs, not text — they
do not feed the `tsvector`. The `users.name` text is searchable through
join, not denormalized into the FTS column. (A future "show me alice's
facts about X" query goes through `user_id = (SELECT id FROM memstore_users
WHERE name='alice')`, not FTS.)

### Backfill rule

Existing facts are single-user (Matthew's). The migration creates one user
row for him and assigns every fact to it, with one transformation:

1. Resolve the default user (per backend, see *Default user resolution*
   below). Insert `(namespace, name)` into `memstore_users`.
2. `UPDATE memstore_facts SET user_id = $defaultUser` for every row in the
   namespace.
3. **Subject rewrite:**
   ```sql
   UPDATE memstore_facts
       SET subject = NULL
       WHERE subject = 'matthew'
         AND category NOT IN ('identity', 'preference');
   ```
   Facts where the subject was `matthew` but the category is `identity` or
   `preference` keep `subject='matthew'` because those facts are *about*
   Matthew (the topic IS him). Other categories used `subject=matthew` only
   to mark ownership; that role is now the `user_id` column, so `subject`
   is freed.
4. Topical-subject facts (`memstore`, `home-server`, `jane-austen`, etc.)
   are unchanged — subject already meant topic for them.

For `api_tokens` backfill, parse the existing hyphen-shaped `name` on the
first hyphen, then **rewrite** it to the new email shape `<user>@<device>`:
- `matthew-laptop` → user `matthew`, name rewritten to `matthew@laptop`.
- `matthew-zero` → user `matthew`, name rewritten to `matthew@zero`.
- `legacy` (the bootstrap token, no hyphen) → default user, name rewritten
  to `<default-user>@legacy` so the constraint holds.
- Any token whose name doesn't match `<user>-<device>` and isn't `legacy`
  → log a warning, assign to default user, name rewritten to
  `<default-user>@<sanitized-original>`.

The rewrite is a one-time, in-migration `UPDATE`. Post-migration, all
token names match the email constraint enforced at issuance.

### Default user resolution

The two backends resolve the default user differently because their
identity stories differ:

- **sqlite** (local, single-process): the migration uses
  `os/user.Current().Username` as the default user name. Sqlite has no
  bearer-token concept; the OS user IS the identity. On a fresh sqlite DB
  with no facts, the same OS user is seeded into `memstore_users` so the
  first insert has somewhere to point.
- **pgstore** (memstored, multi-user-capable): the migration *infers* the
  default user from the existing `api_tokens` table. Parse every non-legacy
  token name on the first hyphen (the pre-Phase-0 convention), collect the
  prefixes:
  - Unanimous prefix (e.g. all tokens are `matthew-*`) → that's the default
    user. Existing tokens get backfilled to it (and rewritten to the email
    shape, see *Token-name convention*); existing facts likewise.
  - Empty (only the `legacy` bootstrap token, or no tokens at all) →
    migration errors with: *"Tier 3 migration cannot infer default user.
    Run `memstore admin tier3-init --default-user <name>` before starting
    memstored."* The CLI command runs the same migration with the user
    supplied explicitly.
  - Ambiguous (multiple distinct prefixes) → same error; operator picks
    one to be the default and reassigns others post-migration with
    `memstore admin user-reassign-tokens`.

  No environment variable. The pgstore default-user case is rare and the
  CLI is the explicit surface; env-var-driven config in this path would
  encourage stale configuration and silent assignment.

### Token-name convention

Token names are constrained to an email-address shape: `<user>@<host>`.
The hyphen-splitting convention used pre-Phase 0 (`matthew-laptop`) is
retired because:
- Hyphens appear in legitimate user names (`mary-jane`, `el-amin`) and
  hostnames (`docker-memstore`, `home-server-1`); splitting on the first
  hyphen breaks for both. `matthew-jay-laptop` is unambiguously
  `matthew-jay@laptop` under the new convention; under the old it was a
  judgment call.
- `@` is not a legal character in either user names or hostnames per the
  conventions we already use, so it's an unambiguous separator.
- Email-shaped identifiers carry intuitive management semantics: per-user
  search, per-user revocation, password-reset-style flows ("revoke all
  tokens for `matthew@*`") map cleanly.

**Validation at issuance:**
- Must contain exactly one `@`.
- Local-part: lowercase letters, digits, `.`, `-`, `_`; 1-64 chars.
  (Subset of RFC 5322; no quoted-string nonsense, no `+` tags — keeping
  the surface small.)
- Host-part: lowercase letters, digits, `.`, `-`; 1-253 chars. Does not
  need to resolve in DNS — it is a scope label.
- Local-part MUST equal an existing `memstore_users.name`. Issuance fails
  if the user doesn't exist; create it first with `memstore admin
  user-add`.

**No `--user` flag at issuance.** `memstore admin issue-token --name
matthew@laptop` infers the user from the local-part. Adding a separate
`--user` flag would create the silent-misroute hazard the email shape is
meant to prevent (typo in `--name`'s local-part vs. `--user` argument).
Single source of truth is the `--name`'s local-part, validated against
the users table.

### MCP surface

**No new params on write/update/search tools.** User identity is derived
from the transport, never from tool input. Per backend:

| Transport                                  | Identity source                          | Anonymous allowed |
|--------------------------------------------|------------------------------------------|-------------------|
| memstored (HTTP / mTLS)                    | bearer token → `api_tokens.user_id`      | No — request rejected with 401 |
| memstore-mcp local stdio against sqlite    | `os/user.Current()`                      | N/A — there is no remote caller |

memstored explicitly does not have a default-user fallback at request
time. An untokened or unverifiable request is rejected before it reaches a
handler — same shape as today, just now the rejection is total (no legacy
"single-key" implicit-user mode after Phase 0). Token verification already
loads `api_tokens.user_id` via the existing TokenStore.Verify path.

The sqlite path keeps the zero-ceremony local-dev story: no token, no
config, just `os/user.Current()`. A `memstore_users` row is auto-created
on first use if missing.

Tool semantics in both modes:
- `memory_store`, `memory_store_batch`, `memory_update` — inserted facts
  get `user_id = caller.UserID`. No `user` field in tool input.
- `memory_search`, `memory_list` — implicit `WHERE user_id = caller.UserID`
  on every query in Phase 0. (Phase 1 generalizes this via permission
  predicates that may include other users' visible facts.)
- `memory_get` (lookup by id) — returns the fact only if its `user_id`
  matches `caller.UserID`. Mismatched id returns the same "not found"
  shape as nonexistent ids — no error, no signal that the id exists.
  (Topology rule per the original placeholder: errors leak signal.)

`group_id` / `role_id` are not exposed on the MCP surface in Phase 0 —
they are write-only schema slots until Phase 1 lands.

### CLI surface

```
memstore admin user-add <name>             # create a user in the namespace
memstore admin user-list                   # list users + token counts
memstore admin issue-token --name <user>@<host>
                                            # user inferred from local-part;
                                            # fails if user doesn't exist
memstore admin list-tokens [--user <name>]  # filter on local-part
memstore admin revoke-token <user>@<host>   # revoke one specific token
memstore admin revoke-token --user <name>   # revoke ALL tokens for a user
                                            # (key-reset style flow)
```

`memstore admin user-add` is the only way to grow users in Phase 0 — no
MCP-layer self-signup. `revoke-token --user <name>` is the password-reset
analogue: invalidates every device token for that user in one call,
forcing reissuance.

### httpapi.Identity changes

`Identity` gains a `UserID int64` field, populated by the token verifier
from the joined `api_tokens.user_id`. The handler context — already
populated via `IdentityFromContext` per id=2491 — becomes the source of
truth for the user_id stamped onto inserts.

```go
type Identity struct {
    Name   string  // token device label, unchanged
    Scopes []string
    Source string  // "bearer" | "mtls"  -- "legacy" retired in Phase 0
    UserID int64   // NEW: resolved user, required (non-zero on every request)
}
```

The httpapi rejects any request whose token can't be resolved to a
non-zero `UserID`. This includes the legacy single-key bootstrap path —
that path is removed in Phase 0. Existing legacy tokens are migrated to
the default user during Phase 0's migration; thereafter every token has a
`user_id`. The `EnsureLegacyToken` bootstrap on daemon start is removed
outright — there is no implicit-token mode after Phase 0.

memstored starts fine with an empty `api_tokens` table. It logs a
prominent warning at startup ("WARNING: api_tokens is empty — no caller
will be able to authenticate. Use `memstore admin issue-token` to add
tokens; the daemon will pick them up on the next request without restart.")
and serves traffic — every request just 401s until a token exists. This is
the right shape because `memstore admin` writes directly to the DB
(decision id=2491, "trust model: has shell on daemon host == admin"), so
the operator can add tokens at any time and the daemon sees them
immediately via the existing TokenStore.Verify path. Refusing to start
would create a hostile UX with no security benefit.

There is no stdio path through httpapi. Stdio MCP runs in-process against
sqlite via the memstore-mcp binary directly; it never calls memstored.
Identity for that path is set at the binary's startup using
`os/user.Current()` and lives in the local `memstore_users` table —
parallel mechanism, not the same code path.

### Migration sequence

Schema versions: sqlite V11, pgstore V3 (per id=3111). One migration per
backend. Pgstore order:

```sql
-- 1. users table
CREATE TABLE memstore_users (...);

-- 2. seed default user (resolved per "Default user resolution":
--    sqlite uses os/user, pgstore infers from token-name prefixes
--    and errors out if ambiguous)
INSERT INTO memstore_users (namespace, name)
    VALUES ($namespace, $default_user);

-- 3. tokens get nullable user_id, then backfill, then rewrite name
--    to the email shape, then NOT NULL + FK
ALTER TABLE api_tokens ADD COLUMN user_id BIGINT;
UPDATE api_tokens SET user_id = (
    SELECT id FROM memstore_users
    WHERE namespace = $namespace
      AND name = COALESCE(
          NULLIF(split_part(api_tokens.name, '-', 1), 'legacy'),
          $default_user)
);
-- Rewrite token names: matthew-laptop -> matthew@laptop;
-- legacy -> $default_user@legacy.
UPDATE api_tokens
    SET name = CASE
        WHEN name = 'legacy' THEN $default_user || '@legacy'
        WHEN position('-' in name) > 0
            THEN split_part(name, '-', 1) || '@' || substring(name from position('-' in name) + 1)
        ELSE $default_user || '@' || name
    END;
-- (Pre-step in Go, not SQL: parse all non-legacy api_tokens.name values
--  before this migration runs and either confirm a unanimous user prefix
--  or fail with the tier3-init CLI suggestion. Also add a CHECK constraint
--  on api_tokens.name validating the email shape after the rewrite.)
ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_name_email_shape
        CHECK (name ~ '^[a-z0-9._-]{1,64}@[a-z0-9.-]{1,253}$');
ALTER TABLE api_tokens
    ALTER COLUMN user_id SET NOT NULL,
    ADD CONSTRAINT api_tokens_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT;

-- 4. facts get identity columns
ALTER TABLE memstore_facts
    ADD COLUMN user_id  BIGINT,
    ADD COLUMN group_id BIGINT,
    ADD COLUMN role_id  BIGINT;

-- 5. backfill all facts to default user
UPDATE memstore_facts SET user_id = (
    SELECT id FROM memstore_users
    WHERE namespace = memstore_facts.namespace AND name = $default_user);

-- 6. subject rewrite per the backfill rule
UPDATE memstore_facts SET subject = NULL
    WHERE subject = $default_user
      AND category NOT IN ('identity', 'preference');

-- 7. enforce, drop NOT NULL on subject, FK
ALTER TABLE memstore_facts
    ALTER COLUMN user_id SET NOT NULL,
    ADD CONSTRAINT memstore_facts_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT,
    ALTER COLUMN subject DROP NOT NULL;
CREATE INDEX idx_memstore_facts_user      ON memstore_facts (namespace, user_id);
CREATE INDEX idx_memstore_facts_user_subj ON memstore_facts (namespace, user_id, subject);
```

The migration is wrapped in a transaction. Standard `ALTER TABLE` taking a
brief write lock is acceptable — memstore is personal infrastructure and
the migration runs once per deployment (matches the tier 1 V3 GIN-index
deployment decision).

### Test plan

Tests in `pgstore/store_test.go` (new section), `pgstore/tokens_test.go`,
and `mcpserver/server_test.go`.

Migration tests:
1. Fresh DB: migration runs, default user created, no facts.
2. DB with one legacy fact corpus: backfill assigns all to default user;
   `subject='matthew'` rewritten to NULL only for non-identity, non-preference
   categories.
3. DB with three tokens (`matthew-laptop`, `matthew-zero`, `legacy`):
   backfill resolves all to the same user; the `legacy` token routes via
   default-user fallback.

Runtime tests:
4. Insert via MCP with token A: fact has `user_id` = A's user. Read by
   token B (different user): not visible.
5. Search by token A: results filter to A's user_id. Inserting "Matthew
   prefers terse responses" via stdio (OS user `matthew`) is then visible
   from `matthew-laptop` token but not `alice-laptop`.
6. Subject rewrite parity: a fact with `category='identity'`,
   `subject='matthew'` retains `subject='matthew'` post-migration; a fact
   with `category='note'`, `subject='matthew'` has `subject IS NULL`.
7. Transfer round-trip preserves user_id, group_id, role_id, and the
   topical subject value (where present).
8. CLI: `memstore admin user-add alice && memstore admin issue-token --user
   alice --name laptop` succeeds; the issued token verifies with
   `Identity.UserID = alice.ID`.
9. Sqlite stdio caller: first run as a never-seen OS user auto-creates
   the `memstore_users` row in the local sqlite DB.
10. memstored rejects untokened request with 401; rejects token whose
    `user_id` is NULL (shouldn't happen post-migration, but the verifier
    must enforce this invariant defensively).
11. memstored startup with empty `api_tokens`: starts cleanly, logs the
    "no tokens" warning, accepts connections, 401s every request. After
    `memstore admin issue-token` runs against the DB, the next request
    with that token verifies successfully — no daemon restart needed.
12. Pgstore migration with ambiguous token prefixes (`matthew-laptop` +
    `alice-laptop` present): migration errors before applying any DDL,
    points the operator at `memstore admin tier3-init --default-user`.
13. Token-name validation at issuance: `memstore admin issue-token --name
    matthew@laptop` succeeds; `--name matthew_laptop` (no `@`),
    `--name matthew@@laptop` (two `@`s), `--name matthew@LAPTOP` (uppercase
    rejected by the CHECK constraint regex), and `--name nobody@laptop`
    (no `nobody` user in `memstore_users`) all fail with distinct
    error messages.
14. Migration name-rewrite: pre-migration tokens `matthew-laptop` and
    `legacy` end up as `matthew@laptop` and `matthew@legacy` (assuming
    `matthew` resolved as default). The CHECK constraint applies cleanly
    after the UPDATE.
15. `revoke-token --user matthew` revokes all of matthew's device tokens
    in one call; subsequent verification of any of them returns the same
    "unknown token" shape as a never-existed token.

### Out of scope, deferred to Phase 1

- Permission predicates (`fact_visible_to`).
- `group_id` / `role_id` source tables and FK targets.
- Cross-user fact visibility and grants.
- Per-fact ACLs (vs. per-user-default visibility).
- Permission-aware graph traversal (the tier 1 hooks already exist —
  Phase 1 wires them).

## Phase 1 — Permission predicates (deferred)

Out of scope for now. Detailed design starts when Phase 0 has been in
production long enough that real multi-user access patterns inform what
visibility shape is actually wanted.

What's already on file from the original placeholder, retained as design
constraints for Phase 1:

- *User-driven filters* (subject, category) are caller's choice, fine to
  post-filter. *Permission filters* are mandatory, in-engine, never
  post-filter — counts, IDs, edge topology, error-vs-empty all leak signal.
- Tier 1 graph operations are pre-shaped: predicate-form recursive CTEs,
  `*Caller` parameter on handlers, the rule that invisible facts are
  treated as nonexistent (a path through an invisible node is "no path",
  not "redacted path"). Phase 1 fills in the `WHERE fact_visible_to(...)`
  predicate and wires caller identity through the existing `*Caller` slot.
- For graph operations specifically: invisible seeds in `Subgraph` are
  dropped silently (erroring confirms ID existence); `Degree` counts only
  visible-edge endpoints; `ShortestPath` returns "no path" rather than a
  redacted path.
- The visibility predicate's implementation shape (SQL function vs.
  CTE-injected ID list vs. row-level security) is the central Phase 1
  decision and is deferred until then.

---

**Writeup reminder:** when Phase 0 ships, write a homepage/blog post
covering the schema split (subject overloading → user/group/role + topic),
the backfill rule and what it means for category=identity vs other
categories, the implicit-identity-from-token MCP design, and how this
unblocks tier 3 Phase 1 plus task 3135 (labeled-prefix embeddings).
A second post for Phase 1 covers the threat model, visibility predicate,
graph integration, and caller-identity sourcing across MCP transports.
