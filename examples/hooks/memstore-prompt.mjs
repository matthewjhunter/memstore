#!/usr/bin/env node
/**
 * memstore-prompt: Claude Code UserPromptSubmit hook
 *
 * On each user message, runs an FTS search against memstore and injects
 * matching facts as additionalContext so relevant memories surface
 * automatically without Claude needing to initiate a search.
 *
 * Design choices:
 *   - FTS-only (no Ollama round-trip) to keep latency under ~50ms.
 *   - Skips short prompts (<5 words) and slash commands.
 *   - Caps query at 200 chars to avoid shell arg limits.
 *   - Silently exits 0 on any error so it never blocks a session.
 *
 * Setup: register in ~/.claude/settings.local.json under UserPromptSubmit.
 */

import { execSync } from 'child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || 'memstore';
const MIN_WORDS = 5;
const MAX_QUERY_CHARS = 200;
const RESULT_LIMIT = 5;

let input = {};
try {
  const raw = await stdinText();
  input = JSON.parse(raw);
} catch {
  // No stdin or invalid JSON — proceed with empty input.
}

const prompt = (input.prompt || '').trim();

// Skip trivial prompts and slash commands.
const words = prompt.split(/\s+/).filter(Boolean);
if (words.length < MIN_WORDS || prompt.startsWith('/')) {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

const query = prompt.slice(0, MAX_QUERY_CHARS);

try {
  const output = execSync(
    `${MEMSTORE_BIN} search --query ${shellQuote(query)} --limit ${RESULT_LIMIT}`,
    { encoding: 'utf-8', timeout: 4000, stdio: ['pipe', 'pipe', 'pipe'] }
  ).trim();

  if (!output) {
    console.log(JSON.stringify({ continue: true }));
    process.exit(0);
  }

  console.log(JSON.stringify({
    continue: true,
    hookSpecificOutput: {
      hookEventName: 'UserPromptSubmit',
      additionalContext: `<memstore-recall>\n\n${output}\n</memstore-recall>\n`,
    },
  }));
} catch {
  // Search failed — proceed silently.
  console.log(JSON.stringify({ continue: true }));
}

// Shell-safe quoting: wrap in single quotes, escape internal single quotes.
function shellQuote(str) {
  return "'" + str.replace(/'/g, "'\\''") + "'";
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
