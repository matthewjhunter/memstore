# Installing memstore-mcp

memstore-mcp gives Claude Code persistent, searchable memory across sessions. It runs as an MCP server over stdio, backed by SQLite with hybrid full-text and vector search.

## Prerequisites

- **Go 1.24+** (for building from source)
- **Ollama** running locally with an embedding model

### Pull an embedding model

memstore-mcp uses Ollama to compute embeddings for semantic search. Pull a model before first use:

```bash
ollama pull embeddinggemma
```

Any Ollama embedding model works. The default is `embeddinggemma`; you can also use `nomic-embed-text` or others. The model is locked in on first use — the store validates that subsequent opens use the same model to prevent mixed embeddings.

## Build and install

```bash
git clone https://github.com/matthewjhunter/memstore.git
cd memstore
GOWORK=off go install ./cmd/memstore-mcp
```

This places the binary at `$GOPATH/bin/memstore-mcp` (typically `~/go/bin/memstore-mcp`). Make sure `$GOPATH/bin` is on your `PATH`.

Verify it runs:

```bash
memstore-mcp --help
```

## Register with Claude Code

Claude Code manages MCP servers through the `claude mcp` CLI. Add memstore at user scope so it's available in every project:

```bash
claude mcp add memstore -s user -- memstore-mcp
```

Or with explicit options:

```bash
claude mcp add memstore -s user -- memstore-mcp --model embeddinggemma --ollama http://localhost:11434
```

### Verify

```bash
claude mcp list
```

You should see:

```
memstore: memstore-mcp --model embeddinggemma - ✓ Connected
```

For full details:

```bash
claude mcp get memstore
```

### Remove

```bash
claude mcp remove memstore -s user
```

## Configuration flags

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | `~/.local/share/memstore/memory.db` | Path to SQLite database |
| `--namespace` | `default` | Namespace for fact isolation |
| `--ollama` | `http://localhost:11434` | Ollama base URL |
| `--model` | `embeddinggemma` | Embedding model name |

The database directory is created automatically on first run. The default path follows the XDG Base Directory Specification (`$XDG_DATA_HOME/memstore/memory.db`).

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
# Build the CLI
GOWORK=off go install ./cmd/memstore

# Export all facts to JSON
memstore export > backup.json

# Import facts (embeddings are recomputed on import)
memstore import < backup.json
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
