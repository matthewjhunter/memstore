// Run: node --test cmd/memstore/hooks/memstore-file-context.test.mjs
//
// The Read and Edit hooks run as subprocesses against a stub memstore binary
// that records its argv. Pinned: they hand eval-triggers the session id, so
// what it shows is recorded against the session and not shown again.

import { describe, it, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));

let dir, stubBin, argvLog;

before(() => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-file-hook-'));
  stubBin = join(dir, 'memstore-stub');
  argvLog = join(dir, 'argv.log');
  // Records argv; answers eval-triggers with a marker so the wrapping can be checked.
  writeFileSync(stubBin, `#!/bin/sh\nprintf '%s\\n' "$*" >> ${argvLog}\n[ "$1" = eval-triggers ] && printf 'TRIGGER-BLOCK\\n'\nexit 0\n`);
  chmodSync(stubBin, 0o755);
});

after(() => rmSync(dir, { recursive: true, force: true }));
beforeEach(() => rmSync(argvLog, { force: true }));

function runHook(hook, input) {
  const result = spawnSync(process.execPath, [join(HERE, hook)], {
    input: JSON.stringify(input),
    encoding: 'utf-8',
    // Port 9 refuses the file-touch POST at once, so the hook does not wait on it.
    env: { ...process.env, MEMSTORE_BIN: stubBin, MEMSTORED_URL: 'http://127.0.0.1:9' },
    timeout: 10000,
  });
  // Only the eval-triggers calls: the auth helper also runs the binary, for
  // mcp-headers, when the hook touches the file.
  const argv = existsSync(argvLog)
    ? readFileSync(argvLog, 'utf-8').trim().split('\n').filter(c => c.startsWith('eval-triggers'))
    : [];
  return { argv, out: JSON.parse(result.stdout) };
}

for (const hook of ['memstore-read.mjs', 'memstore-edit.mjs']) {
  describe(hook, () => {
    it('passes the session id to eval-triggers', () => {
      const { argv, out } = runHook(hook, { session_id: 's-1', tool_input: { file_path: '/w/repo/main.go' } });
      assert.deepEqual(argv, ['eval-triggers --file /w/repo/main.go --session s-1']);
      assert.equal(out.hookSpecificOutput.additionalContext, '<memstore-file-context>\nTRIGGER-BLOCK\n</memstore-file-context>\n');
    });

    it('runs without a session id', () => {
      const { argv } = runHook(hook, { tool_input: { file_path: '/w/repo/main.go' } });
      assert.deepEqual(argv, ['eval-triggers --file /w/repo/main.go']);
    });

    it('skips a relative path', () => {
      const { argv, out } = runHook(hook, { session_id: 's-1', tool_input: { file_path: 'main.go' } });
      assert.deepEqual(argv, []);
      assert.equal(out.hookSpecificOutput, undefined);
    });
  });
}
