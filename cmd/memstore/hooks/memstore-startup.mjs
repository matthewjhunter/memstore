#!/usr/bin/env node
/**
 * memstore-startup: Claude Code SessionStart hook
 *
 * Injects pending startup-surface tasks and homelab host inventory at
 * session start. Project context is handled via the per-prompt recall
 * pipeline (UserPromptSubmit hook), which applies a project-surface boost
 * when the CWD matches a fact's project_path.
 */

import { execSync } from 'child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

// The number of tasks a session opens with. Every pending task used to be
// injected -- 190 of them, past the hook's context cap, so the model saw a
// truncated preview of an arbitrary prefix. The daemon picks the few that
// matter for this repo (memstore tasks --limit, TaskSelector); the rest are
// one `memstore tasks` away.
const STARTUP_TASK_LIMIT = Number(process.env.MEMSTORE_STARTUP_TASKS || 5);

// Read the SessionStart payload for the working directory; drain stdin either way.
let cwd = '';
try {
  const input = JSON.parse(await stdinText());
  cwd = input.cwd || input.directory || '';
} catch {
  // No stdin or invalid JSON — proceed without a cwd.
}

const sections = [];

// 1. Pending startup tasks, the top few for this repo.
try {
  const tasks = execSync(`${MEMSTORE_BIN} tasks --surface startup --limit ${STARTUP_TASK_LIMIT} --cwd ${shellQuote(cwd || process.cwd())}`, {
    encoding: 'utf-8',
    timeout: 4000,
    stdio: ['pipe', 'pipe', 'pipe'],
  }).trim();

  if (tasks) sections.push(tasks);
} catch {
  // Binary missing, DB absent, or command failed — proceed silently.
}

// 2. Homelab system inventory (always inject so hosts/IPs are available without asking).
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

function shellQuote(s) {
  return `'${String(s).replace(/'/g, `'\\''`)}'`;
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
