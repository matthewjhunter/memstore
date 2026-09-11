#!/usr/bin/env node
/**
 * memstore-read: Claude Code PreToolUse:Read hook
 *
 * Notifies memstored of the file being read (for recall context), and
 * evaluates trigger facts against the file path to load any matching
 * context before the Read completes.
 *
 * Silently exits 0 on any error so it never blocks a Read operation.
 */

import { touchFile } from './memstore-context-touch.mjs';
import { runHookFormat } from './memstore-notices.mjs';

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
  // With the session id, eval-triggers records what it shows and leaves out
  // what this session was already shown. The notice goes out as systemMessage:
  // shown to the user, not given to the model.
  const args = ['eval-triggers', '--file', filePath, ...(sessionId ? ['--session', sessionId] : [])];
  const { context, notice } = runHookFormat(args, args, 3000);

  if (!context) {
    console.log(JSON.stringify({ continue: true }));
    process.exit(0);
  }

  console.log(JSON.stringify({
    continue: true,
    ...(notice && { systemMessage: notice }),
    hookSpecificOutput: {
      hookEventName: 'PreToolUse',
      additionalContext: `<memstore-file-context>\n${context}\n</memstore-file-context>\n`,
    },
  }));
} catch {
  // memstore missing, DB absent, or no facts -- proceed silently.
  console.log(JSON.stringify({ continue: true }));
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
