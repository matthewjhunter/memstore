#!/usr/bin/env node
// Stop hook: forwards Claude Code session events to memstored, nudges
// the model to store facts, and uploads completed session transcripts
// to trigger the extraction + hint generation pipeline.
//
// 1. POSTs the raw hook payload to memstored via `memstore-mcp --hook`.
// 2. Writes ~/.cache/memstore/sessions/<session_id>.json state file.
//    One file per session prevents concurrent sessions from overwriting each other.
// 3. Tracks message count per session. After NUDGE_THRESHOLD messages, creates
//    a context hint reminding the model to store unstored decisions/repos/tasks.
// 4. Uploads one completed (non-current) session transcript to memstored per
//    invocation, draining the backlog gradually. This triggers the ExtractQueue
//    pipeline (fact extraction, A-MEM linking, hint generation) server-side.
//
// Note: transcript upload was originally in memstore-mcp's shutdown handler
// (uploadTranscriptOnShutdown), but Claude Code kills MCP servers with SIGKILL
// so the cleanup code after server.Run() never executes. Moving it here ensures
// transcripts are reliably uploaded on the next session's first Stop event.

import { spawnSync } from 'child_process';
import { readFileSync, writeFileSync, mkdirSync, readdirSync, renameSync, statSync } from 'fs';
import { homedir } from 'os';
import { join } from 'path';

const MEMSTORED_URL = process.env.MEMSTORED_URL || '__MEMSTORED_URL__';
const NUDGE_THRESHOLD = 8;
const MAX_TRANSCRIPT_BYTES = 5 * 1024 * 1024; // 5 MB — skip oversized transcripts

const sessionsDir = join(homedir(), '.cache', 'memstore', 'sessions');

const stdinData = await stdinText();
let hook;
try {
  hook = JSON.parse(stdinData);
} catch {
  process.stderr.write('stop-hook: invalid JSON on stdin\n');
  process.exit(0);
}

// 1. POST hook payload to memstored (fire and forget — don't block Claude).
spawnSync('memstore-mcp', ['--hook'], {
  input: stdinData,
  encoding: 'utf8',
  stdio: ['pipe', 'ignore', 'pipe'],
});

// 2+3. Update per-session state file and nudge if needed.
if (hook.session_id) {
  mkdirSync(sessionsDir, { recursive: true });

  const stateFile = join(sessionsDir, `${hook.session_id}.json`);
  let state = {};
  try {
    state = JSON.parse(readFileSync(stateFile, 'utf8'));
  } catch { /* first message or missing file */ }

  state.message_count = (state.message_count || 0) + 1;
  state.session_id = hook.session_id;
  if (hook.transcript_path) state.transcript_path = hook.transcript_path;
  if (hook.cwd) state.cwd = hook.cwd;

  // Create a store-nudge hint after threshold messages if not already done.
  if (state.message_count >= NUDGE_THRESHOLD && !state.nudged) {
    state.nudged = true;
    try {
      await fetch(`${MEMSTORED_URL}/v1/context/hints`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: hook.session_id,
          cwd: state.cwd || '',
          turn_index: state.message_count,
          hint_text: 'This session has had several exchanges. If architectural decisions were made, new repos created, or work was deferred, store them now using memory_store or memory_store_batch. Check what was discussed and whether anything should persist for future sessions.',
          relevance: 0.8,
          desirability: 0.9,
        }),
        signal: AbortSignal.timeout(2000),
      });
    } catch { /* non-fatal — nudge is best-effort */ }
  }

  writeFileSync(stateFile, JSON.stringify(state, null, 2));
}

// 4. Upload one completed session transcript to memstored.
//    Only processes sessions other than the current one (whose transcript is
//    still being written). Drains one per Stop event to stay within timeout.
await uploadOneCompletedSession(hook.session_id);

// --- helpers ---

async function uploadOneCompletedSession(currentSessionID) {
  let entries;
  try {
    entries = readdirSync(sessionsDir);
  } catch {
    return;
  }

  for (const entry of entries) {
    if (!entry.endsWith('.json')) continue;

    const statePath = join(sessionsDir, entry);
    let state;
    try {
      state = JSON.parse(readFileSync(statePath, 'utf8'));
    } catch { continue; }

    // Skip the current session — its transcript is still being written.
    if (state.session_id === currentSessionID) continue;
    if (!state.transcript_path) continue;

    // Check transcript exists and isn't too large for the hook timeout budget.
    let size;
    try {
      size = statSync(state.transcript_path).size;
    } catch {
      // Transcript file missing — mark as done so we don't retry forever.
      try { renameSync(statePath, statePath.replace('.json', '.done')); } catch {}
      continue;
    }
    if (size > MAX_TRANSCRIPT_BYTES) {
      // Too large for hook timeout — spawn memstore-mcp --transcript detached.
      try {
        const { spawn } = await import('child_process');
        const child = spawn('memstore-mcp', ['--transcript', state.transcript_path], {
          detached: true,
          stdio: 'ignore',
        });
        child.unref();
        renameSync(statePath, statePath.replace('.json', '.done'));
      } catch { /* leave for next attempt */ }
      return; // one per invocation
    }

    // Atomic claim — prevents concurrent uploads from parallel sessions.
    const uploading = statePath.replace('.json', '.uploading');
    try {
      renameSync(statePath, uploading);
    } catch { continue; }

    try {
      const content = readFileSync(state.transcript_path, 'utf8');
      const resp = await fetch(`${MEMSTORED_URL}/v1/sessions/transcript`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: state.session_id,
          cwd: state.cwd || '',
          content,
        }),
        signal: AbortSignal.timeout(3000),
      });
      if (resp.ok) {
        renameSync(uploading, statePath.replace('.json', '.done'));
      } else {
        renameSync(uploading, statePath); // restore for retry
      }
    } catch {
      try { renameSync(uploading, statePath); } catch {} // restore for retry
    }
    return; // one per invocation
  }
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
