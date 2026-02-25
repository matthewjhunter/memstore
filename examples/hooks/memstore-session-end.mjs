#!/usr/bin/env node
/**
 * memstore-session-end: Claude Code SessionEnd hook
 *
 * At session close this hook does two things:
 *   1. Records a "last active" timestamp fact in memstore for the working
 *      directory, giving a lightweight history of which projects were active.
 *   2. Prints any still-open startup tasks as a reminder to update their
 *      status before leaving.
 *
 * Setup:
 *   1. Copy this file to ~/.claude/hooks/memstore-session-end.mjs
 *   2. Set MEMSTORE_BIN below to the path of your memstore binary,
 *      or ensure memstore is on PATH.
 *   3. Add the hook to ~/.claude/settings.local.json (see README.md).
 *
 * The hook exits 0 silently if the binary is missing or the DB does not
 * exist yet, so it is safe to deploy before memstore is initialized.
 */

import { execSync } from 'child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || 'memstore';

// SessionEnd hook input arrives on stdin as JSON.
// Fields: sessionId, directory (cwd at session end).
let input = {};
try {
  const raw = await stdinText();
  input = JSON.parse(raw);
} catch {
  // No stdin or invalid JSON — proceed with empty input.
}

const cwd = input.directory || process.cwd();
const now = new Date().toISOString();

// 1. Record a "last active" fact for this working directory.
try {
  const project = cwd.split('/').filter(Boolean).pop() || cwd;
  const metadata = JSON.stringify({ directory: cwd, timestamp: now });
  execSync(
    `${MEMSTORE_BIN} store --subject "session-activity" --category "note" ` +
    `--content "Active in ${project} at ${now}" ` +
    `--metadata '${metadata}'`,
    { encoding: 'utf-8', timeout: 4000, stdio: ['pipe', 'pipe', 'pipe'] }
  );
} catch {
  // Store failed — not fatal.
}

// 2. Print open startup tasks as a reminder.
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
