// Unit tests for codex-notify-memstore.mjs pure-logic exports.
// Run: node --test examples/codex/codex-notify-memstore.test.mjs
//
// We do not exercise the spawn / filesystem path here — those are integration
// concerns that require Codex + memstored to be live. The unit tests cover
// the JSON-shape decisions where regressions would silently corrupt
// transcripts or upload the wrong session id.

import { describe, it } from 'node:test';
import assert from 'node:assert/strict';

import {
  classifyEvent,
  buildTranscriptLines,
  buildHookPayload,
} from './codex-notify-memstore.mjs';

describe('classifyEvent', () => {
  it('forwards a well-formed agent-turn-complete', () => {
    const decision = classifyEvent({
      type: 'agent-turn-complete',
      'thread-id': '01HXYZ-deadbeef',
      'turn-id': 't-1',
      cwd: '/home/matthew/git/memstore',
      'input-messages': ['Refactor extract.go', 'and add tests'],
      'last-assistant-message': 'Done — see commit abc123.',
    });
    assert.deepEqual(decision, {
      action: 'forward',
      threadId: '01HXYZ-deadbeef',
      cwd: '/home/matthew/git/memstore',
      userText: 'Refactor extract.go\nand add tests',
      assistantText: 'Done — see commit abc123.',
    });
  });

  it('skips non-turn-complete event types', () => {
    const decision = classifyEvent({ type: 'session-started', 'thread-id': 'x' });
    assert.equal(decision.action, 'skip');
    assert.match(decision.reason, /^unhandled type=session-started/);
  });

  it('skips events with missing thread-id', () => {
    const decision = classifyEvent({ type: 'agent-turn-complete' });
    assert.equal(decision.action, 'skip');
    assert.equal(decision.reason, 'missing thread-id');
  });

  it('skips events whose thread-id is unsafe for a path', () => {
    const decision = classifyEvent({
      type: 'agent-turn-complete',
      'thread-id': '../etc/passwd',
    });
    assert.equal(decision.action, 'skip');
    assert.match(decision.reason, /^unsafe thread-id/);
  });

  it('skips a null event', () => {
    assert.equal(classifyEvent(null).action, 'skip');
    assert.equal(classifyEvent(undefined).action, 'skip');
    assert.equal(classifyEvent('string').action, 'skip');
  });

  it('coerces a missing input-messages to an empty string', () => {
    const decision = classifyEvent({
      type: 'agent-turn-complete',
      'thread-id': 't1',
      'last-assistant-message': 'hi',
    });
    assert.equal(decision.action, 'forward');
    assert.equal(decision.userText, '');
    assert.equal(decision.assistantText, 'hi');
  });

  it('coerces a non-array input-messages to a string', () => {
    const decision = classifyEvent({
      type: 'agent-turn-complete',
      'thread-id': 't1',
      'input-messages': 'single',
      'last-assistant-message': '',
    });
    assert.equal(decision.action, 'forward');
    assert.equal(decision.userText, 'single');
  });

  it('falls back to cwd of process when event.cwd is missing', () => {
    const decision = classifyEvent({
      type: 'agent-turn-complete',
      'thread-id': 't1',
      'last-assistant-message': '',
    });
    assert.equal(decision.action, 'forward');
    assert.equal(decision.cwd, process.cwd());
  });
});

describe('buildTranscriptLines', () => {
  it('emits one JSONL line per role, newline-terminated', () => {
    const out = buildTranscriptLines('hello', 'world');
    const lines = out.split('\n');
    // Two JSON entries plus the trailing empty element from the final newline.
    assert.equal(lines.length, 3);
    assert.equal(lines[2], '');

    const user = JSON.parse(lines[0]);
    assert.equal(user.type, 'user');
    assert.equal(user.message.role, 'user');
    assert.equal(user.message.content[0].text, 'hello');

    const assistant = JSON.parse(lines[1]);
    assert.equal(assistant.type, 'assistant');
    assert.equal(assistant.message.role, 'assistant');
    assert.equal(assistant.message.content[0].text, 'world');
  });

  it('preserves embedded newlines in user input', () => {
    const out = buildTranscriptLines('line one\nline two', 'ack');
    const user = JSON.parse(out.split('\n')[0]);
    assert.equal(user.message.content[0].text, 'line one\nline two');
  });
});

describe('buildHookPayload', () => {
  it('emits the three fields memstore-mcp --hook reads', () => {
    const payload = JSON.parse(
      buildHookPayload('thread-1', '/tmp/work', '/tmp/transcript.jsonl'),
    );
    assert.deepEqual(payload, {
      session_id: 'thread-1',
      cwd: '/tmp/work',
      transcript_path: '/tmp/transcript.jsonl',
    });
  });
});
