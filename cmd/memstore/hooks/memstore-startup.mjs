#!/usr/bin/env node
/**
 * memstore-startup: Claude Code SessionStart hook
 *
 * Injects pending startup-surface tasks at session start. Everything else,
 * the homelab inventory included, reaches a session through the per-prompt
 * recall pipeline (UserPromptSubmit hook) when a prompt is about it; a fixed
 * search pinned into every session arrives unasked and unrelated to the work.
 */

import { runHookFormat } from './memstore-notices.mjs';

// The number of tasks a session opens with. Every pending task used to be
// injected -- 190 of them, past the hook's context cap, so the model saw a
// truncated preview of an arbitrary prefix. The list is now this project's
// tasks only, one title line each, so ten fit where five full bodies did not;
// the rest are one `memstore tasks --project-only` away.
const STARTUP_TASK_LIMIT = Number(process.env.MEMSTORE_STARTUP_TASKS || 10);

// Read the SessionStart payload for the working directory and session id;
// drain stdin either way.
let cwd = '';
let sessionId = '';
try {
  const input = JSON.parse(await stdinText());
  cwd = input.cwd || input.directory || '';
  sessionId = input.session_id || input.sessionId || '';
} catch {
  // No stdin or invalid JSON -- proceed without a cwd.
}

// This project's open tasks, and only this project's: another project's work
// pushed into an unrelated session is the #221 failure. The CLI renders the
// list -- framing, then each title inside the fence -- because the fence and
// its neutralizer are Go; this hook passes the block through untouched. With
// the session id, the CLI records which tasks the session was shown.
// The notice goes out as systemMessage: shown to the user, not given to the
// model. A binary from before notices gets --format context instead. A missing
// binary, an unreachable daemon or a failed command is nothing to inject.
const args = [
  'tasks', '--surface', 'startup', '--limit', String(STARTUP_TASK_LIMIT), '--project-only',
  '--cwd', cwd || process.cwd(), ...(sessionId ? ['--session', sessionId] : []),
];
const { context: tasks, notice } = runHookFormat(args, [...args, '--format', 'context'], 4000);

if (!tasks) {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

console.log(JSON.stringify({
  continue: true,
  ...(notice && { systemMessage: notice }),
  hookSpecificOutput: {
    hookEventName: 'SessionStart',
    additionalContext: `<memstore-tasks>\n${tasks}\n</memstore-tasks>`,
  },
}));

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
