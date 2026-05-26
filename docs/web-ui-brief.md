# Brief — Management & Visualization Web UI

Status: Exploratory brief, not a design.
Author: Matthew + Claude
Date: 2026-05-26

A short brief, not a spec. Today the only interface to memstore is the MCP
client, which is opaque: you can't see what's stored, who can see it, or how
facts connect without issuing tool calls and reading JSON. As the data model
grows users, projects, and tokens (see
[`multi-user-data-model.md`](multi-user-data-model.md)), that opacity stops
being tolerable — there is no surface on which a user could manage their own
tokens, an admin could provision a project, or anyone could audit visibility.

## Why now

The multi-user model has a hard dependency on this UI for one feature:
**self-service token management** is incoherent without an authenticated
surface for a user to do it on. Everything else here is valuable but not
blocking.

## What it covers

Three concerns, roughly in priority order:

1. **User & token management** (unblocks the multi-user model)
   - A user signs in and manages their own API keys: create, name, list,
     see last-used, revoke. Plaintext shown once at creation (mirrors
     `pgstore/tokens.go` issuance).
   - Token list shows scope/type (user vs. project), expiry, last use.

2. **Project & role management** (admin)
   - Create a project, issue/rotate/revoke its token, see members and roles.
   - Enroll/remove members if the enrollment model (data-model Q1/Q2) is
     adopted.
   - Settle the open governance question: who may create users and projects.

3. **Memory visualization & search** (the "see what's in there" win)
   - Search and browse facts the signed-in principal can see — respecting the
     same in-engine visibility predicate as the MCP path, never a separate
     less-strict read path.
   - Render the supersession chain (history) and the `memstore_links` graph,
     so the repo/cross-repo connection structure is actually visible.
   - Read-only first; editing/superseding through the UI is a later add.

## Hard constraints

- **No separate read path.** The UI reads through the same store + visibility
  predicate as MCP. A UI that can see more than the predicate allows is a
  permission bypass. Reuse the engine; do not reimplement filtering in the
  front end.
- **Secure by design and visibly so** (this is a published repo). Session auth,
  CSRF protection, no token plaintext in logs or URLs, token plaintext shown
  exactly once. Standard web hardening — the project's existing
  `golang-security` practices apply to the server side.
- **Personal-infra scale.** This is not a SaaS console. Single namespace per
  deployment, a handful of users. Don't over-build multi-tenant machinery the
  data model doesn't have yet.

## Explicitly out of scope (for the brief)

- Framework / stack choice (Go-templated server vs. SPA + JSON API).
- Hosting and how it relates to `memstored`.
- Real-time updates, dashboards, analytics.

## Open questions

- Does this live in `memstored` (one binary, one port, auth shared) or as a
  separate service against the same store?
- Auth for the UI itself: reuse the bearer token, or a session layer
  (cookie + login) on top of it? A browser UI wants sessions, not a bearer
  token pasted into a field.
- How much of the MCP tool surface should the UI expose for writes, given the
  agent is the primary writer and humans editing memory directly is a
  different (and riskier) interaction.

## Next step

Decide whether to track this as a GitHub issue or promote it to a full design
doc once the multi-user data model settles — the UI's auth and project
surfaces depend on choices still open there.
