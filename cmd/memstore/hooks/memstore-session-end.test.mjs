// Behavioural tests for memstore-session-end.mjs.
// Run: node --test cmd/memstore/hooks/memstore-session-end.test.mjs
//
// The hook is a script with top-level side effects rather than exported
// functions, so it is exercised as a subprocess against a stub MEMSTORE_BIN
// that records its own argv. That is the behaviour worth pinning: which
// memstore subcommands the hook invokes at session end.

import { describe, it, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HOOK = join(dirname(fileURLToPath(import.meta.url)), 'memstore-session-end.mjs');

let dir, stubBin, argvLog;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-session-end-'));
  stubBin = join(dir, 'memstore-stub');
  argvLog = join(dir, 'argv.log');
  // Records each invocation's arguments, one line per call.
  writeFileSync(stubBin, `#!/bin/sh\nprintf '%s\\n' "$*" >> ${argvLog}\n`);
  chmodSync(stubBin, 0o755);
});

after(() => rmSync(dir, { recursive: true, force: true }));

beforeEach(() => rmSync(argvLog, { force: true }));

function runHook(input = { sessionId: 's-1', directory: '/home/matthew/git/matthewjhunter/memstore' }) {
  const result = spawnSync(process.execPath, [HOOK], {
    input: JSON.stringify(input),
    encoding: 'utf-8',
    env: { ...process.env, MEMSTORE_BIN: stubBin },
    timeout: 10000,
  });
  const calls = existsSync(argvLog)
    ? readFileSync(argvLog, 'utf-8').trim().split('\n').filter(Boolean)
    : [];
  return { ...result, calls };
}

describe('memstore-session-end', () => {
  it('does not write a session-activity fact', () => {
    // Session activity is already recorded in session_hooks by the Stop hook,
    // with a full cwd and a real timestamp column. A duplicate fact row adds
    // nothing retrievable and competes with real content in search. See #151.
    const { calls } = runHook();
    const stores = calls.filter((c) => c.startsWith('store'));
    assert.deepEqual(stores, [], `hook shelled out to memstore store: ${stores.join(' | ')}`);
    assert.ok(
      !calls.some((c) => c.includes('session-activity')),
      'hook referenced the session-activity subject',
    );
  });

  it('still reports open startup tasks', () => {
    const { calls } = runHook();
    assert.ok(
      calls.some((c) => c.startsWith('tasks') && c.includes('--surface startup')),
      `expected a tasks --surface startup call, got: ${calls.join(' | ')}`,
    );
  });

  it('emits the continue directive on stdout', () => {
    const { stdout, status } = runHook();
    assert.equal(status, 0);
    assert.deepEqual(JSON.parse(stdout.trim()), { continue: true });
  });

  it('exits cleanly when stdin is not valid JSON', () => {
    const result = spawnSync(process.execPath, [HOOK], {
      input: 'not json',
      encoding: 'utf-8',
      env: { ...process.env, MEMSTORE_BIN: stubBin },
      timeout: 10000,
    });
    assert.equal(result.status, 0);
    assert.deepEqual(JSON.parse(result.stdout.trim()), { continue: true });
  });
});
