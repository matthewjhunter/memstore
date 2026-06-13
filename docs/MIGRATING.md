# Migrating

This document covers upgrading between memstore versions. It records breaking
changes, the migration steps for each, and the security caveats that apply to
the current release.

For the full changelog, see [`CHANGELOG.md`](../CHANGELOG.md).

## From v0.3.0 to v0.4.0

v0.4.0 closes the single-user gap that v0.3.0 shipped with. Isolation is now
**enforced**, not just deployment-assumed: every read and write -- facts,
links, sessions, hints, and feedback -- is filtered by the user the bearer
token belongs to. Two tokens for two users never see each other's data. A
shared deployment where multiple people hold their own tokens is now safe.

### Breaking changes

**Identity is required.** Each fact, link, token, and session row now carries
a `user_id`, and tokens are bound to a user. The daemon refuses to start
against a Postgres database that has data but no recorded default user.

- **Existing single-user deployments upgrade automatically.** On first start,
  the migration infers the default user from your existing token names (the
  pre-v0.4.0 `<user>-<host>` convention) and assigns every existing fact,
  link, and session to that user. No manual step if your token names share a
  single prefix.
- **A fresh Postgres deployment, or one the migration cannot infer a user
  for** (no tokens, or tokens with more than one distinct prefix), stops with
  an instruction to run:
  ```sh
  memstore admin tier3-init --default-user <name>
  ```
  once before starting `memstored`.

**Token names move to `<user>@<host>`.** The old `<user>-<host>` convention is
retired (hyphens are ambiguous -- they appear in user names and hostnames).
The migration rewrites existing token names automatically (`matthew-laptop`
becomes `matthew@laptop`; the bootstrap `legacy` token becomes
`<default-user>@legacy`). New tokens must match the `<user>@<host>` shape.

### Managing users

```sh
# Create a user
memstore admin user-add alice

# Mint a token for a user (the token proves who the caller is)
memstore admin issue-token alice@laptop --user alice

# List users / tokens
memstore admin list-users
memstore admin list-tokens

# Disable a user -- revokes all their tokens. With no active token the user
# cannot authenticate. This is how you lock an account out.
memstore admin disable-user alice
```

All `admin` commands connect directly to Postgres and are meant to run on the
daemon host (set `--pg` or `MEMSTORE_PG`).

### What did not change

Clients need no changes: the bearer token they already hold keeps working
(its name is rewritten in place, its value is unchanged), and it now scopes
the holder to their own data automatically. The MCP tools take no new
parameters -- identity comes from the transport, never from tool input.

## From v0.2.0 to v0.3.0

v0.3.0 is a substantial re-platform. The CLI library still works in
local-only mode against SQLite, but the recommended deployment shape is now
the `memstored` daemon with a Postgres backend.

### Breaking changes

**`memstored` is Postgres-only.** SQLite mode has been removed from the
daemon. The `memstore` CLI binary still supports SQLite for local-only
operation, but if you were running `memstored` against a SQLite file, the
upgrade is to run it against Postgres + pgvector. See
[*Deploying the daemon*](#deploying-the-daemon) below.

**`AppConfig.Model` field removed.** Embedder configuration is now read
from environment variables:

```sh
# Was: AppConfig.Model = "nomic-embed-text"
# Now:
export MEMSTORE_EMBED_BACKEND=ollama      # or openai
export MEMSTORE_EMBED_BASE_URL=http://localhost:11434
export MEMSTORE_EMBED_MODEL=nomic-embed-text

# Optional: separate generator endpoint for chat/extraction
export MEMSTORE_GEN_URL=http://localhost:11434
export MEMSTORE_GEN_MODEL=llama3.1
```

The shared `EMBEDDING_*` namespace also works as a fallback. See
[go-embedding](https://github.com/matthewjhunter/go-embedding) for the full
config surface.

**`memory_learn` tool removed.** The Go codebase-ingestion subsystem is
gone. Facts that were produced by `memory_learn` runs are still in the
store and still searchable, but they will not be regenerated and there is
no replacement tool. If you had workflows that depended on
`memory_learn`-generated symbol summaries, those workflows need to use
explicit `memory_store` calls.

**`memory_check_drift` tool removed.** The drift-detection surface --
including the `source_files` metadata convention, `GitRunner`,
`Config.RepoPaths`, and the inline drift warning in `memory_get_context`
output -- has been removed. With `memory_learn` gone, the producer of
`source_files`-tagged facts was retired; the consumer follows. If you
have facts in your store with `source_files` metadata, the metadata is
harmless (just unused). No data migration is required.

**`metadata.related_facts` convention deprecated.** Use explicit links via
`memory_link` / `memory_get_links` (V7 schema). Old `related_facts` JSON
arrays are not migrated automatically; current code paths ignore them.

**Fact content cap.** Postgres enforces `CHECK (length(content) <= 8000)`
at the schema layer (V2 migration). Larger facts are rejected at write
time. The application layer has the same cap, but the schema constraint
is the wall behind the wall.

**Hooks moved.** Hooks now live in `cmd/memstore/hooks/` with install-time
placeholders, embedded in the `memstore` binary. The previous standalone
hook scripts in `examples/` are no longer installed. Run `memstore setup`
to deploy the current hooks.

**Hooks register to `settings.json`.** Earlier versions wrote to
`settings.local.json`. The new location version-controls hook
registrations with the project. Existing `settings.local.json`
registrations should be removed; `memstore setup` does this when run.

**Compact-before-exit gate removed.** The PostCompact hook still records
observability data, but the `/exit` gate that prevented exiting an
uncompacted long session has been removed. If your workflow depended on
that gate, it isn't there anymore.

### Behavioral changes worth knowing

**Hybrid search now reranks by default when an embedder and reranker are
configured.** The third stage is a cross-encoder pass over the top-K
candidates from the FTS + vector merge. Configurable per-request via
`memory_search`'s `rerank_mode` parameter or per-session via
`memory_rerank_settings`. If no reranker is configured, the search
degrades to the two-stage hybrid as before.

**Per-prompt recall fires on every user message** when a daemon is
configured. The `UserPromptSubmit` hook (`memstore-prompt.mjs`) calls
`/v1/recall` and injects relevant facts into the model's context. If the
daemon is unavailable, the hook silently no-ops; the session continues
without injection.

**Embed queue quarantines permanent failures.** A fact whose embed call
hits a permanent error (post adaptive shrink) gets `embed_failed_at` and
`embed_error` set, and the queue moves on. Previously the queue would
retry such facts forever. To re-queue a quarantined fact, edit its
content (which resets the embedding) and clear `embed_failed_at`.

**Per-fact embed calls.** The queue no longer batches embed calls. One
network round-trip per fact. Slower in the happy path; immune to one bad
fact stalling the queue.

### Deploying the daemon

If you were running `memstored` against SQLite:

1. Stand up a Postgres instance with pgvector. The container image needs
   a `MEMSTORE_PG` like
   `postgres://user:pass@host:5432/memstore?sslmode=disable` (or with
   `sslmode=require` for non-local hosts).
2. Issue an API token:
   ```sh
   memstore admin issue --name <client-name>
   # Prints the plaintext token once. Save it.
   ```
3. (Recommended) Generate a TLS cert and run the daemon over HTTPS:
   ```sh
   memstore tls init-ca
   memstore tls issue-server --host memstored.lan
   memstored --tls-cert /path/to/server.crt --tls-key /path/to/server.key
   ```
4. Export the previous SQLite store and import into Postgres:
   ```sh
   # On the old host
   memstore export --db /path/to/old.db --output facts.json

   # On the new host (with MEMSTORE_REMOTE pointing at memstored)
   export MEMSTORE_REMOTE=https://memstored.lan:8230
   export MEMSTORE_API_KEY=<token from step 2>
   memstore import facts.json
   ```
   Note: embeddings are excluded from export and will be regenerated by
   the async embed queue after import. Searches that depend on the vector
   lane will be FTS-only for the few minutes it takes the queue to catch
   up.
5. Point clients at the new daemon by setting `MEMSTORE_REMOTE` and
   `MEMSTORE_API_KEY` in their environment. The MCP server, the CLI, and
   the hooks all read these.

### Local-only upgrades

If you weren't running the daemon and only used the local CLI library,
the upgrade is simpler:

1. `go install github.com/matthewjhunter/memstore/cmd/memstore@latest`
2. `go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest`
3. Set the embedder env vars (see *Breaking changes* above) so the new
   env-driven config can find your embedding endpoint.
4. Run `memstore setup` to refresh hooks and MCP registration.
5. Your existing SQLite database is still readable. The schema
   migrations will run automatically on first open.

### Verification

After upgrading, sanity-check:

```sh
# Daemon is reachable
curl https://memstored.lan:8230/v1/health

# Token works
MEMSTORE_REMOTE=https://memstored.lan:8230 \
MEMSTORE_API_KEY=<token> \
memstore list --limit 1

# MCP server starts
memstore-mcp --help

# Hooks are registered
cat .claude/settings.json | jq .hooks
```

If you hit issues, [open an issue](https://github.com/matthewjhunter/memstore/issues)
with the output of `memstore-mcp --version`, your environment variables
(with secrets redacted), and what you saw.
