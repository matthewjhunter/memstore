// Behavioural tests for stop-hook.mjs.
// Run: node --test cmd/memstore/hooks/stop-hook.test.mjs
//
// Same approach as memstore-session-end.test.mjs: the hook is a script with
// top-level side effects, so it runs as a subprocess against a stub MEMSTORE_BIN
// that records its own argv. What is worth pinning here is which binary the hook
// reaches for -- the whole point of the port is that it is no longer memstore-mcp.

import { describe, it, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HOOK = join(dirname(fileURLToPath(import.meta.url)), 'stop-hook.mjs');

let dir, stubBin, argvLog;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-stop-hook-'));
  stubBin = join(dir, 'memstore-stub');
  argvLog = join(dir, 'argv.log');
  writeFileSync(stubBin, `#!/bin/sh\nprintf '%s\\n' "$*" >> ${argvLog}\n`);
  chmodSync(stubBin, 0o755);
});

after(() => rmSync(dir, { recursive: true, force: true }));

beforeEach(() => rmSync(argvLog, { force: true }));

function runHook(bin = stubBin) {
  const result = spawnSync(process.execPath, [HOOK], {
    input: JSON.stringify({ session_id: 's-1', cwd: '/tmp', transcript_path: '/tmp/t.jsonl' }),
    encoding: 'utf-8',
    env: { ...process.env, MEMSTORE_BIN: bin },
    timeout: 10000,
  });
  const calls = existsSync(argvLog)
    ? readFileSync(argvLog, 'utf-8').trim().split('\n').filter(Boolean)
    : [];
  return { ...result, calls };
}

describe('stop-hook', () => {
  it('invokes the memstore CLI hook subcommand', () => {
    const { calls } = runHook();
    assert.deepEqual(calls, ['hook'], `unexpected invocations: ${calls.join(' | ')}`);
  });

  it('does not reach for memstore-mcp', () => {
    // The capture is an HTTP client posting to the daemon. Requiring the MCP
    // binary for it was what kept a local binary mandatory on every machine.
    const source = readFileSync(HOOK, 'utf-8');
    const spawned = source.match(/spawnSync\([^)]*/s)?.[0] ?? '';
    assert.ok(!spawned.includes('memstore-mcp'), 'the hook still spawns memstore-mcp');
  });

  it('exits cleanly when the binary is missing', () => {
    // A machine that has not run `memstore setup` yet must not have every Stop
    // event reported to the user as a hook failure.
    const { status } = runHook(join(dir, 'does-not-exist'));
    assert.equal(status, 0);
  });
});
