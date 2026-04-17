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

// Drain any stdin provided by the SessionStart hook so it doesn't block.
try {
  await stdinText();
} catch {
  // No stdin or read error — proceed.
}

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

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
