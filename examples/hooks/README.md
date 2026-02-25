# Claude Code Hook Examples

These hooks wire memstore into Claude Code's session lifecycle so context
surfaces automatically without relying on the assistant to remember.

## memstore-startup.mjs

**Event:** `SessionStart`

Runs at the start of every session and injects two blocks of context:

1. **Pending tasks** — runs `memstore tasks --surface startup`. Tasks created
   with `memory_task_create` and `surface=startup` appear automatically;
   completing or cancelling a task removes the flag so it stops surfacing.

2. **Project facts** — runs `memstore list --subject <project>` where
   `<project>` is the basename of the working directory, giving Claude
   immediate context about the active project without needing to search.

### Installation

1. Copy the script to `~/.claude/hooks/`:

   ```bash
   cp memstore-startup.mjs ~/.claude/hooks/
   ```

2. Set `MEMSTORE_BIN` to your memstore binary path, or edit the script
   directly. The default tries `memstore` on PATH; if `~/go/bin` is not in
   your hook environment, set the env var or hardcode the path:

   ```bash
   export MEMSTORE_BIN="$HOME/go/bin/memstore"
   ```

3. Register the hook in `~/.claude/settings.local.json`:

   ```json
   {
     "hooks": {
       "SessionStart": [
         {
           "matcher": "*",
           "hooks": [
             {
               "type": "command",
               "command": "node /home/YOU/.claude/hooks/memstore-startup.mjs",
               "timeout": 5
             }
           ]
         }
       ]
     }
   }
   ```

   Replace `/home/YOU` with your home directory path.

### Behaviour

- Injects a `[MEMSTORE - Pending Tasks]` block if startup-surface tasks exist.
- Injects a `[MEMSTORE - Project: <name>]` block if facts exist for the
  current project subject.
- Exits 0 silently if the binary is missing, the DB does not exist yet, or
  no relevant data is found. Safe to deploy on a fresh install.

### Example output injected into session context

```
[MEMSTORE - Pending Tasks]
• [high] Fix memory leak in feed parser (project: herald)
• Add test coverage for majordomo and herald (project: portfolio)

[MEMSTORE - Project: memstore]
[id=12] memstore | project | 2025-11-01
  SQLite-backed persistent memory MCP server for Claude Code sessions.
```

---

## memstore-prompt.mjs

**Event:** `UserPromptSubmit`

Runs before every user message is processed. Searches memstore with FTS
and injects matching facts as `additionalContext`, so relevant memories
surface automatically on every prompt without Claude needing to initiate
a search.

**Design:** FTS-only (no Ollama round-trip) to keep added latency under
~50ms. Skips prompts shorter than 5 words and slash commands. Caps the
query at 200 characters.

### Installation

1. Copy the script to `~/.claude/hooks/`:

   ```bash
   cp memstore-prompt.mjs ~/.claude/hooks/
   ```

2. Set `MEMSTORE_BIN` if needed (same as the startup hook).

3. Register the hook in `~/.claude/settings.local.json`:

   ```json
   {
     "hooks": {
       "UserPromptSubmit": [
         {
           "matcher": "*",
           "hooks": [
             {
               "type": "command",
               "command": "node /home/YOU/.claude/hooks/memstore-prompt.mjs",
               "timeout": 5
             }
           ]
         }
       ]
     }
   }
   ```

### Behaviour

- Injects a `<memstore-recall>` block with up to 5 FTS-matched facts.
- Exits 0 silently on any error so it never blocks a prompt.
- No-ops on short prompts and slash commands to avoid noise.

---

## memstore-session-end.mjs

**Event:** `SessionEnd`

Runs at the end of every session and does two things:

1. **Records a "last active" fact** — stores a `session-activity` note with
   the working directory and timestamp, building a lightweight history of
   which projects were active and when.

2. **Prints open tasks** — runs `memstore tasks --surface startup` and writes
   any still-pending tasks to stderr as a reminder to update statuses before
   leaving.

### Installation

1. Copy the script to `~/.claude/hooks/`:

   ```bash
   cp memstore-session-end.mjs ~/.claude/hooks/
   ```

2. Set `MEMSTORE_BIN` if needed (same as the startup hook).

3. Register the hook in `~/.claude/settings.local.json`:

   ```json
   {
     "hooks": {
       "SessionEnd": [
         {
           "matcher": "*",
           "hooks": [
             {
               "type": "command",
               "command": "node /home/YOU/.claude/hooks/memstore-session-end.mjs",
               "timeout": 5
             }
           ]
         }
       ]
     }
   }
   ```

   Replace `/home/YOU` with your home directory path.

### Behaviour

- **Session activity**: stores a `session-activity` fact (subject) with
  `category: note` and metadata containing `directory` and `timestamp`.
  These accumulate over time and can be queried with `memstore list
  --subject session-activity`.
- **Open task reminder**: writes to stderr (visible in the Claude Code UI)
  if any startup-surface tasks are still open.
- Exits 0 silently if the binary is missing or the DB does not exist yet.

---

## Slash commands

Two slash commands complement the hooks with explicit control over storage
and retrieval. Install them by placing the files in `~/.claude/commands/`:

### /mem

```bash
# Store in ~/.claude/commands/mem.md
```

Usage: `/mem <fact to store>`

Instructs Claude to call `memory_store` with the given content, inferring
subject and category from context. Useful for quickly capturing something
without breaking flow.

```
/mem Matthew prefers FTS-only search in latency-sensitive contexts
```

### /recall

```bash
# Store in ~/.claude/commands/recall.md
```

Usage: `/recall <search query>`

Instructs Claude to call `memory_search` and present matching facts. Useful
when you know something relevant is stored but the automatic hook missed it.

```
/recall what do we know about the memstore embedding pipeline?
```

---

## Complete settings.local.json example

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "node /home/YOU/.claude/hooks/memstore-startup.mjs",
            "timeout": 5
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "node /home/YOU/.claude/hooks/memstore-prompt.mjs",
            "timeout": 5
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "node /home/YOU/.claude/hooks/memstore-session-end.mjs",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
```
