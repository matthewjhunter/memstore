#!/usr/bin/env node
// PostCompact hook shim — forwards the PostCompact event to memstore-mcp.
//
// Claude Code fires PostCompact after /compact (manual) or auto-compaction
// finishes, with the model-generated summary in the `compact_summary` field.
// memstore-mcp --post-compact stores it as a fact attributed to the cwd's
// enclosing repo, so the summary the active session's model just produced
// becomes a memstore-searchable artifact without re-summarization.
//
// We capture stdin into a tee log before forwarding so failures upstream
// of memstore-mcp (PATH lookup, spawn, stdin truncation) are still
// observable in ~/.cache/memstore/debug/post-compact-hook.jsonl.

import { spawnSync } from 'child_process';
import { mkdirSync, appendFileSync, writeFileSync } from 'fs';
import { homedir } from 'os';
import { join } from 'path';
import { readFileSync } from 'fs';

const debugDir = join(homedir(), '.cache', 'memstore', 'debug');
const log = {
  timestamp: new Date().toISOString(),
  pid: process.pid,
  cwd: process.cwd(),
  argv: process.argv,
};

let stdin = '';
try {
  stdin = readFileSync(0, 'utf8');
} catch (err) {
  log.stdin_error = String(err && err.message || err);
}
log.stdin_bytes = stdin.length;
log.stdin_preview = stdin.length > 4096 ? stdin.slice(0, 4096) + '...[truncated]' : stdin;

const result = spawnSync('memstore-mcp', ['--post-compact'], {
  input: stdin,
  encoding: 'utf8',
});
log.spawn_status = result.status;
log.spawn_signal = result.signal;
log.spawn_error = result.error ? String(result.error.message) : undefined;
log.subprocess_stderr = (result.stderr || '').slice(0, 2000);
log.subprocess_stdout = (result.stdout || '').slice(0, 2000);

try {
  mkdirSync(debugDir, { recursive: true, mode: 0o700 });
  appendFileSync(join(debugDir, 'post-compact-hook.jsonl'), JSON.stringify(log) + '\n', { mode: 0o600 });
  writeFileSync(join(debugDir, 'last-post-compact-hook.json'), JSON.stringify(log, null, 2), { mode: 0o600 });
} catch {
  // Debug logging is best-effort.
}

process.exit(result.status ?? 0);
