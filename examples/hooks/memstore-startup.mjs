#!/usr/bin/env node
/**
 * memstore-startup: Claude Code SessionStart hook
 *
 * Injects pending tasks (surface=startup) into session context so they
 * appear automatically at the start of every Claude Code session.
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

try {
  const output = execSync(`${MEMSTORE_BIN} tasks --surface startup`, {
    encoding: 'utf-8',
    timeout: 4000,
    stdio: ['pipe', 'pipe', 'pipe'],
  }).trim();

  if (!output) {
    console.log(JSON.stringify({ continue: true }));
    process.exit(0);
  }

  console.log(JSON.stringify({
    continue: true,
    hookSpecificOutput: {
      hookEventName: 'SessionStart',
      additionalContext: `<session-restore>\n\n${output}\n\n</session-restore>\n\n---\n`,
    },
  }));
} catch {
  // memstore not installed, DB missing, or command failed — proceed silently.
  console.log(JSON.stringify({ continue: true }));
}
