#!/usr/bin/env node
// Stop hook shim — forwards the Stop event to memstore-mcp.
//
// All upload state machine, per-session state tracking, nudge emission, and
// pending-transcript draining live in the Go binary (see runHookCapture in
// cmd/memstore-mcp/main.go). The Node shim is only here because Claude Code's
// hook configuration points at a script path; nothing about this work needs
// JavaScript anymore.

import { spawnSync } from 'child_process';

const result = spawnSync('memstore-mcp', ['--hook'], {
  stdio: ['inherit', 'inherit', 'inherit'],
});

process.exit(result.status ?? 0);
