#!/usr/bin/env node
/**
 * memstore-auth: bearer token for hooks that call memstored directly.
 *
 * The daemon refuses unauthenticated requests, and a hook that swallows the
 * 401 looks exactly like a hook with nothing to say -- which is how recall
 * injection ran dark for days. The token lives in ~/.config/memstore/config.toml
 * (mode 0600); rather than copy it into these world-readable scripts, ask the
 * CLI for the header it already knows how to build (`memstore mcp-headers`,
 * the same helper Claude Code's MCP registration uses).
 *
 * Usage: import { authHeaders, warnOnce } from './memstore-auth.mjs';
 *        fetch(url, { headers: { ...authHeaders(), 'Content-Type': ... } })
 */

import { spawnSync } from 'node:child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

let cached;

// authHeaders returns the daemon's auth header(s) as an object, or {} when the
// CLI is unavailable or has no token configured. Resolved once per process.
export function authHeaders() {
  if (cached !== undefined) return cached;
  cached = {};
  try {
    const result = spawnSync(MEMSTORE_BIN, ['mcp-headers'], {
      encoding: 'utf-8',
      timeout: 1500,
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    if (result.status === 0 && result.stdout) {
      const parsed = JSON.parse(result.stdout);
      if (parsed && typeof parsed === 'object') cached = parsed;
    }
  } catch {
    // No CLI, no token, or unparseable output: send nothing and let the
    // daemon's 401 be reported by the caller.
  }
  return cached;
}

const warned = new Set();

// warnOnce writes a diagnostic to stderr once per process per message. Hooks
// must never block a session, but silence is how this class of failure hides.
export function warnOnce(message) {
  if (warned.has(message)) return;
  warned.add(message);
  process.stderr.write(`${message}\n`);
}

// reportRefusal names a 401/403 for what it is: a credential problem, not an
// empty result.
export function reportRefusal(hook, resp) {
  if (resp.status === 401 || resp.status === 403) {
    warnOnce(`${hook}: memstored refused the request (${resp.status}); is api_key set in ~/.config/memstore/config.toml?`);
  }
}
