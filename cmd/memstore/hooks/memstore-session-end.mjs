#!/usr/bin/env node
/**
 * memstore-session-end: Claude Code SessionEnd hook
 *
 * At session close this hook prints any still-open startup tasks as a
 * reminder to update their status before leaving.
 *
 * It used to also store a "last active in <project> at <timestamp>" fact per
 * session. That was removed in #151: the Stop hook already records every
 * session into session_hooks with the full cwd and a real timestamp column,
 * so the fact was a strictly worse duplicate (basename only) that was
 * embedded and competed with real content in search. Query session_hooks for
 * project activity instead.
 *
 * The hook exits 0 silently if the binary is missing or the DB does not
 * exist yet, so it is safe to deploy before memstore is initialized.
 */

import { execSync } from 'child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

// SessionEnd hook input arrives on stdin as JSON (sessionId, directory).
// Nothing here needs those fields any more, but stdin is still drained so the
// caller's write completes rather than hitting a closed pipe.
try {
  await stdinText();
} catch {
  // No stdin — proceed.
}

// Print open startup tasks as a reminder.
try {
  const output = execSync(`${MEMSTORE_BIN} tasks --surface startup`, {
    encoding: 'utf-8',
    timeout: 4000,
    stdio: ['pipe', 'pipe', 'pipe'],
  }).trim();

  if (output) {
    process.stderr.write(`\n[MEMSTORE] Open tasks at session end:\n${output}\n\n`);
  }
} catch {
  // tasks command failed — proceed silently.
}

// Helper: read all of stdin as a string (Node 18+).
async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}

console.log(JSON.stringify({ continue: true }));
