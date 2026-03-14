# Installing memstore

memstore gives Claude Code persistent, searchable memory across sessions. It runs as an MCP server over stdio, backed by SQLite with hybrid full-text and vector search. Hooks inject relevant context automatically at every stage of the session lifecycle.

## Quick Start (Recommended)

```bash
# Install both binaries
go install github.com/matthewjhunter/memstore/cmd/memstore@latest
go install github.com/matthewjhunter/memstore/cmd/memstore-mcp@latest

# Pull an embedding model
ollama pull embeddinggemma

# Set up everything
memstore setup
```

`memstore setup` does the following:
1. Checks prerequisites (Claude CLI, Ollama)
2. Detects the `memstore` and `memstore-mcp` binary locations
3. Auto-detects daemon mode (checks for running `memstored`)
4. Installs 8 hook scripts to `~/.claude/hooks/`
5. Merges hook registrations into `~/.claude/settings.local.json`
6. Registers the MCP server with `claude mcp add`
7. Creates `~/.config/memstore/config.toml` if absent

### Setup flags

| Flag | Description |
|------|-------------|
| `--force` | Overwrite existing hooks and config |
| `--remote URL` | Specify memstored daemon URL (skip auto-detection) |
| `--dry-run` | Show what would be done without making changes |

Running `memstore setup` again after updating the binary deploys the latest hooks. Identical files are skipped; changed files warn unless `--force` is used.

## Prerequisites

- **Go 1.24+** (for building from source)
- **Ollama** running locally with an embedding model

### Pull an embedding model

memstore uses Ollama to compute embeddings for semantic search. Pull a model before first use:

```bash
ollama pull embeddinggemma
```

Any Ollama embedding model works. The default is `embeddinggemma`; you can also use `nomic-embed-text` or others. The model is locked in on first use — the store validates that subsequent opens use the same model to prevent mixed embeddings.

## Daemon Mode

For lower-latency context injection and background processing (transcript extraction, hint generation), run `memstored`:

```bash
go install github.com/matthewjhunter/memstore/cmd/memstored@latest
memstored
```

The daemon listens on port 8230 by default and provides:
- `/v1/recall` — fact-based context injection for the prompt hook
- `/v1/context/hints` — proactive storage nudges
- `/v1/context/touch` — file access tracking for recall boosting
- `/v1/sessions/transcript` — session transcript processing and fact extraction

`memstore setup` auto-detects a running daemon. To configure manually:

```bash
memstore setup --remote http://your-host:8230
```

Without the daemon, memstore operates in local-only mode. Hooks that depend on HTTP APIs (prompt recall, context touch, stop hook) silently degrade — they won't error, but recall and hint injection won't be available.

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

Hook scripts are installed to `~/.claude/hooks/` and registered in `~/.claude/settings.local.json`.

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

With explicit options:

```bash
claude mcp add memstore -s user -- memstore-mcp --model embeddinggemma --ollama http://localhost:11434
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
memstore: memstore-mcp --model embeddinggemma - ✓ Connected
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
| `--ollama` | `http://localhost:11434` | Ollama base URL |
| `--model` | `embeddinggemma` | Embedding model name |
| `--remote` | (none) | memstored daemon URL |

The database directory is created automatically on first run. The default path follows the XDG Base Directory Specification (`$XDG_DATA_HOME/memstore/memory.db`).

### Environment variables

All configuration can also be set via environment variables: `MEMSTORE_DB`, `MEMSTORE_NAMESPACE`, `MEMSTORE_OLLAMA`, `MEMSTORE_MODEL`, `MEMSTORE_REMOTE`, `MEMSTORE_API_KEY`.

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
2. Computes an embedding via Ollama
3. Inserts the fact into SQLite with FTS5 indexing

When Claude calls `memory_search`, the server:
1. Runs parallel FTS5 full-text search and cosine similarity vector search
2. Merges and ranks results with configurable weights (default: 60% FTS, 40% vector)
3. Applies temporal decay for ephemeral categories (notes decay over 30 days; preferences and identity facts don't)
4. Bumps usage counters on returned facts

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
Run `memstore setup --dry-run` to verify hook installation. Check `~/.claude/settings.local.json` for correct hook registrations. Restart Claude Code after installing hooks.

**Daemon not detected by setup:**
Make sure `memstored` is running and accessible at `http://localhost:8230`. Use `memstore setup --remote URL` to specify a non-default address.
