#!/usr/bin/env node
/**
 * memstore-prompt: Claude Code UserPromptSubmit hook
 *
 * On each user message, sends the prompt and context to memstored's /v1/recall
 * endpoint, which handles keyword extraction, search, and curation server-side.
 *
 * Design choices:
 *   - Single HTTP call to memstored instead of multiple CLI spawns.
 *   - Server-side IDF keyword extraction for better relevance.
 *   - Skips short prompts (<5 words) and slash commands.
 *   - Silently exits 0 on any error so it never blocks a session.
 */

const MEMSTORED_URL = process.env.MEMSTORED_URL || 'http://localhost:8230';
const MEMSTORED_API_KEY = process.env.MEMSTORED_API_KEY || '';
const MIN_WORDS = 5;
const RECALL_LIMIT = 5;
const RECALL_BUDGET = 2000;

let input = {};
try {
  const raw = await stdinText();
  input = JSON.parse(raw);
} catch {
  // No stdin or invalid JSON — proceed with empty input.
}

const prompt = (input.prompt || '').trim();
const sessionId = input.session_id || input.sessionId || '';
const cwd = input.cwd || input.directory || '';

// Skip trivial prompts and slash commands.
const words = prompt.split(/\s+/).filter(Boolean);
if (words.length < MIN_WORDS || prompt.startsWith('/')) {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

try {
  const body = JSON.stringify({
    prompt,
    session_id: sessionId,
    cwd,
    limit: RECALL_LIMIT,
    budget: RECALL_BUDGET,
  });

  const headers = { 'Content-Type': 'application/json' };
  if (MEMSTORED_API_KEY) {
    headers['Authorization'] = `Bearer ${MEMSTORED_API_KEY}`;
  }

  const resp = await fetch(`${MEMSTORED_URL}/v1/recall`, {
    method: 'POST',
    headers,
    body,
    signal: AbortSignal.timeout(3000),
  });

  if (!resp.ok) {
    console.log(JSON.stringify({ continue: true }));
    process.exit(0);
  }

  const result = await resp.json();
  const context = (result.context || '').trim();

  if (!context) {
    console.log(JSON.stringify({ continue: true }));
    process.exit(0);
  }

  console.log(JSON.stringify({
    continue: true,
    hookSpecificOutput: {
      hookEventName: 'UserPromptSubmit',
      additionalContext: `<memstore-recall>\n${context}\n</memstore-recall>\n`,
    },
  }));
} catch {
  // memstored unavailable — proceed silently.
  console.log(JSON.stringify({ continue: true }));
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
