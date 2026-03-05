#!/usr/bin/env node
/**
 * memstore-read: Claude Code PreToolUse:Read hook
 *
 * When Claude reads a file, injects file-surface and symbol-surface facts
 * for that file (produced by memory_learn) so constraints and known patterns
 * are visible before any changes are made. Also evaluates trigger facts
 * whose file patterns match the path, loading subsystem context automatically.
 *
 * Silently exits 0 on any error so it never blocks a Read operation.
 */

import { execSync } from 'child_process';
import { touchFile } from './memstore-context-touch.mjs';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || 'memstore';

let input = {};
try {
  const raw = await stdinText();
  input = JSON.parse(raw);
} catch {
  // No stdin or invalid JSON.
}

const filePath = input.tool_input?.file_path || '';
const sessionId = input.session_id || input.sessionId || '';

// Notify memstored about the file access (fire-and-forget for recall context).
touchFile(sessionId, filePath);

// Only inject for absolute paths (skip relative paths, notebooks, etc.)
if (!filePath || !filePath.startsWith('/')) {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

try {
  let context = '';

  // File-surface and symbol-surface facts.
  try {
    const fileOutput = execSync(
      `${MEMSTORE_BIN} list-file --file ${shellQuote(filePath)}`,
      { encoding: 'utf-8', timeout: 3000, stdio: ['pipe', 'pipe', 'pipe'] }
    ).trim();
    if (fileOutput) context += fileOutput;
  } catch { /* no file facts */ }

  // Trigger evaluation — load context when file matches trigger patterns.
  try {
    const triggerOutput = execSync(
      `${MEMSTORE_BIN} eval-triggers --file ${shellQuote(filePath)}`,
      { encoding: 'utf-8', timeout: 3000, stdio: ['pipe', 'pipe', 'pipe'] }
    ).trim();
    if (triggerOutput) {
      if (context) context += '\n\n';
      context += triggerOutput;
    }
  } catch { /* no triggers */ }

  if (!context) {
    console.log(JSON.stringify({ continue: true }));
    process.exit(0);
  }

  console.log(JSON.stringify({
    continue: true,
    hookSpecificOutput: {
      hookEventName: 'PreToolUse',
      additionalContext: `<memstore-file-context>\n${context}\n</memstore-file-context>\n`,
    },
  }));
} catch {
  // memstore missing, DB absent, or no facts — proceed silently.
  console.log(JSON.stringify({ continue: true }));
}

function shellQuote(str) {
  return "'" + str.replace(/'/g, "'\\''") + "'";
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
