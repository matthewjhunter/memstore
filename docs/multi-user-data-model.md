# Multi-User Data Model — Users, Projects, and Tokens

Status: Proposed. Concretizes tier 3 Phase 1 (permission predicates), which
[`tier3-permissions.md`](tier3-permissions.md) deliberately left as a stub.
Author: Matthew + Claude
Date: 2026-05-26

This is a design *proposal*, captured from a working discussion. It builds
directly on tier 3 Phase 0 (the identity schema, already designed) and fills
in the access-control model that Phase 0 reserved schema slots for. Several
choices here diverge from assumptions baked into the Phase 0 doc; those are
called out explicitly in [Divergences from tier 3](#divergences-from-tier-3)
and the open questions, rather than silently overriding them.

---

## 1. Where we are today

memstore's only access control is binary. An API key proves you are *allowed
to talk to the store* — nothing finer. There is one logical tenant; everyone
holding a valid token sees everything in the namespace.

Internally we already model richer concepts, but only as *content*, not as
*access boundaries*:

- **Users** — facts about a person (preferences, toolchains, identity).
- **Projects** — facts about a body of work, conventionally keyed by repo
  name in `subject` (`ProjectNameFromCWD`, `session.go:17`).
- **Repos** and **connections between repos** — modeled as facts and as graph
  links (`memstore_links`).

tier 3 Phase 0 took the first structural step: it promotes the owning user
from an overloaded `subject` convention to a typed `user_id` FK, and — this is
the part this doc depends on — it ships **`group_id` and `role_id` columns on
`memstore_facts`, nullable, with no FK yet** (`tier3-permissions.md`, "Schema").
Those slots exist precisely so the access model below can land without another
migration on the hot facts table.

## 2. The proposal in one paragraph

Split credentials from identity. A **user** is a first-class principal with
self-managed API keys that prove *who* the caller is. A **project** is a
second principal with its own token that proves *authorization to a body of
shared memory* — a capability, distributable with the repo it belongs to.
Every memory is owned by exactly one principal. Reading or writing a
user-owned memory requires that user's credential; reading or writing a
project-owned memory requires a valid credential for that project. Ownership
is assigned **structurally at write time from the credential context**, not by
asking the model to self-classify.

## 3. Principals and credentials

### Principals

| Principal | Represents | Owns |
|-----------|------------|------|
| **User** | A human (and their agents) | Personal memory: preferences, toolchains, identity, private notes |
| **Project** | A body of shared work, typically one repo | Shared memory: project decisions, conventions, repo facts, cross-repo links |

A *role* (permission level *within* a project — reader / writer / admin) is a
third axis. The discussion framing was "roles map to projects." This doc keeps
**project** (the resource being accessed) and **role** (the level of access)
as separate concepts, because collapsing them makes membership binary — see
[Open question 3](#3-does-role-map-to-project-or-sit-orthogonal-to-it).

### Credentials

| Credential | Proves | Lives where | Managed by |
|------------|--------|-------------|------------|
| **User token** | Caller identity (`user_id`) | Per-user env var or user-scoped config file | The user (self-service create / list / revoke) |
| **Project token** | Authorization for a project | Assigned in the management UI, or distributed alongside the repo | Project admin |

The user token is the evolution of today's `api_tokens` row (which Phase 0
already binds to a `user_id`). The project token is **new**: a credential that
carries a `project_id` and *no* `user_id`.

## 4. Memory ownership

Every fact is owned by exactly one principal. This maps onto the Phase 0
schema slots:

| Ownership | Column set | Visibility |
|-----------|------------|------------|
| User-owned | `user_id` set, `project_id` NULL | The owning user only |
| Project-owned | `project_id` set, `user_id` = the writer (for attribution) | Anyone holding a valid token for that project |

Note the project-owned row still records `user_id` — not as an access
boundary, but as **attribution**: which human wrote this project fact. That
preserves an audit trail and keeps the door open for per-member project
controls later. Access is gated on `project_id`; attribution is recorded in
`user_id`.

(Phase 0 named the reserved slot `group_id`. This doc proposes renaming it
`project_id`, or adding `project_id` as the concrete backing of the abstract
`group_id` slot — see [Schema](#6-schema-changes).)

### Structural classification — the security boundary

> **Rule:** a memory's owner is determined by the credential that authorized
> the write, never by the model's judgment.

- A write authorized by a **project credential** is project-owned by
  construction.
- A write authorized by **only a user token** is user-owned by construction.

Prompt instructions ("never store user facts in projects") are a **backstop
and a UX nicety, not the control**. A boundary that depends on the LLM
choosing correctly is not secure by design — it is a soft suggestion enforced
by a non-deterministic component. The engine already knows which principal
authorized each request (Phase 0 wires caller identity through the transport);
it should stamp ownership from that, and the prompt instruction only shapes
*which store the agent reaches for*, not whether the boundary holds.

### The residual risk: the model still writes the content

Structural stamping fixes *placement* — a write under a project credential is
project-owned, deterministically. It does **not** fix *content*: the same LLM
that placed the write also chose what to put in it, and it may put genuinely
personal content (the user's identity, a preference, PII) into a project store
while operating in a project context. The engine can't read intent, so at the
moment the model decides what to write, there is no belt — only suspenders.

The mitigation principle: **the failure modes are not symmetric.** A project
fact leaking into a user's private store is harmless (they can already see
their own memory); a private fact leaking into a project store is the breach.
So every default and every ambiguous case resolves toward *private*. That
converts the problem from "classify correctly every time" into "take a
deliberate, friction-bearing action to expose something," so a single
misjudgment fails safe rather than open.

A layered stack, none of which trusts the writer model to be right:

1. **Default-to-private, asymmetric friction.** Personal memory is the
   low-friction default sink. A project write is a *distinct tool*
   (`memory_store_project`), not a flag — so misroutes appear in the audit log
   as a deliberate project-write call, rarer (friction) and visible (review).
2. **Privilege separation by context.** In a project working session, the
   *write* credential is the project token and the user token is not
   simultaneously write-active — so the engine *cannot* write user-owned
   memory there. Reads may span both (user prefs are wanted while working);
   writes are single-target, gated by the live token. Saving a user pref
   mid-project becomes an explicit context switch, not an ambient capability.
3. **Deterministic pattern guard on project writes.** Cheap, no LLM, runs
   before any project commit: hard-block content matching the user's own
   identifiers (name, email, `/home/<user>` paths) and the
   `identity`/`preference` categories. Catches the egregious leaks with zero
   model judgment; not comprehensive, but makes the worst cases impossible.
4. **Independent DLP classifier on the project-write path.** A small local
   model (the same Gemma/Qwen routing/extraction pattern memstore already
   uses) with one job — "does this look like personal/PII content?" — gates
   project writes; flagged ones quarantine into the web UI review queue rather
   than going live. The value is that it is a *different* model than the
   writer, so it doesn't share the writer's failure mode. Two independent
   non-deterministic checks beat one.

The honest limit: because the same fallible component generates and classifies
the content, nothing here is airtight. What the stack buys is that no single
LLM misjudgment becomes a leak directly — it must clear a deterministic guard,
an independent classifier, and a friction-bearing explicit action, and the
default bias is toward the harmless failure. The prompt instruction stays as
the outermost, softest layer: it reduces how often layers 3 and 4 fire, and it
is the only place to influence the writer model's *content* choice at all.

## 5. Permission enforcement

Carried over unchanged from the tier 3 Phase 1 constraints, which this doc
honors:

- **In-engine, never post-filter.** Visibility is a `WHERE` predicate in the
  query, not a filter applied to results in app code. Counts, IDs, edge
  topology, and error-vs-empty all leak signal if filtered late
  (`tier3-permissions.md`, Phase 1 constraints).
- **Invisible == nonexistent.** A `memory_get` for a fact the caller can't see
  returns the same "not found" shape as a genuinely missing id. Graph paths
  through invisible nodes are "no path," not "redacted path."
- **Identity from transport, not tool input.** No `user`/`project` parameter
  on write/search tools. The caller's principals come from the credential(s)
  presented at the transport layer. (This is what makes [Open question 2](#2-how-are-two-credentials-presented-on-one-connection)
  the central one to settle — two credentials, one connection.)

The effective predicate for a read becomes, in spirit:

```sql
WHERE (user_id = $caller_user AND project_id IS NULL)      -- my personal memory
   OR (project_id = ANY($caller_projects))                 -- projects I hold a token for
```

where `$caller_projects` is the set of projects the caller's presented
credentials authorize.

## 6. Schema changes

Building on Phase 0 (`memstore_users`, `api_tokens.user_id`, facts'
`user_id`/`group_id`/`role_id`):

### `memstore_projects` (new)

```sql
CREATE TABLE memstore_projects (
    id          BIGSERIAL   PRIMARY KEY,
    namespace   TEXT        NOT NULL,
    name        TEXT        NOT NULL,   -- canonical project key, e.g. repo name
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, name)
);
```

### `api_tokens` gains a type and an optional project (extends Phase 0)

```sql
ALTER TABLE api_tokens
    ADD COLUMN token_type TEXT NOT NULL DEFAULT 'user',  -- 'user' | 'project'
    ADD COLUMN project_id BIGINT REFERENCES memstore_projects(id) ON DELETE CASCADE;
-- invariant: token_type='user'    => user_id NOT NULL,  project_id NULL
--            token_type='project' => project_id NOT NULL (user_id = issuer, optional)
```

A project token reuses the existing hashing / expiry / revocation machinery in
`pgstore/tokens.go` — it is the same row shape with a different `token_type`
and a `project_id` instead of (or in addition to) a `user_id`.

### `memstore_facts` — bind the reserved slot to projects

Phase 0 already added `group_id`/`role_id` (nullable, no FK). Either:

- **(a)** add `project_id BIGINT REFERENCES memstore_projects(id)` and leave
  `group_id` for a future, distinct grouping concept; or
- **(b)** treat `group_id` *as* the project FK and add the constraint now.

(a) is cleaner — "group" and "project" may not stay synonymous — and costs one
nullable column on the facts table. Recommend (a). Add the partial indexes
mirroring Phase 0's `idx_memstore_facts_user`:

```sql
CREATE INDEX idx_memstore_facts_project
    ON memstore_facts (namespace, project_id) WHERE project_id IS NOT NULL;
```

### `memstore_project_members` (optional, for non-binary membership)

Only needed if we want per-user roles within a project rather than a single
shared project token (see Open questions 1 and 3):

```sql
CREATE TABLE memstore_project_members (
    project_id BIGINT NOT NULL REFERENCES memstore_projects(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES memstore_users(id)    ON DELETE CASCADE,
    role       TEXT   NOT NULL DEFAULT 'writer',  -- reader | writer | admin
    PRIMARY KEY (project_id, user_id)
);
```

## 7. Token management surface

Phase 0 put token issuance behind `memstore admin` CLI (trust model: shell on
the daemon host == admin) and explicitly ruled out self-service. This proposal
**reverses that for user tokens**: a user manages their own keys. That is only
coherent once there is an authenticated surface for them to do it on — i.e.
the web UI in the companion brief. Until that ships, the CLI remains the
issuance path and self-service is deferred.

Sketch of the eventual surface (UI-backed, CLI-mirrored):

```
# user tokens — self-service, scoped to the authenticated user
token create  --name <user>@<host>
token list
token revoke  <user>@<host>

# projects + project tokens — project admin
project create <name>
project token create  --project <name> [--expires <dur>]
project token list     --project <name>
project token revoke   <token-id>
```

## 8. Divergences from tier 3

`tier3-permissions.md` made three assumptions this proposal changes. Listing
them so the change is deliberate, not accidental:

1. **"A web UI or self-service signup … Users are admin-provisioned via CLI"**
   (Phase 0 non-goals). This proposal introduces self-service user-token
   management and a management UI. Reconcile: keep *user creation* admin/invite
   -gated; let *token management* be self-service once authenticated.
2. **One token type.** Phase 0 has a single `api_tokens` shape (user device
   tokens). This adds a second type (project tokens). The row machinery is
   reused; the semantics differ.
3. **Roles/groups deferred entirely.** Phase 0 shipped the columns but no
   model. This proposal gives `group_id`/`role_id` a concrete meaning
   (project + permission level).

## 9. Open questions

### 1. Is a repo-distributed project token a shared secret in version control? — DECIDED (enrollment)

If the project token ships *inside* the repo, then anyone who clones the repo
has it — and so does anyone who ever saw any historical commit, because git
retains it forever. That has real costs a security review will flag:

- **No per-member revocation.** Removing one person's access means rotating
  the token and re-distributing to everyone.
- **No real attribution if the token is the only credential.** (Mitigated here
  by also requiring the user token — see Q2 — which is *why* the dual-credential
  model is worth the complexity.)
- **Secret-in-VCS smell.** Even a low-sensitivity capability token in a public
  repo is a finding.

Recommended shape: treat a repo-distributed token as an **enrollment token**,
not a standing credential. On first use, the user *redeems* it — the server
records a `memstore_project_members` row binding *that user* to the project —
and thereafter the user's own identity carries project access. The enrollment
token can then be rotated freely without locking anyone out, and access is
revocable per-member. This keeps the "drop a token in the repo and it just
works" ergonomics while removing the standing shared secret. If the project is
genuinely low-stakes and convenience wins, the raw shared token stays an
option — but as a documented downgrade, not the default.

**Decided (2026-05-26):** enrollment model. The repo-distributed token is a
one-time enrollment token, not a standing credential. The raw-shared-token
variant is a documented downgrade for low-stakes projects only.

### 2. How are two credentials presented on one connection?

MCP/bearer transport carries one token by convention. Requiring *both* a user
token and a project token per request needs a mechanism. Two options:

- **(a) Enrollment / membership grant** (preferred, and it composes with Q1):
  the project token is redeemed once; afterward the caller presents only their
  user token, and the engine derives `$caller_projects` from
  `memstore_project_members`. Clean, respects "identity from transport, not
  tool input," and gives per-member revocation for free.
- **(b) Second credential per request** (project token as a second header or
  connection parameter). Simpler to reason about for ephemeral/CI access where
  no durable user exists, but it pushes a secret onto every call and doesn't
  by itself solve revocation.

Recommend (a) as the model, with (b) retained only for the
no-durable-user case (CI bots, one-shot tooling).

**Decided (2026-05-26):** follows from Q1 — enrollment *is* the membership
grant. At request time the caller presents only their user token; the engine
derives `$caller_projects` from `memstore_project_members`. Option (b) is kept
solely for the no-durable-user case.

### 3. Does "role" map to "project," or sit orthogonal to it? — DECIDED (orthogonal)

The discussion framing mapped roles onto projects 1:1. Doing so makes
membership binary: you hold the project token or you don't; there is no
"reader vs. writer vs. admin of project X." Keeping them separate (project =
*what* you can reach, role = *what you can do there*) costs the
`memstore_project_members.role` column but buys least-privilege within a
project. Recommend separate; the binary case is just "everyone is a writer,"
which the separate model expresses for free.

**Decided (2026-05-26):** project (resource) and role (reader / writer /
admin) are separate axes. `memstore_project_members.role` carries the level.

### 4. Who can create users and projects?

Self-service token *management* is proposed, but user/project *creation* is a
different privilege. In personal-infra deployments, creation should stay
invite- or admin-gated. The web brief needs to settle this.

### 5. Can a memory be relevant to both a user and a project?

The model says one owner per fact. A genuinely dual-relevant fact ("Matthew
prefers X *in this project*") forces a choice. Options: duplicate, or use a
`memstore_links` edge from a user-owned fact to a project context. Lean on
links rather than dual ownership — dual ownership reopens the visibility
predicate complexity the one-owner rule exists to avoid.

## 10. Phasing

- **Phase 0** — identity schema. *Designed (`tier3-permissions.md`).*
- **Phase 1a** — projects, project tokens, `project_id` on facts,
  write-time ownership stamping, the in-engine read predicate, enrollment/
  membership grant. The minimum that makes shared project memory real and
  enforced.
- **Phase 1b** — per-member roles (`memstore_project_members.role`),
  self-service token management, and the management/visualization web UI
  (companion brief: [`web-ui-brief.md`](web-ui-brief.md)).

## 11. Test obligations (sketch)

Mirrors the tier 3 Phase 0 plan, extended for projects:

- Write under a project credential → fact is project-owned; readable by a
  second user holding the same project token, **not** by a user without it.
- Write under only a user token → user-owned; invisible to every other user
  and to project-token-only callers.
- `memory_get` on a fact in a project the caller can't reach → same "not
  found" shape as a missing id (no existence signal).
- Enrollment: redeem a project token → `memstore_project_members` row appears;
  subsequent access works with the user token alone; rotating the enrollment
  token doesn't revoke the enrolled member.
- Per-member revocation removes one member without affecting others.
- Ownership is stamped from the credential even when the content "looks like"
  the other kind (the structural-classification guarantee — a user-flavored
  fact written under a project credential is still project-owned).
```
