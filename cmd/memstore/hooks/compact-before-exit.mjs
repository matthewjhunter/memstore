#!/usr/bin/env node
// Compact-before-exit reminder hook.
//
// Runs on UserPromptSubmit. When the user types /exit, /quit, or /clear on a
// substantial session that has not been compacted recently, this hook blocks
// the prompt with a reminder to run /compact first — the model-generated
// compact_summary then lands in memstore via the PostCompact hook, instead of
// the daemon's tail-truncated fallback.
//
// Escape paths (so the hook never traps the user):
//  1. Recent /compact (within COMPACT_RECENCY_MS) — passes through.
//  2. Double-tap — the second consecutive /exit/quit/clear passes through;
//     the gate is one-shot per session and resets on any non-targeted prompt.
//  3. MEMSTORE_COMPACT_GATE=off in the environment — disables the hook.
//
// Whether slash commands fire UserPromptSubmit is officially undocumented; if
// /exit bypasses hooks entirely, this never fires and is harmless.

import { readFileSync, writeFileSync, statSync, mkdirSync } from 'fs';
import { homedir } from 'os';
import { join } from 'path';

if (process.env.MEMSTORE_COMPACT_GATE === 'off') process.exit(0);

const SUBSTANTIVE_TOKEN_THRESHOLD = 50_000;
const COMPACT_RECENCY_MS = 30 * 60 * 1000;
const APPROX_BYTES_PER_TOKEN = 4;
const TARGETED = new Set(['/exit', '/quit', '/clear']);

let input = {};
try {
  input = JSON.parse(await stdinText());
} catch {
  process.exit(0);
}

const prompt = (input.prompt || '').trim().toLowerCase();
const sessionId = input.session_id || '';
const sessionsDir = join(homedir(), '.cache', 'memstore', 'sessions');
const stateFile = sessionId ? join(sessionsDir, `${sessionId}.json`) : '';

let state = {};
if (stateFile) {
  try { state = JSON.parse(readFileSync(stateFile, 'utf8')); } catch {}
}

// Non-targeted prompt clears any pending gate (so a /clear after some real
// work doesn't get bypassed by a stale flag from earlier in the session).
if (!TARGETED.has(prompt)) {
  if (state.exit_gate_pending) {
    state.exit_gate_pending = false;
    writeState();
  }
  process.exit(0);
}

// Targeted prompt — decide whether to block.

// Substantive-size gate: small sessions exit silently.
let bytes = 0;
const transcriptPath = input.transcript_path;
if (transcriptPath) {
  try { bytes = statSync(transcriptPath).size; } catch {}
}
const tokens = bytes / APPROX_BYTES_PER_TOKEN;
if (tokens < SUBSTANTIVE_TOKEN_THRESHOLD) process.exit(0);

// Recent compact — already saved a high-quality summary, pass through.
const lastCompactMs = state.last_compacted_at ? Date.parse(state.last_compacted_at) : 0;
if (lastCompactMs && (Date.now() - lastCompactMs) < COMPACT_RECENCY_MS) process.exit(0);

// Double-tap: previous targeted prompt was blocked; user is confirming.
if (state.exit_gate_pending) {
  state.exit_gate_pending = false;
  writeState();
  process.exit(0);
}

// First targeted attempt — block and arm the double-tap.
state.exit_gate_pending = true;
writeState();

const tokensK = Math.round(tokens / 1000);
const verb = prompt === '/clear' ? 'clear' : 'exit';
const reason =
  `This session has ~${tokensK}K tokens that have not been compacted. ` +
  `Run /compact first so the model produces a high-quality summary that PostCompact stores in memstore, ` +
  `then ${prompt} to ${verb}. ` +
  `Re-run ${prompt} to ${verb} without compacting (gate cleared).`;

console.log(JSON.stringify({ decision: 'block', reason }));
process.exit(0);

function writeState() {
  if (!stateFile) return;
  try {
    mkdirSync(sessionsDir, { recursive: true });
    state.session_id = sessionId;
    writeFileSync(stateFile, JSON.stringify(state, null, 2));
  } catch {}
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
