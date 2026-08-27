#!/usr/bin/env node
// Stop hook shim -- forwards the Stop event to the memstore CLI.
//
// All upload state machine, per-session state tracking, nudge emission, and
// pending-transcript draining live in Go (see internal/hookcapture). The Node
// shim is only here because Claude Code's hook configuration points at a script
// path; nothing about this work needs JavaScript.
//
// It used to spawn `memstore-mcp --hook`, which made the Stop hook the last
// thing on the machine that required the MCP binary to be installed locally --
// and so the last thing standing between memstore and an HTTP-only client. The
// capture is an HTTP client posting to the daemon, which is what the CLI is.

import { spawnSync } from 'child_process';

const MEMSTORE_BIN = process.env.MEMSTORE_BIN || '__MEMSTORE_BIN__';

const result = spawnSync(MEMSTORE_BIN, ['hook'], {
  stdio: ['inherit', 'inherit', 'inherit'],
});

// A missing binary must not fail the hook: Claude Code surfaces a non-zero exit
// to the user, and an unconfigured machine losing its session archive is not
// worth interrupting a session over.
process.exit(result.status ?? 0);
