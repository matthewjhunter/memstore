#!/usr/bin/env node
//
// codex-notify-memstore.mjs — EXPERIMENTAL
//
// Bridge OpenAI Codex CLI's `notify` event to memstore so cross-session
// memory works inside Codex the way it does inside Claude Code.
//
// Codex invokes the configured `notify` program once per `agent-turn-complete`
// event, passing a single JSON argument with thread-id, turn-id, cwd,
// input-messages, and last-assistant-message (see the Codex config docs).
//
// This shim:
//   1. Filters for type == "agent-turn-complete".
//   2. Appends one user + one assistant entry, in Claude-Code-shaped JSONL,
//      to ~/.cache/memstore/codex-sessions/<thread-id>.jsonl.
//   3. Synthesizes a Claude Code Stop-hook payload (session_id, cwd,
//      transcript_path) and pipes it to `memstore-mcp --hook`, which
//      handles persona, upload, dedup, retry, and the optional nudge.
//
// The child is spawned detached so Codex doesn't block on the upload.
//
// Status: experimental. See examples/codex/README.md.

import { spawn } from 'node:child_process';
import { appendFileSync, mkdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

const SESSIONS_DIR = join(homedir(), '.cache', 'memstore', 'codex-sessions');
const MEMSTORE_MCP_BIN = process.env.MEMSTORE_MCP_BIN || 'memstore-mcp';

// classifyEvent inspects the parsed Codex notify payload and returns either
// {action: "skip", reason} or {action: "forward", threadId, cwd, userText,
// assistantText}. Pure function; tested directly.
export function classifyEvent(event) {
  if (event == null || typeof event !== 'object') {
    return { action: 'skip', reason: 'event is not an object' };
  }
  if (event.type !== 'agent-turn-complete') {
    return { action: 'skip', reason: `unhandled type=${event.type}` };
  }
  const threadId = event['thread-id'];
  if (!threadId || typeof threadId !== 'string') {
    return { action: 'skip', reason: 'missing thread-id' };
  }
  // thread-id ends up in a filesystem path; reject anything that isn't a
  // safe identifier. Codex emits UUIDs, so this constraint is not lossy.
  if (!/^[A-Za-z0-9._-]{1,128}$/.test(threadId)) {
    return { action: 'skip', reason: `unsafe thread-id: ${threadId}` };
  }
  const cwd = typeof event.cwd === 'string' && event.cwd.length > 0 ? event.cwd : process.cwd();
  const userText = Array.isArray(event['input-messages'])
    ? event['input-messages'].map(String).join('\n')
    : String(event['input-messages'] ?? '');
  const assistantText = String(event['last-assistant-message'] ?? '');
  return { action: 'forward', threadId, cwd, userText, assistantText };
}

// buildTranscriptLines returns the two JSONL entries (user, assistant) to
// append, shaped like Claude Code transcripts. Returned as a single string
// ending in a newline so appendFile is one syscall.
export function buildTranscriptLines(userText, assistantText) {
  const user = {
    type: 'user',
    message: { role: 'user', content: [{ type: 'text', text: userText }] },
  };
  const assistant = {
    type: 'assistant',
    message: { role: 'assistant', content: [{ type: 'text', text: assistantText }] },
  };
  return JSON.stringify(user) + '\n' + JSON.stringify(assistant) + '\n';
}

// buildHookPayload returns the JSON string fed to `memstore-mcp --hook`.
// memstored's runHookCapture reads session_id, cwd, transcript_path.
export function buildHookPayload(threadId, cwd, transcriptPath) {
  return JSON.stringify({
    session_id: threadId,
    cwd,
    transcript_path: transcriptPath,
  });
}

function forward(threadId, cwd, userText, assistantText) {
  mkdirSync(SESSIONS_DIR, { recursive: true, mode: 0o700 });
  const transcriptPath = join(SESSIONS_DIR, `${threadId}.jsonl`);
  appendFileSync(transcriptPath, buildTranscriptLines(userText, assistantText), { mode: 0o600 });

  const payload = buildHookPayload(threadId, cwd, transcriptPath);
  // Detached so we return immediately and Codex isn't held on memstored I/O.
  // Errors from the child go to stderr where Codex may log them; we never
  // fail the parent — a memstore outage shouldn't break Codex turn output.
  const child = spawn(MEMSTORE_MCP_BIN, ['--hook'], {
    stdio: ['pipe', 'ignore', 'inherit'],
    detached: true,
  });
  child.on('error', (err) => {
    process.stderr.write(`codex-notify-memstore: spawn ${MEMSTORE_MCP_BIN} failed: ${err.message}\n`);
  });
  child.stdin.end(payload);
  child.unref();
}

// main is exported so the test file can stub it; the script is launched
// only when run directly, not when imported.
export function main(argv = process.argv) {
  const arg = argv[2];
  if (!arg) {
    process.stderr.write('codex-notify-memstore: no JSON arg\n');
    return 0;
  }

  let event;
  try {
    event = JSON.parse(arg);
  } catch (err) {
    process.stderr.write(`codex-notify-memstore: invalid JSON arg: ${err.message}\n`);
    return 0;
  }

  const decision = classifyEvent(event);
  if (decision.action === 'skip') {
    // Stay silent on the common "skip" reason (non-turn-complete events)
    // so logs aren't noisy; surface the loud ones for debugging.
    if (decision.reason && !decision.reason.startsWith('unhandled type=')) {
      process.stderr.write(`codex-notify-memstore: ${decision.reason}\n`);
    }
    return 0;
  }

  try {
    forward(decision.threadId, decision.cwd, decision.userText, decision.assistantText);
  } catch (err) {
    process.stderr.write(`codex-notify-memstore: forward failed: ${err.message}\n`);
  }
  return 0;
}

// Run only when invoked as a script.
const invokedDirectly = import.meta.url === `file://${process.argv[1]}`;
if (invokedDirectly) {
  process.exit(main());
}
