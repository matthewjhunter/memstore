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

import { execSync } from 'child_process';
import { touchFile } from './memstore-context-touch.mjs';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

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
  let notice = '';
  try {
    // With the session id, eval-triggers records what it shows and leaves out
    // what this session was already shown. --format hook returns the block and
    // a notice for the user, which goes out as systemMessage: shown to the user,
    // not given to the model.
    const sessionArg = sessionId ? ` --session ${shellQuote(sessionId)}` : '';
    const out = execSync(
      `${MEMSTORE_BIN} eval-triggers --file ${shellQuote(filePath)}${sessionArg} --format hook`,
      { encoding: 'utf-8', timeout: 3000, stdio: ['pipe', 'pipe', 'pipe'] }
    ).trim();
    ({ context, notice } = parseHookOutput(out));
  } catch { /* no triggers */ }

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

// parseHookOutput reads `--format hook` output: {context, notice}. Anything
// else is nothing to inject.
function parseHookOutput(out) {
  try {
    const parsed = JSON.parse(out);
    return {
      context: typeof parsed?.context === 'string' ? parsed.context.trim() : '',
      notice: typeof parsed?.notice === 'string' ? parsed.notice : '',
    };
  } catch {
    return { context: '', notice: '' };
  }
}

function shellQuote(str) {
  return "'" + str.replace(/'/g, "'\\''") + "'";
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
