#!/usr/bin/env node
// PostCompact hook shim — forwards the PostCompact event to memstore-mcp.
//
// Claude Code fires PostCompact after /compact (manual) or auto-compaction
// finishes, with the model-generated summary in the `compact_summary` field.
// memstore-mcp --post-compact stores it as a fact attributed to the cwd's
// enclosing repo, so the summary the active session's model just produced
// becomes a memstore-searchable artifact without re-summarization.

import { spawnSync } from 'child_process';

const result = spawnSync('memstore-mcp', ['--post-compact'], {
  stdio: ['inherit', 'inherit', 'inherit'],
});

process.exit(result.status ?? 0);
