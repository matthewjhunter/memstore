# Installing memstore

memstore gives Claude Code persistent, searchable memory across sessions. It runs as an MCP server over stdio backed by SQLite in local mode, or by a `memstored` daemon (Postgres + pgvector) in daemon mode. Hybrid full-text and vector search with an optional cross-encoder rerank stage. Hooks inject relevant context automatically at every stage of the session lifecycle.

> **⚠ v0.3.0 ships authentication plumbing without per-user data scoping.** Two valid tokens see the same facts. Don't deploy the daemon as a shared multi-user service until v0.4.0. See [`MIGRATING.md`](MIGRATING.md) for the full caveat.

## Quick Start (Recommended)

```bash
# Install both binaries
go install github.com/matthewjhunter/memstore/cmd/memstore@latest
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest

# Pull an embedding model
ollama pull nomic-embed-text

# Configure embedder (or set EMBEDDING_* / MEMSTORE_EMBED_* in your shell rc)
export EMBEDDING_BACKEND=ollama
export EMBEDDING_BASE_URL=http://localhost:11434
export EMBEDDING_MODEL=nomic-embed-text

# Set up everything
memstore setup
```

`memstore setup` does the following:
1. Checks prerequisites (Claude CLI, embedder reachability)
2. Detects the `memstore` and `memstore-mcp` binary locations
3. Auto-detects daemon mode (checks for running `memstored`)
4. Installs 7 hook scripts to `~/.claude/hooks/`
5. Merges hook registrations into `~/.claude/settings.json`
6. Registers the MCP server with `claude mcp add`
7. Creates `~/.config/memstore/config.toml` if absent

### Setup flags

| Flag | Description |
|------|-------------|
| `--force` | Overwrite existing hooks (config.toml is preserved if it exists) |
| `--remote URL` | Specify memstored daemon URL (skip auto-detection) |
| `--dry-run` | Show what would be done without making changes |

Running `memstore setup` again after updating the binary deploys the latest hooks. Identical files are skipped; changed files warn unless `--force` is used.

## Prerequisites

- **Go 1.25+** (for building from source)
- An embedding endpoint: Ollama, or any OpenAI-compatible `/v1/embeddings` provider
- For daemon mode: **Postgres 14+ with pgvector**

### Pull an embedding model

memstore delegates embedding to [go-embedding](https://github.com/matthewjhunter/go-embedding), which speaks both the native Ollama API and any OpenAI-compatible `/v1/embeddings` endpoint (LiteLLM, vLLM, Ollama's compat layer, Lemonade, OpenAI itself). The simplest local setup is Ollama:

```bash
ollama pull nomic-embed-text
```

`nomic-embed-text` is a sensible default; any Ollama or OpenAI-compatible embedding model works. The model + vector dimension are locked in on first use — the store validates the recorded fingerprint on subsequent opens to prevent mixing embeddings from incompatible models.

### Configuring the embedder

Embedding configuration is environment-driven. Memstore's binaries call `embedding.ConfigFromEnvPrefix("MEMSTORE_EMBED")`, which cascades per-field through `MEMSTORE_EMBED_*` → `EMBEDDING_*` → `embedding.DefaultConfig()`:

| Variable | Example | Notes |
|----------|---------|-------|
| `EMBEDDING_BACKEND` | `ollama` or `openai` | Required if defaults won't do |
| `EMBEDDING_BASE_URL` | `http://localhost:11434` | Ollama API or OpenAI-compatible base |
| `EMBEDDING_MODEL` | `nomic-embed-text` | Model name as the backend understands it |
| `EMBEDDING_API_KEY` | `sk-…` | Only needed for authed backends |
| `EMBEDDING_STRICT` | `false` | If `true`, oversize text errors instead of truncating |

Use the `MEMSTORE_EMBED_*` form when you want memstore to differ from a shared `EMBEDDING_*` default that other apps inherit.

## Daemon Mode

For multi-machine access, lower-latency context injection, and background processing (transcript extraction, hint generation, feedback rating), run `memstored`:

```bash
go install github.com/matthewjhunter/memstore/cmd/memstored@latest

# Postgres with pgvector is required
export MEMSTORE_PG='postgres://memstore:secret@host:5432/memstore?sslmode=require'

# Same embedder config as the CLI
export MEMSTORE_EMBED_BACKEND=ollama
export MEMSTORE_EMBED_BASE_URL=http://localhost:11434
export MEMSTORE_EMBED_MODEL=nomic-embed-text

memstored
```

The daemon listens on port 8230 by default. Endpoints:

- `/v1/health` -- unauthenticated liveness probe
- `/v1/recall` -- per-prompt context injection
- `/v1/search`, `/v1/facts/*` -- full Store interface over HTTP
- `/v1/context/hints` -- proactive nudges from the extraction pipeline
- `/v1/context/touch` -- file-access tracking
- `/v1/sessions/turns`, `/v1/sessions/turns/finalize` -- session capture pipeline
- `/v1/learn` (deprecated; honored for backwards compatibility but no longer wired into the MCP server)

### TLS (recommended)

Generate a self-signed CA + server cert via the built-in stdlib CA:

```bash
memstore tls init-ca
memstore tls issue-server --host memstored.lan
memstored --tls-cert server.crt --tls-key server.key
```

Optional mTLS: also pass `--client-ca ca.crt` to require client certificates.
See [`internal/caetl/caetl.go`](../internal/caetl/caetl.go) for the CA shape.

### Bearer-token auth

Every endpoint except `/v1/health` requires `Authorization: Bearer <token>`.
Tokens are issued via the CLI; the daemon stores SHA-256 hashes and verifies
with constant-time comparison.

Tokens are bound to a user, so the user has to exist first. Admin commands talk
to PostgreSQL directly rather than through the API, so they run on the daemon
host with `--pg` (or `MEMSTORE_PG`) pointing at the database:

```bash
export MEMSTORE_PG=postgres://memstore:<password>@localhost:5432/memstore

# One-time: seed the identity schema and create the user
memstore admin tier3-init --default-user matthew
memstore admin user-add matthew

# Issue a token per client machine, so it can be revoked individually.
# Convention for the token name is <user>@<host>; plaintext is shown once.
memstore admin issue-token --user matthew --scopes admin matthew@laptop

# Configure the client
export MEMSTORE_REMOTE=https://memstored.lan:8230
export MEMSTORE_API_KEY=<token>

# memstore setup will pick those up automatically
memstore setup
```

Every admin command acts on a namespace, defaulting to the daemon's own
(`namespace` in the config file, or `MEMSTORE_NAMESPACE`; built-in default
`default`). Pass `--namespace` only when administering some other tenant --
targeting the wrong namespace is how you end up with two users of the same
name. Token names are global, not per-namespace.

Other subcommands: `list-users`, `disable-user <name>` (revokes all of a user's
tokens), `list-tokens`, `revoke-token <name>`, `rotate-token <name>`.

`memstore setup` auto-detects a running daemon. To configure manually:

```bash
memstore setup --remote https://memstored.lan:8230
```

### Optional rerank sidecar

Cross-encoder reranking runs against a separate sidecar speaking the
Cohere/Jina `/v1/rerank` wire shape (typically [llama.cpp](https://github.com/ggerganov/llama.cpp) with `--reranking`):

```bash
export MEMSTORE_RERANK_BASE_URL=http://reranker:8080
export MEMSTORE_RERANK_MODEL=bge-reranker-v2-m3
```

When the sidecar is unreachable, rerank gracefully degrades to first-stage
hybrid order. The model can tune rerank behavior per session via the
`memory_rerank_settings` MCP tool.

### Container

`memstored` is published as a container image to GHCR on every push to main:

```bash
docker run -d \
  -e MEMSTORE_PG='postgres://memstore@db:5432/memstore?sslmode=disable' \
  -e MEMSTORE_API_KEY='<bootstrap-api-key>' \
  -e MEMSTORE_TLS_CERT_FILE=/certs/server.crt \
  -e MEMSTORE_TLS_KEY_FILE=/certs/server.key \
  -p 8230:8230 \
  ghcr.io/matthewjhunter/memstored:latest
```

Without the daemon, memstore operates in local-only mode. Hooks that depend on HTTP APIs (prompt recall, context touch, stop hook) silently no-op so they're safe to install either way.

## Hooks

Hooks are embedded in the `memstore` binary and installed automatically by `memstore setup`. They wire into Claude Code's session lifecycle:

| Hook | Event | Timeout | Purpose |
|------|-------|---------|---------|
| `memstore-startup.mjs` | SessionStart | 5s | Inject pending tasks + project facts |
| `memstore-prompt.mjs` | UserPromptSubmit | 5s | Recall relevant facts per prompt (daemon) |
| `memstore-read.mjs` | PreToolUse:Read | 5s | Inject file/symbol constraints |
| `memstore-edit.mjs` | PreToolUse:Edit | 5s | Inject file/symbol constraints |
| `store-nudge.mjs` | PostToolUse:Write,Bash | 2s | Nudge to store after key actions |
| `stop-hook.mjs` | Stop | 10s | Session tracking + transcript upload (daemon) |
| `memstore-session-end.mjs` | SessionEnd | 5s | Record activity + task reminders |

Hook scripts are installed to `~/.claude/hooks/` and registered in `~/.claude/settings.json` (Claude Code's `userSettings` source). Note that `~/.claude/settings.local.json` is **not** read by Claude Code — its `localSettings` source is project-scoped at `<cwd>/.claude/settings.local.json`.

## Manual Setup

If you prefer not to use `memstore setup`, follow these steps:

### Build and install

```bash
git clone https://github.com/matthewjhunter/memstore.git
cd memstore
GOWORK=off go install ./cmd/memstore-mcp
GOWORK=off go install ./cmd/memstore
```

This places the binaries at `$GOPATH/bin/` (typically `~/go/bin/`). Make sure `$GOPATH/bin` is on your `PATH`.

### Register MCP server

```bash
claude mcp add memstore -s user -- memstore-mcp
```

With daemon mode:

```bash
claude mcp add memstore -s user -- memstore-mcp --remote http://localhost:8230
```

### Verify

```bash
claude mcp list
```

You should see:

```
memstore: memstore-mcp - ✓ Connected
```

### Remove

```bash
claude mcp remove memstore -s user
```

## Configuration

### config.toml

The config file lives at `~/.config/memstore/config.toml` (or `$XDG_CONFIG_HOME/memstore/config.toml`):

```toml
# memstore configuration
remote = "http://localhost:8230"
```

### Configuration flags

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | `~/.local/share/memstore/memory.db` | Path to SQLite database |
| `--namespace` | `default` | Namespace for fact isolation |
| `--ollama` | `http://localhost:11434` | Chat LLM base URL (used for `--gen-model`) |
| `--gen-model` | (none) | Chat model for fact extraction / generation |
| `--remote` | (none) | memstored daemon URL |

Embedder settings come from environment variables only — see [Configuring the embedder](#configuring-the-embedder).

The database directory is created automatically on first run. The default path follows the XDG Base Directory Specification (`$XDG_DATA_HOME/memstore/memory.db`).

### Environment variables

| Variable | Used by | Purpose |
|----------|---------|---------|
| `MEMSTORE_DB` | CLI, MCP (local mode) | SQLite database path |
| `MEMSTORE_NAMESPACE` | CLI, MCP, daemon | Namespace partition |
| `MEMSTORE_REMOTE` | CLI, MCP | Daemon URL (enables daemon mode) |
| `MEMSTORE_API_KEY` | CLI, MCP | Bearer token for daemon mode |
| `MEMSTORE_PG` | daemon | Postgres connection string |
| `MEMSTORE_TLS_CERT_FILE`, `MEMSTORE_TLS_KEY_FILE` | daemon | Server cert paths |
| `MEMSTORE_TLS_CLIENT_CA_FILE` | daemon | mTLS client trust roots |
| `MEMSTORE_API_KEY` | daemon | Single bootstrap API key; additional tokens live in the api_tokens table (issued via `memstore admin issue-token`) |
| `MEMSTORE_EMBED_BACKEND`, `MEMSTORE_EMBED_BASE_URL`, `MEMSTORE_EMBED_MODEL`, `MEMSTORE_EMBED_API_KEY` | CLI, MCP, daemon | Embedder config (cascade to `EMBEDDING_*`) |
| `MEMSTORE_GEN_URL`, `MEMSTORE_GEN_MODEL` | daemon, MCP | Generator/chat endpoint (separable from embedder) |
| `MEMSTORE_RERANK_BASE_URL`, `MEMSTORE_RERANK_MODEL` | daemon | Optional cross-encoder reranker sidecar |

### Namespaces

Namespaces isolate facts within the same database. You could register multiple instances with different namespaces for different contexts, but the default namespace works fine for most setups.

## Configuring Claude Code to use memory

Add instructions to your global `~/.claude/CLAUDE.md` so Claude knows to use the memory tools. For example:

```markdown
## Memstore

The `memstore-mcp` MCP server provides persistent memory across sessions.

**At session startup**, search memory for:
- The user's profile and preferences (subject: "your-name")
- The current system's hardware profile (subject: hostname)
- The current working directory's project, if any

**Store proactively** as you encounter useful information:
- User preferences and corrections
- Project details and decisions
- Hardware and environment info
- Relationships between people, projects, and systems
```

## How it works

When Claude calls `memory_store`, the server:
1. Checks for exact duplicates
2. Inserts the fact into the active backend (SQLite locally; Postgres in daemon mode)
3. Indexes it for full-text search (FTS5 / tsvector)
4. Enqueues an async embedding job (the embed queue computes the vector in the background)

When Claude calls `memory_search`, the server:
1. Runs parallel full-text and cosine-similarity vector search (facts whose embedding hasn't landed yet participate in FTS only)
2. Merges and ranks with configurable weights (default: 60% FTS, 40% vector)
3. Optionally reruns the top-K through a cross-encoder reranker (when one is configured) under one of four fusion modes (off / balanced / dominant / gate) and a relevance threshold
4. Applies temporal decay for ephemeral categories (notes decay over 30 days; preferences and identity facts don't)
5. Bumps usage counters on returned facts

Facts support supersession rather than deletion — when information changes, the old fact is preserved in history and linked to its replacement.

## Backup and migration

Use the CLI tool to export and import facts:

```bash
# Export all facts to JSON
memstore export > backup.json

# Import facts (embeddings are recomputed on import)
memstore import backup.json
```

Embeddings are excluded from exports and recomputed during import, so exports are portable across embedding models.

## Troubleshooting

**Server not showing in `claude mcp list`:**
Make sure you registered with `claude mcp add -s user`, not by editing config files manually. The user-scoped MCP config lives in `~/.claude.json`.

**Server shows but tools aren't available:**
Restart your Claude Code session. MCP tools are loaded at session start.

**"embedding model mismatch" error:**
The database was created with a different embedding model. Either use the same model or start fresh with a new database path.

**Ollama connection refused:**
Make sure Ollama is running (`ollama serve`) and accessible at the configured URL.

**Hooks not firing:**
Run `memstore setup --dry-run` to verify hook installation. Check `~/.claude/settings.json` for correct hook registrations. Restart Claude Code after installing hooks.

**Daemon not detected by setup:**
Make sure `memstored` is running and accessible at `http://localhost:8230`. Use `memstore setup --remote URL` to specify a non-default address.
