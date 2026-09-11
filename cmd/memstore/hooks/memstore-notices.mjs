#!/usr/bin/env node
/**
 * memstore-notices: hooks showing the user what they injected.
 *
 * A hook's systemMessage is shown to the user and not given to the model, so a
 * short summary there costs no context. The setting is hook_notices in
 * config.toml (MEMSTORE_HOOK_NOTICES overrides it), read through the CLI so
 * there is one parser for the config file. The channels the CLI renders -- file
 * triggers, the startup list -- apply it themselves, and their hooks read the
 * result with runHookFormat; noticesEnabled is for the prompt hook, which talks
 * to the daemon directly.
 *
 * Usage: import { noticesEnabled, runHookFormat } from './memstore-notices.mjs';
 */

import { spawnSync } from 'node:child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

const NOTHING = { context: '', notice: '' };

const SKEW_NOTICE = `memstore: ${MEMSTORE_BIN} is older than these hooks (it has no --format hook), ` +
  'so this context was loaded the old way. Update memstore, then run memstore setup again.';

// noticesEnabled reports whether the user has notices on. Anything but a clear
// "on" -- no CLI, a CLI too old to know the command -- is off.
export function noticesEnabled() {
  try {
    const result = spawnSync(MEMSTORE_BIN, ['hook-notices'], {
      encoding: 'utf-8',
      timeout: 1500,
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    return result.status === 0 && result.stdout.trim() === 'on';
  } catch {
    return false;
  }
}

// runHookFormat runs a CLI command with --format hook and returns its
// {context, notice}. The arguments go to the binary as an argv, not through a
// shell, so nothing in them needs quoting.
//
// A binary older than these hooks does not know that format: eval-triggers
// rejects the flag, and tasks takes the unknown format for text. Read as
// "nothing to inject", either would drop the hook's context without a word,
// so when the answer is not the hook format the command is run again as
// fallbackArgs, the format from before notices, and the notice says the binary
// needs updating. A missing binary, a timeout or any other failure is nothing
// to inject, as before.
export function runHookFormat(args, fallbackArgs, timeout) {
  const first = runCLI([...args, '--format', 'hook'], timeout);
  if (!first) return NOTHING;
  if (first.status === 0) {
    const out = first.stdout.trim();
    if (!out) return NOTHING;
    const parsed = parseHookOutput(out);
    if (parsed) return parsed;
  } else if (!first.stderr.includes('flag provided but not defined: -format')) {
    return NOTHING;
  }
  const fallback = runCLI(fallbackArgs, timeout);
  const context = fallback?.status === 0 ? fallback.stdout.trim() : '';
  return { context, notice: context ? SKEW_NOTICE : '' };
}

// parseHookOutput reads `--format hook` output, or returns null when out is not
// in that format.
function parseHookOutput(out) {
  let parsed;
  try {
    parsed = JSON.parse(out);
  } catch {
    return null;
  }
  if (typeof parsed?.context !== 'string') return null;
  return {
    context: parsed.context.trim(),
    notice: typeof parsed.notice === 'string' ? parsed.notice : '',
  };
}

// runCLI runs the binary and returns the result, or null when it could not be
// run or did not finish in time.
function runCLI(args, timeout) {
  try {
    const result = spawnSync(MEMSTORE_BIN, args, {
      encoding: 'utf-8',
      timeout,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    return result.error ? null : result;
  } catch {
    return null;
  }
}
