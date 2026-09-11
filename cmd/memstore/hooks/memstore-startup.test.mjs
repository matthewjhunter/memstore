// Run: node --test cmd/memstore/hooks/memstore-startup.test.mjs
//
// The startup hook runs as a subprocess against a stub memstore binary that
// records its argv. Pinned: it asks for a bounded selection for the session's
// working directory, not the whole task list.

import { describe, it, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HOOK = join(dirname(fileURLToPath(import.meta.url)), 'memstore-startup.mjs');

let dir, stubBin, argvLog, lastStdout;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-startup-hook-'));
  stubBin = join(dir, 'memstore-stub');
  argvLog = join(dir, 'argv.log');
  // Records argv; answers `tasks` with a marker so the wrapping can be checked.
  writeFileSync(stubBin, `#!/bin/sh\nprintf '%s\\n' "$*" >> ${argvLog}\n[ "$1" = tasks ] && printf '%s\\n' '{"context":"TASK-BLOCK","notice":"memstore: open tasks for r"}'\nexit 0\n`);
  chmodSync(stubBin, 0o755);
});

after(() => rmSync(dir, { recursive: true, force: true }));
beforeEach(() => rmSync(argvLog, { force: true }));

function runHook(input, env = {}) {
  const result = spawnSync(process.execPath, [HOOK], {
    input: JSON.stringify(input),
    encoding: 'utf-8',
    env: { ...process.env, MEMSTORE_BIN: stubBin, ...env },
    timeout: 10000,
  });
  lastStdout = result.stdout;
  return existsSync(argvLog) ? readFileSync(argvLog, 'utf-8').trim().split('\n') : [];
}

describe('memstore-startup', () => {
  it("asks for this project's tasks, rendered as a fenced list", () => {
    const calls = runHook({ session_id: 's-1', cwd: "/home/m/git/it's here" });
    const tasks = calls.find(c => c.startsWith('tasks '));
    assert.ok(tasks, `no tasks call: ${calls.join(' | ')}`);
    assert.match(tasks, /--surface startup/);
    assert.match(tasks, /--limit 10/);
    assert.match(tasks, /--project-only/);
    assert.match(tasks, /--format hook/);
    assert.match(tasks, /--cwd \/home\/m\/git\/it's here/);
    assert.match(tasks, /--session s-1/);
  });

  it('asks without a session id when the payload has none', () => {
    const calls = runHook({ cwd: '/tmp/r' });
    assert.doesNotMatch(calls.find(c => c.startsWith('tasks ')) ?? '', /--session/);
  });

  it('injects the list in a memstore-tasks block', () => {
    runHook({ cwd: '/tmp/r' });
    const ctx = JSON.parse(lastStdout).hookSpecificOutput?.additionalContext ?? '';
    assert.equal(ctx, '<memstore-tasks>\nTASK-BLOCK\n</memstore-tasks>');
  });

  it('shows the notice to the user as systemMessage', () => {
    runHook({ cwd: '/tmp/r' });
    assert.equal(JSON.parse(lastStdout).systemMessage, 'memstore: open tasks for r');
  });

  it('honours MEMSTORE_STARTUP_TASKS', () => {
    const calls = runHook({ cwd: '/tmp/r' }, { MEMSTORE_STARTUP_TASKS: '3' });
    assert.match(calls.find(c => c.startsWith('tasks ')) ?? '', /--limit 3/);
  });

  it('pins no search result into every session', () => {
    // A fixed query injected at every start, in every repo, arrives unasked
    // and unrelated to the session. Facts like the homelab inventory reach a
    // session through recall when a prompt is about them.
    const calls = runHook({ cwd: '/tmp/r' });
    assert.deepEqual(calls.filter(c => c.startsWith('search')), [], calls.join(' | '));
  });
});
