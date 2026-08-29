// Run: node --test cmd/memstore/hooks/memstore-context-touch.test.mjs
//
// touchFile is a module, so it is imported directly with MEMSTORED_URL and
// MEMSTORE_BIN pointed at a local server and a stub CLI. Pinned: the touch
// request carries the bearer token.

import { describe, it, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtempSync, writeFileSync, rmSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

let dir, server, requests = [];

before(async () => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-touch-hook-'));
  const stubBin = join(dir, 'memstore-stub');
  writeFileSync(stubBin, `#!/bin/sh\n[ "$1" = mcp-headers ] && printf '%s\\n' '{"Authorization":"Bearer test-token"}'\n`);
  chmodSync(stubBin, 0o755);
  process.env.MEMSTORE_BIN = stubBin;

  server = createServer((req, res) => {
    let body = '';
    req.on('data', c => { body += c; });
    req.on('end', () => {
      requests.push({ path: req.url, auth: req.headers.authorization, body: JSON.parse(body) });
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{}');
    });
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  process.env.MEMSTORED_URL = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
  server.close();
  rmSync(dir, { recursive: true, force: true });
});

describe('memstore-context-touch', () => {
  it('sends the bearer token with the touch', async () => {
    const { touchFile } = await import('./memstore-context-touch.mjs');
    await touchFile('s-1', '/tmp/repo/main.go');
    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, '/v1/context/touch');
    assert.equal(requests[0].auth, 'Bearer test-token');
    assert.deepEqual(requests[0].body, { session_id: 's-1', files: ['/tmp/repo/main.go'] });
  });
});
