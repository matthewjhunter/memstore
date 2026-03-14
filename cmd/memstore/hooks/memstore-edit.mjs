#!/usr/bin/env node
/**
 * memstore-edit: Claude Code PreToolUse:Edit hook
 *
 * When Claude edits a file, injects file-surface facts and, if a symbol
 * can be inferred from the edit context, symbol-surface facts for that
 * specific function/type.
 *
 * Silently exits 0 on any error so it never blocks an Edit operation.
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

// Only inject for absolute paths.
if (!filePath || !filePath.startsWith('/')) {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

// Try to infer a symbol name from the old_string context (first func/type/method
// declaration in the edited text, if any). This is best-effort; no symbol is fine.
const symbolName = inferSymbol(input.tool_input?.old_string || '');

try {
  let context = '';

  // File-surface and symbol-surface facts.
  try {
    const cmd = symbolName
      ? `${MEMSTORE_BIN} list-file --file ${shellQuote(filePath)} --symbol ${shellQuote(symbolName)}`
      : `${MEMSTORE_BIN} list-file --file ${shellQuote(filePath)}`;

    const fileOutput = execSync(cmd, {
      encoding: 'utf-8',
      timeout: 3000,
      stdio: ['pipe', 'pipe', 'pipe'],
    }).trim();
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

/**
 * Infer a symbol name from a code snippet by looking for the first
 * function/method/type declaration.
 */
function inferSymbol(code) {
  if (!code) return '';
  // Go: func Name, func (r Recv) Name, type Name
  const goMatch = code.match(/(?:^|\n)\s*(?:func\s+(?:\(\w[^)]*\)\s+)?(\w+)|type\s+(\w+))/);
  if (goMatch) return goMatch[1] || goMatch[2] || '';
  // Python: def name, class Name
  const pyMatch = code.match(/(?:^|\n)\s*(?:def|class)\s+(\w+)/);
  if (pyMatch) return pyMatch[1] || '';
  return '';
}

function shellQuote(str) {
  return "'" + str.replace(/'/g, "'\\''") + "'";
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
