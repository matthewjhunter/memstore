# Web UI: the human side of the memory store

Status: design, settled 2026-08-27 (promoted from the 2026-05-26 brief)
Author: Matthew + Claude

Companion to `docs/document-synthesis.md` (what the UI displays) and
`docs/mcp-oauth-scope.md` (the auth it shares with the MCP path). The brief this
replaces had three concerns: token management, project/role management, and
visualization. The first two are unchanged in intent and deferred behind the
multiuser data model; this document settles the third, because the synthesis
design depends on it and it no longer depends on multiuser.

## Why

Today the only interface to memstore is the MCP client, and it is opaque: what
is stored, how facts connect, and where a claim came from are visible only by
issuing tool calls and reading JSON. That was tolerable while every fact was
written by a model in conversation with the user. It stops being tolerable once
stage two of ingest writes facts nobody was in the room for. The user cannot correct
what they cannot see, and the synthesis design depends on correction.

The comparison with a markdown wiki makes the requirement concrete. A wiki is
browseable, searchable, editable, and every page links to its sources. Memstore
has better data structures than a wiki and, until this ships, none of those
properties for a human.

## What it is

A browser interface, served by `memstored`, giving a signed-in user:

- **Search** over facts and documents they can see, through the same store
  calls the MCP path uses.
- **Pages.** The view defined in `docs/document-synthesis.md`: for a subject,
  the current synthesis fact, the live facts with their provenance, and the
  documents cited. For a document, its summary fact, its chunks, and the facts
  that cite it. Every citation is a link to the verbatim chunk at the recorded
  hash. Supersession chains render as page history; links render as a graph.
- **Provenance, always shown.** Whether a fact was written by the user, derived
  by an agent (which one, from which chunks), or came from session extraction.
  Untrusted sources are marked as such on the facts derived from them.
- **Correction.** Edit a fact (which supersedes it with a user-authored fact,
  outranking any derived one), confirm it, supersede it, delete it. Resolve a
  lint flag. These are the existing store operations; the UI adds no write path
  the MCP server lacks, it just gives a person a way to invoke them.
- **Ingest.** Upload a file or a small set of files. This is the one place a
  human, rather than the CLI, exercises the `ingest` scope. Repo-scale sync
  stays on the CLI; the browser form is for the document someone just read.
- **Lint queue.** The scheduled lint's output -- stale derived facts,
  contradictions, orphans -- as a list a person works through.

## Hard constraints

Carried over from the brief and still binding.

- **No separate read path.** The UI reads through the same store and the same
  visibility predicate as MCP. A UI that can see more than the predicate
  allows is a permission bypass. The handlers call the store; there is no
  second implementation of filtering.
- **No separate write path.** Every edit, supersession, deletion, and upload is
  the store operation the MCP or ingest path already performs, under the
  signed-in user's identity. Scopes apply: a session whose token lacks `ingest`
  does not get the upload form.
- **Secure by design and visibly so.** Session cookies (`Secure`, `HttpOnly`,
  `SameSite=Lax`), CSRF tokens on every state-changing form, no token plaintext
  in logs or URLs, plaintext shown exactly once at issuance. Content-Security-
  Policy with no inline script. The chunk content and fact content are
  untrusted text and are rendered as text, never as HTML.
- **Personal-infra scale.** Server-rendered Go templates, no SPA, no build step
  in the release pipeline. A handful of users per deployment.

## Auth: the UI is a dumb RP

The brief asked whether the UI reuses the bearer token or adds a session layer.
The OAuth resource-server work answers it. Under the federation design memstore
is a resource server for the MCP path: the client runs the authorization-code
flow against webauth, presents a bearer token, memstore validates it and maps
`sub` to a memstore user. A browser wants a session, not a bearer token pasted
into a field, and the federation design already has a shape for that: websites
are dumb relying parties behind webauth.

So the UI is a dumb RP. Login redirects to webauth; the callback establishes a
server-side session bound to the same `sub` the MCP path would resolve; both
paths land on the same memstore user and the same visibility predicate. The UI
runs no federation of its own and talks to nothing upstream of webauth. This is
the one place memstore does hold a client registration with webauth, and it
is the RP kind, not the resource-server kind -- the two roles are distinct and
documented separately in `docs/mcp-oauth-scope.md`.

For the static-token trial (`docs/trial-onboarding-scope.md`), the login page
accepts a token once and mints the same session. No OAuth, no webauth, same
session machinery, same predicate. That keeps the docker trial at one
credential.

Sessions live in Postgres alongside tokens, so revoking a token or a user ends
their sessions on the next request.

## Mounted under the prefix

`memstored` serves the UI at `/memstore/ui/` beside `/memstore/mcp` and
`/memstore/v1/`. One binary, one port, one auth resolution. `/.well-known/`
stays the host's. Static assets are embedded in the binary.

## Not in scope

- Token self-service and project/role administration: the brief's first two
  concerns. They wait for the multiuser data model, and the same session
  machinery will carry them when they arrive.
- Real-time updates, dashboards, analytics.
- Editing chunks or documents. The corpus is immutable by design; the UI can
  delete a document (which cascades to its chunks and orphans its citations)
  and re-upload, and that is all.

## Open questions

- **Rendering untrusted facts.** A derived fact whose citations include an
  untrusted chunk should say so. Whether that is a badge, a fenced quotation,
  or both is a design-in-the-browser question.
- **Graph view.** A force-directed graph of links demos well and tends to go
  unused afterwards. Start with a list of links per page and see
  whether anyone asks.
- **Upload limits.** The browser form needs a size cap and a file-count cap
  that the CLI does not; pick them when the form exists.

## Sequence

Follows `docs/document-synthesis.md`: pages and search first, read-only, so
derived facts can be seen as soon as summarize-on-ingest writes them; then
correction; then upload; then the lint queue when lint exists. Session auth
comes first of all, because nothing else can be served without it.
