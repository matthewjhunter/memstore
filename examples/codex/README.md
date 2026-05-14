# Codex notify → memstore (experimental)

> **Status: experimental.** Wires OpenAI's Codex CLI into memstore by
> abusing Codex's per-turn `notify` callback as a substitute for the
> session-end hook that Codex doesn't expose. Works, but with the caveats
> below; not part of the supported install flow.

Claude Code has a `Stop` hook (one event per assistant turn settle), and
`memstore-mcp --hook` consumes that shape on stdin: `{session_id, cwd,
transcript_path}`. It reads the transcript JSONL from disk and posts to
memstored, which extracts facts server-side with local LLMs.

Codex has no equivalent. Its `notify` config calls an external program once
per `agent-turn-complete`, passing a single JSON argument with `type`,
`thread-id`, `turn-id`, `cwd`, `input-messages`, and
`last-assistant-message`. No transcript file on disk (just a global
`~/.codex/history.jsonl`), and no session-end event at all.

`codex-notify-memstore.mjs` bridges the two:

1. Filters for `type === "agent-turn-complete"`.
2. Appends a Claude-Code-shaped JSONL pair (user + assistant) to
   `~/.cache/memstore/codex-sessions/<thread-id>.jsonl`.
3. Synthesizes a Claude Code Stop-hook payload and pipes it to
   `memstore-mcp --hook`.
4. Detaches the child so Codex doesn't block on the upload.

No changes to `memstore-mcp` or memstored — the server treats Codex turns
exactly like Claude turns.

## Install

```bash
# 1. Make sure memstore-mcp is on PATH (or set MEMSTORE_MCP_BIN).
which memstore-mcp

# 2. Drop the shim somewhere stable. Out-of-tree is fine; this examples/
#    location is canonical, but ~/.codex/hooks/ is more idiomatic for Codex.
install -Dm755 examples/codex/codex-notify-memstore.mjs \
    ~/.codex/hooks/codex-notify-memstore.mjs

# 3. Register it in Codex's config.
cat >> ~/.codex/config.toml <<'EOF'
notify = ["node", "/home/<you>/.codex/hooks/codex-notify-memstore.mjs"]
EOF
```

Verify by running the shim by hand:

```bash
node ~/.codex/hooks/codex-notify-memstore.mjs '{
  "type": "agent-turn-complete",
  "thread-id": "smoke-test-1",
  "cwd": "/tmp",
  "input-messages": ["hello"],
  "last-assistant-message": "hi"
}'
# Should create ~/.cache/memstore/codex-sessions/smoke-test-1.jsonl
# and spawn memstore-mcp --hook in the background.
```

## What memstore sees

The thread-id becomes the memstored `session_id`. Each turn appends to the
per-thread transcript and re-posts it. The Go binary's per-session state
file (in `~/.cache/memstore/sessions/`) tracks message count and emits the
8-message "store your decisions" nudge to stderr — Codex's TUI will not
surface that nudge as a system message the way Claude Code does, so the
nudge is best-effort logging only inside Codex.

Persona is set by the OS user (`os/user.Current()` in the Go binary), so
facts attribute to the right person even though Codex has no concept of
the memstored user model.

## Caveats

**Per-turn re-upload.** Codex has no session-end event, so the shim fires on
every turn and re-uploads the *growing* transcript each time. memstored
dedups at the fact level, so no duplicate memories accumulate, but the
upload bandwidth is wasted. Three mitigations, in increasing effort:

- **Default: accept it.** Codex sessions tend to be short; the wasted I/O
  is small.
- **Debounce in the shim.** Append every turn, but only call
  `memstore-mcp --hook` every N turns or after T seconds of idle. Add a
  `.pending` sidecar file and a `setTimeout` shaped chain. Not implemented.
- **Native Codex mode in `memstore-mcp`.** Add a `--codex-notify` flag that
  takes Codex's JSON shape directly and tracks an incremental cursor
  server-side. Cleanest, but a real change. Skip until per-turn cost bites.

**No nudge feedback loop.** The store-your-decisions nudge writes to
stderr; Codex doesn't render it as a system message the way Claude Code
does. The nudge is passive — Codex won't tell the model "you should call
`memory_store` now."

**Failure mode is silent.** A memstored outage or a bad spawn produces a
single stderr line and Codex continues. The shim never fails the
parent — a memory-system problem must not break Codex output. Check
`~/.cache/memstore/codex-sessions/` and memstored logs if memories aren't
showing up.

**Thread-id pinning.** The shim rejects thread-ids that aren't
`[A-Za-z0-9._-]{1,128}` because they end up as filesystem path components.
Codex emits UUIDs, so this is not lossy in practice, but a custom-shaped
thread-id from a Codex variant would be skipped — visible in stderr as
`unsafe thread-id: ...`.

**Recursion / paid-LLM rule compliance.** Extraction happens server-side
in memstored using local Gemma/Qwen via Lemonade or Ollama. No paid LLMs
are called from this path. The shim itself does not call any model
provider, so it cannot recurse. Aligned with the rules in
`memory/feedback_no_post_session_hooks.md` and
`memory/feedback_llms_in_memstore.md`.

## Tests

```bash
node --test examples/codex/codex-notify-memstore.test.mjs
```

Unit tests cover only the pure logic — event classification, transcript
line shape, hook-payload shape. The spawn / filesystem path is integration
work and would need a live memstored to test; do that by hand using the
smoke-test invocation above.

## Why this is in `examples/`

The shim is small, single-file, has no memstore-side dependencies, and
hasn't been used in production for long enough to bake into the supported
`memstore setup` flow. If it proves stable, the obvious next step is
either to fold it into the Go binary as `memstore-mcp --codex-notify`
(removing Node from the deploy footprint) or to grow `memstore setup` to
detect a Codex install and offer to install the shim.
