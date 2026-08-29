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

let dir, stubBin, argvLog;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-startup-hook-'));
  stubBin = join(dir, 'memstore-stub');
  argvLog = join(dir, 'argv.log');
  writeFileSync(stubBin, `#!/bin/sh\nprintf '%s\\n' "$*" >> ${argvLog}\n`);
  chmodSync(stubBin, 0o755);
});

after(() => rmSync(dir, { recursive: true, force: true }));
beforeEach(() => rmSync(argvLog, { force: true }));

function runHook(input, env = {}) {
  spawnSync(process.execPath, [HOOK], {
    input: JSON.stringify(input),
    encoding: 'utf-8',
    env: { ...process.env, MEMSTORE_BIN: stubBin, ...env },
    timeout: 10000,
  });
  return existsSync(argvLog) ? readFileSync(argvLog, 'utf-8').trim().split('\n') : [];
}

describe('memstore-startup', () => {
  it('asks for the top five tasks for the session directory', () => {
    const calls = runHook({ session_id: 's-1', cwd: "/home/m/git/it's here" });
    const tasks = calls.find(c => c.startsWith('tasks '));
    assert.ok(tasks, `no tasks call: ${calls.join(' | ')}`);
    assert.match(tasks, /--surface startup/);
    assert.match(tasks, /--limit 5/);
    assert.match(tasks, /--cwd \/home\/m\/git\/it's here/);
  });

  it('honours MEMSTORE_STARTUP_TASKS', () => {
    const calls = runHook({ cwd: '/tmp/r' }, { MEMSTORE_STARTUP_TASKS: '3' });
    assert.match(calls.find(c => c.startsWith('tasks ')) ?? '', /--limit 3/);
  });
});
