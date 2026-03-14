#!/usr/bin/env node
// PostToolUse hook: nudge to store facts when high-signal events occur.
//
// Fires after Bash or Write tool calls. Detects:
// - git init → new repo, update inventory
// - gh pr create → decisions may need storing
// - Write to CLAUDE.md → new project, store project fact
// - Write to go.mod → new Go module, store project fact
//
// Outputs additionalContext with a <store-nudge> tag when a trigger matches.
// Silently passes through for non-matching tool calls.

const stdinData = await stdinText();
let hook;
try {
  hook = JSON.parse(stdinData);
} catch {
  console.log(JSON.stringify({ continue: true }));
  process.exit(0);
}

const toolName = hook.tool_name || '';
const toolInput = hook.tool_input || {};

let nudge = '';

if (toolName === 'Bash') {
  const cmd = toolInput.command || '';
  if (/\bgit\s+init\b/.test(cmd)) {
    nudge = 'A new git repository was just initialized. Update the repo inventory in memstore (subject: "matthew", category: "project") and store a project fact for the new repo.';
  } else if (/\bgh\s+pr\s+create\b/.test(cmd)) {
    nudge = 'A pull request was just created. If this PR represents architectural decisions or significant changes not yet in memstore, store the key decisions now.';
  }
} else if (toolName === 'Write') {
  const path = toolInput.file_path || '';
  if (/\/CLAUDE\.md$/i.test(path)) {
    nudge = 'A CLAUDE.md was just created. Store a project fact in memstore describing this project, its purpose, and any key decisions made so far.';
  } else if (/\/go\.mod$/.test(path) && !toolInput._is_edit) {
    nudge = 'A new Go module was just initialized. If this is a new project, update the repo inventory in memstore and store a project fact.';
  }
}

if (nudge) {
  console.log(JSON.stringify({
    continue: true,
    hookSpecificOutput: {
      hookEventName: 'PostToolUse',
      additionalContext: `<store-nudge>${nudge}</store-nudge>`,
    },
  }));
} else {
  console.log(JSON.stringify({ continue: true }));
}

async function stdinText() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  return Buffer.concat(chunks).toString('utf-8');
}
