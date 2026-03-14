#!/usr/bin/env node
/**
 * memstore-startup: Claude Code SessionStart hook
 *
 * Injects two types of context at session start:
 *   1. Pending tasks with surface=startup.
 *   2. Project facts for the current working directory's project name.
 */

import { execSync } from 'child_process';
import { basename } from 'path';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

// Try to get directory from stdin (SessionStart may provide it).
let cwd = process.cwd();
try {
  const raw = await stdinText();
  const input = JSON.parse(raw);
  cwd = input.directory || input.cwd || cwd;
} catch {
  // No stdin or invalid JSON — use process.cwd().
}

const project = basename(cwd);
const sections = [];

// 1. Pending startup tasks.
try {
  const tasks = execSync(`${MEMSTORE_BIN} tasks --surface startup`, {
    encoding: 'utf-8',
    timeout: 4000,
    stdio: ['pipe', 'pipe', 'pipe'],
  }).trim();

  if (tasks) sections.push(tasks);
} catch {
  // Binary missing, DB absent, or command failed — proceed silently.
}

// 2. Project-surface and package-surface facts for the current working directory.
try {
  const facts = execSync(
    `${MEMSTORE_BIN} list-project --cwd ${shellQuote(cwd)}`,
    { encoding: 'utf-8', timeout: 4000, stdio: ['pipe', 'pipe', 'pipe'] }
  ).trim();

  if (facts) {
    sections.push(`[MEMSTORE - Project: ${project}]\n${facts}`);
  }
} catch {
  // list-project failed (binary may need rebuild) — proceed silently.
}

// 3. Homelab system inventory (always inject so hosts/IPs are available without asking).
try {
  const hosts = execSync(
    `${MEMSTORE_BIN} search -query "homelab hosts" -limit 1`,
    { encoding: 'utf-8', timeout: 4000, stdio: ['pipe', 'pipe', 'pipe'] }
  ).trim();

  if (hosts) {
    sections.push(`[HOMELAB SYSTEMS]\n${hosts}`);
  }
} catch {
  // Search failed — proceed silently.
}

if (sections.length === 0) {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

console.log(JSON.stringify({
  continue: true,
  hookSpecificOutput: {
    hookEventName: 'SessionStart',
    additionalContext: `<session-restore>\n\n${sections.join('\n\n')}\n\n</session-restore>\n\n---\n`,
  },
}));

function shellQuote(str) {
  return "'" + str.replace(/'/g, "'\\''") + "'";
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
