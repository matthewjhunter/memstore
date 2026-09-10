// Behavioural tests for memstore-prompt.mjs.
// Run: node --test cmd/memstore/hooks/memstore-prompt.test.mjs
//
// The hook is a script with top-level side effects, so it runs as a subprocess
// against a local HTTP server standing in for memstored and a stub memstore
// binary answering `mcp-headers`. What is pinned: the daemon requests carry the
// bearer token, and a refusal is reported rather than passed off as "nothing
// relevant" -- the failure that let recall injection run dark.

import { describe, it, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtempSync, writeFileSync, rmSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HOOK = join(dirname(fileURLToPath(import.meta.url)), 'memstore-prompt.mjs');
const PROMPT = 'what embedding model does herald use and where does it rerank';

let dir, stubBin, noTokenBin, server, url, requests, refuse, hints, missingRender;

before(async () => {
  dir = mkdtempSync(join(tmpdir(), 'memstore-prompt-hook-'));
  stubBin = join(dir, 'memstore-stub');
  writeFileSync(stubBin, `#!/bin/sh\n[ "$1" = mcp-headers ] && printf '%s\\n' '{"Authorization":"Bearer test-token"}'\n`);
  chmodSync(stubBin, 0o755);
  noTokenBin = join(dir, 'memstore-notoken');
  writeFileSync(noTokenBin, `#!/bin/sh\n[ "$1" = mcp-headers ] && printf '%s\\n' '{}'\n`);
  chmodSync(noTokenBin, 0o755);

  server = createServer((req, res) => {
    let body = '';
    req.on('data', c => { body += c; });
    req.on('end', () => {
      requests.push({ method: req.method, path: req.url, auth: req.headers.authorization, body });
      if (refuse) {
        res.writeHead(401, { 'Content-Type': 'application/json' });
        res.end('{"error":"unauthorized"}');
        return;
      }
      if (missingRender && req.url.startsWith('/v1/context/hints/render?')) {
        res.writeHead(404, { 'Content-Type': 'text/plain' });
        res.end('404 page not found');
        return;
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      if (req.url.startsWith('/v1/recall')) {
        res.end(JSON.stringify({ context: 'herald reranks through olla', facts: [] }));
      } else if (req.url.startsWith('/v1/context/hints/render?') || req.url.startsWith('/v1/context/hints?')) {
        res.end(JSON.stringify(hints));
      } else {
        res.end('{}');
      }
    });
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  url = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
  server.close();
  rmSync(dir, { recursive: true, force: true });
});

beforeEach(() => { requests = []; refuse = false; hints = { context: '', ids: [] }; missingRender = false; });

// The hook runs as an async child: spawnSync would block this process's event
// loop, and the stand-in daemon lives on it.
function runHook(bin = stubBin, prompt = PROMPT) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [HOOK], {
      env: { ...process.env, MEMSTORE_BIN: bin, MEMSTORED_URL: url },
    });
    let stdout = '', stderr = '';
    child.stdout.on('data', c => { stdout += c; });
    child.stderr.on('data', c => { stderr += c; });
    const timer = setTimeout(() => child.kill(), 10000);
    child.on('close', (status) => { clearTimeout(timer); resolve({ status, stdout, stderr }); });
    child.stdin.end(JSON.stringify({ session_id: 's-1', cwd: '/tmp/repo', prompt }));
  });
}

describe('memstore-prompt', () => {
  it('sends the bearer token on every daemon request', async () => {
    const result = await runHook();
    assert.equal(result.status, 0, result.stderr);
    const paths = requests.map(r => r.path.split('?')[0]).sort();
    assert.deepEqual(paths, ['/v1/context/hints/render', '/v1/recall']);
    for (const r of requests) {
      assert.equal(r.auth, 'Bearer test-token', `${r.path} went out without the token`);
    }
  });

  it('injects the recall context it was given', async () => {
    const result = await runHook();
    const out = JSON.parse(result.stdout);
    const ctx = out.hookSpecificOutput?.additionalContext ?? '';
    assert.ok(ctx.includes('<memstore-recall>') && ctx.includes('herald reranks through olla'), `context missing: ${result.stdout}`);
  });

  it('reports a refusal instead of passing it off as no context', async () => {
    refuse = true;
    const result = await runHook();
    assert.equal(result.status, 0, 'a refusal must not fail the hook');
    assert.match(result.stderr, /refused the request \(401\)/);
    assert.match(result.stderr, /api_key/);
    const out = JSON.parse(result.stdout);
    assert.equal(out.hookSpecificOutput, undefined, 'no context should be injected on a refusal');
  });

  it('still asks, without a header, when the CLI has no token', async () => {
    // A daemon running without auth must keep working; the daemon decides.
    const result = await runHook(noTokenBin);
    assert.equal(result.status, 0, result.stderr);
    assert.ok(requests.length > 0, 'no requests were made');
    for (const r of requests) assert.equal(r.auth, undefined);
  });

  it('injects the daemon-fenced hint block and consumes its ids', async () => {
    hints = { context: 'FENCED-HINT-BLOCK <untrusted-ab12>the deploy is on olla now</untrusted-ab12>', ids: [7, 9] };
    const result = await runHook();
    const ctx = JSON.parse(result.stdout).hookSpecificOutput?.additionalContext ?? '';
    assert.ok(ctx.includes(`<memstore-hints>\n${hints.context}\n</memstore-hints>`), `hint block not injected verbatim: ${ctx}`);
    const hintReq = requests.find(r => r.path.startsWith('/v1/context/hints/render?'));
    assert.ok(hintReq, 'hints were not fetched from the render endpoint');
    assert.equal(new URL(hintReq.path, url).searchParams.get('limit'), '2');
    // Consumption is fire-and-forget, but the child stays alive until its
    // requests settle, so they are all recorded by the time it exits.
    const consumed = requests.filter(r => /\/v1\/context\/hints\/\d+\/consume$/.test(r.path)).map(r => r.path).sort();
    assert.deepEqual(consumed, ['/v1/context/hints/7/consume', '/v1/context/hints/9/consume']);
  });

  it('injects no hints from a daemon without the render endpoint', async () => {
    // Falling back to the raw list would put model-written text in the prompt
    // unfenced, which is the defect the render endpoint exists to close.
    missingRender = true;
    hints = [{ id: 1, hint_text: 'RAW-UNFENCED-HINT' }];
    const result = await runHook();
    assert.equal(result.status, 0, result.stderr);
    const ctx = JSON.parse(result.stdout).hookSpecificOutput?.additionalContext ?? '';
    assert.ok(!ctx.includes('RAW-UNFENCED-HINT'), `unfenced hint injected: ${ctx}`);
    assert.ok(!ctx.includes('<memstore-hints>'), `empty hint block injected: ${ctx}`);
    assert.match(result.stderr, /hints skipped/);
  });

  it('injects nothing when the daemon returns no hint context', async () => {
    hints = { context: '', ids: [] };
    const result = await runHook();
    const ctx = JSON.parse(result.stdout).hookSpecificOutput?.additionalContext ?? '';
    assert.ok(!ctx.includes('<memstore-hints>'), ctx);
    assert.ok(!requests.some(r => r.path.endsWith('/consume')), 'consumed hints it never showed');
  });

  it('skips recall for a short prompt', async () => {
    await runHook(stubBin, 'merge it');
    assert.ok(!requests.some(r => r.path.startsWith('/v1/recall')), 'recall ran on a two-word prompt');
  });
});
