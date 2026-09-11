#!/usr/bin/env node
/**
 * memstore-notices: whether hooks show the user what they injected.
 *
 * A hook's systemMessage is shown to the user and not given to the model, so a
 * short summary there costs no context. The setting is hook_notices in
 * config.toml (MEMSTORE_HOOK_NOTICES overrides it), read through the CLI so
 * there is one parser for the config file. The channels the CLI renders -- file
 * triggers, the startup list -- apply it themselves; this is for the prompt
 * hook, which talks to the daemon directly.
 *
 * Usage: import { noticesEnabled } from './memstore-notices.mjs';
 */

import { spawnSync } from 'node:child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

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
