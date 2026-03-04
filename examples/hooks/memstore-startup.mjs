#!/usr/bin/env node
/**
 * memstore-startup: Claude Code SessionStart hook
 *
 * Injects pending tasks (surface=startup) and drift reports into session
 * context so they appear automatically at the start of every Claude Code
 * session.
 *
 * Setup:
 *   1. Copy this file to ~/.claude/hooks/memstore-startup.mjs
 *   2. Set MEMSTORE_BIN below to the path of your memstore binary,
 *      or ensure memstore is on PATH.
 *   3. Add the hook to ~/.claude/settings.local.json (see README.md).
 *
 * The hook exits 0 silently if the binary is missing or the DB does not
 * exist yet, so it is safe to deploy before memstore is initialized.
 */

import { execSync } from 'child_process';

// Use full path if memstore is not on the hook's PATH (common with Go binaries).
// Change this to 'memstore' if ~/go/bin is reliably in your environment.
const MEMSTORE_BIN = process.env.MEMSTORE_BIN || 'memstore';

// Collect output sections.
const sections = [];

// 1. Pending tasks.
try {
  const tasks = execSync(`${MEMSTORE_BIN} tasks --surface startup`, {
    encoding: 'utf-8',
    timeout: 4000,
    stdio: ['pipe', 'pipe', 'pipe'],
  }).trim();
  if (tasks) {
    sections.push(tasks);
  }
} catch {
  // memstore not installed, DB missing, or command failed — skip tasks silently.
}

// 2. Drift check — detect stale facts whose source files changed in git.
try {
  // Detect current git repo root from cwd.
  const repoRoot = execSync('git rev-parse --show-toplevel', {
    encoding: 'utf-8',
    timeout: 2000,
    stdio: ['pipe', 'pipe', 'pipe'],
  }).trim();

  if (repoRoot) {
    const drift = execSync(
      `${MEMSTORE_BIN} check-drift --repo ${JSON.stringify(repoRoot)} --since-days 7`,
      {
        encoding: 'utf-8',
        timeout: 4000,
        stdio: ['pipe', 'pipe', 'pipe'],
      }
    ).trim();
    if (drift) {
      sections.push(drift);
    }
  }
} catch {
  // Not in a git repo, memstore missing, or timeout — skip drift silently.
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
