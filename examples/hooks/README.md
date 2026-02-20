# Claude Code Hook Examples

These hooks wire memstore into Claude Code's session lifecycle so context
surfaces automatically without relying on the assistant to remember.

## memstore-startup.mjs

**Event:** `SessionStart`

Runs `memstore tasks --surface startup` at the start of every session and
injects pending tasks as context. Tasks created with `memory_task_create`
and `surface=startup` appear automatically; completing or cancelling a task
removes the flag so it stops surfacing.

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

- Outputs a `[MEMSTORE - Pending Tasks]` block as `additionalContext` if
  any matching tasks exist.
- Exits 0 silently if the memstore binary is missing, the DB does not exist
  yet, or no tasks have `surface=startup`. Safe to deploy on a fresh install.
- High-priority tasks are prefixed with `[high]`; project name is appended
  as `(project: name)` when present.

### Example output injected into session context

```
[MEMSTORE - Pending Tasks]
• [high] Fix memory leak in feed parser (project: herald)
• Add test coverage for majordomo and herald (project: portfolio)
• Register _mail IANA service name (project: infodancer-protocol)
```
