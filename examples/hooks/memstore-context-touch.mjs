#!/usr/bin/env node
/**
 * memstore-context-touch: Helper for Read/Edit hooks
 *
 * Sends a POST /v1/context/touch to memstored so it tracks which files
 * are being accessed in the current session. This data is used by /v1/recall
 * to boost facts related to recently touched files.
 *
 * Usage: import { touchFile } from './memstore-context-touch.mjs';
 *        await touchFile(sessionId, filePath);
 *
 * Silently swallows errors — this is advisory, not critical.
 */

const MEMSTORED_URL = process.env.MEMSTORED_URL || 'http://localhost:8230';
const MEMSTORED_API_KEY = process.env.MEMSTORED_API_KEY || '';

export async function touchFile(sessionId, filePath) {
  if (!sessionId || !filePath) return;
  try {
    const headers = { 'Content-Type': 'application/json' };
    if (MEMSTORED_API_KEY) {
      headers['Authorization'] = `Bearer ${MEMSTORED_API_KEY}`;
    }
    await fetch(`${MEMSTORED_URL}/v1/context/touch`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ session_id: sessionId, files: [filePath] }),
      signal: AbortSignal.timeout(1000),
    });
  } catch {
    // Best-effort — don't block the hook.
  }
}
