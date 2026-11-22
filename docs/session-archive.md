# The session archive: raw transcripts, and a hierarchy derived from them

Status: **decided** 2026-11-21 (D1-D4 below); phase 1 in progress
Author: Matthew + Claude
Date: 2026-11-21

## Why

The daemon keeps the text of each user and assistant message (`session_turns`) and drops everything else at upload: tool calls, tool results, thinking, attachments. The full transcript exists only as a JSONL file on the client, and the client ages those out after about a month. Two things follow.

- There is no audit record. What ran, on which host, with what result, is in the tool calls, and the tool calls are gone.
- A better reader cannot be pointed at old sessions. Extraction today reads the last 120 KB of a session's text; measured since September, 38 of 140 sessions were longer than that, so the start of every long session was never read by anything.

The fix is to keep the raw file and derive structure from it, so that a session can be read again later by whatever reader exists then.

## What a transcript contains

Measured on 54 transcripts (387 MB) on 2026-11-21, Claude Code 2.1.287 and later. The format is not documented or stable; this is what the files hold.

- One JSON object per line, each with a `type`. `user` and `assistant` carry messages; the rest is bookkeeping (`attachment`, `api-request`, `api-request-blob`, `ai-title`, `system`, `file-history-snapshot`, and a dozen more).
- Message content is a string or an array of typed blocks: `text`, `thinking`, `tool_use`, `tool_result`.
- By size: tool results 80 MB, thinking 13 MB, tool inputs 8.5 MB, assistant text 2 MB.
- A compaction is two lines: a `system` entry with `subtype: "compact_boundary"` and `compactMetadata` (`trigger`, `preTokens`), and a `user` entry with `isCompactSummary: true` whose `parentUuid` is the boundary's `uuid`. 52 compactions in 25 sessions, at most 5 in one.
- The summary text begins "This session is being continued from a previous conversation". The text-only archive already holds 317 such turns across 171 sessions.
- `ai-title` entries carry a title for the session; the last one is current.
- Subagent transcripts are separate files under `<session>/subagents/` (31 files, 163 MB) and are not uploaded today.
- gzip -6 compresses the set 2.3x.

## Raw storage

Files on disk, indexed from Postgres. Postgres can hold a 51 MB value (the largest transcript measured), but reads and writes it whole in memory, and the nightly logical dump would carry every transcript every night.

- The daemon takes a directory (`MEMSTORE_TRANSCRIPT_DIR`). Unset, raw storage is off and uploads behave as they do now.
- One file per session: `<dir>/<user id>/<session id>.jsonl.gz`, mode 0600 in a 0700 directory. The path is built from the authenticated user id and a session id that must parse as a UUID. Nothing else from the request reaches the path.
- The client sends the gzip bytes as the request body to `PUT /v1/sessions/{id}/raw`, with `cwd` and `persona` as query parameters. The daemon decompresses once, bounded by a maximum decompressed size, to parse and to verify the stream is well formed; it writes the compressed bytes it was sent to a temporary file in the same directory and renames it into place.
- A resumed session uploads again and replaces its file. The index row keeps the latest size and hash.
- `session_archives` is the index: session id, user id, relative path, compressed and raw sizes, SHA-256 of the raw bytes, line count, parser version, first and last upload time.
- Retention is a setting (`MEMSTORE_TRANSCRIPT_RETENTION`, a duration; unset or 0 keeps everything, which is the default). When set, a sweep removes the file and marks the index row; derived rows stay.
- The existing `POST /v1/sessions/transcript` stays for clients that predate this. The new endpoint does everything the old one does (turns, citations, extraction queue) from the same bytes.

A raw transcript is more sensitive than the text archive: tool output includes file contents and whatever credentials passed through a session. Phase 1 relies on file modes, the host and the backup's own encryption. Encryption at rest is phase 2 (below).

## The hierarchy

Everything here is derived from the raw file and can be rebuilt from it. Each derived row records the parser version that produced it.

| Layer | Holds | Links |
|---|---|---|
| session | title, first and last timestamp, cwd, bullet summary | parent of the rest |
| compaction | the compacted summary text, trigger, token count before, timestamp, detector format | child of the session, ordered |
| session-transcript | one segment: a line range and byte range in the raw file | the compaction it started from, and the compaction that followed, each if there is one |
| session-tools | one row per tool call: name, call id, input, result size and location, error flag | its segment |

A session with N compactions has N+1 segments. Compaction k is Claude's own description of segment k, so only the last segment of a session (the whole of an uncompacted one) has no summary written by the model that did the work. That one summary is generated; the others are free.

Segment k runs from the line after compaction k-1's summary to the line before compaction k's boundary. The two compaction lines belong to the compaction, not to either segment.

### Facts and tables (D1)

- **session** and **compaction** are facts (`kind: "session"`, `kind: "compaction"`), linked parent to child, so they are searchable and linkable like anything else in the store. A compaction summary averages 16 KB and would crowd prompt recall, so both kinds carry a metadata flag that keeps them out of recall by default while leaving them in explicit search. The flag is data on the fact, not a list of kinds in the recall code.
- **session-transcript** and **session-tools** are tables (`session_segments`, `session_tool_calls`) that point into the raw file. A segment is a pointer, not content; a tool call's input is stored (about 1 KB on average) and its result is not.

This is a starting point. If segments or tool calls turn out to want search, they can be promoted later; the raw file makes that a re-derive, not a migration of content.

### What is not indexed yet

Left in the raw file with no table: thinking blocks (mostly empty or signature data), `attachment` entries (they include what hooks injected, which makes them ground truth for exposure and the likely next thing to index), `api-request-blob` entries (duplicates), and file-history snapshots.

## Compaction detection

A fixed list of detectors, compiled in, behind one interface. A detector reads the parsed entries and returns the boundaries it recognises: the line, the summary text, what metadata it has, and its own format name, which is stored on the compaction. Adding a format is adding a detector to the list; there is no configuration.

1. `claude-code-marker`: a `compact_boundary` entry and the `isCompactSummary` entry that names it as parent. A summary with no boundary entry still counts.
2. `continuation-prefix`: a user message that begins with the continuation sentence, where no marker already claimed the line. This reads older files, and it reads the text-only archive, which has no markers at all.

A line claimed by an earlier detector is not offered to a later one.

## Extraction over the hierarchy

Not part of phase 1, but it is what the structure is for. A reader runs per segment with the preceding compaction as context, so its input is bounded and no part of a long session is skipped. A compacted segment already has its summary. The Sonnet comparison (shadow output to a side table, against what the local model extracted for the same sessions) runs on this.

## Phases

**Phase 1**

1. `internal/transcript`: parse a raw transcript into session metadata, compactions, segments and tool calls. Pure, no store.
2. `session_archives`, `session_segments`, `session_tool_calls`; session and compaction facts with links and the recall flag.
3. `PUT /v1/sessions/{id}/raw`, and the client side in the Stop hook's upload.
4. Retention setting and sweep.
5. Backfill: derive sessions and compactions for the text-only archive through `continuation-prefix`. Segments there are turn ranges, since there is no raw file to point into.
6. Deployment: a volume for the daemon, and the directory in the backup.

**Phase 2**

- Encryption at rest (D2).
- Subagent transcripts as children of their session (D4).
- Attachments, if exposure measurement wants them.

## Decisions

**D1. Facts or tables.** Session and compaction as facts, segments and tool calls as tables. **Decided: yes, as a starting point.**

**D2. Encryption at rest.** **Decided: yes, optional, phase 2, tied to the user's API key if that can be done without creating a key-management problem.** The open design problem is that the daemon verifies tokens and never stores them, so a key derived from a token exists only while that user's request is in flight. That is enough to encrypt at upload and to decrypt on that user's own read, and it means the daemon holds nothing that opens the file. It is not enough for work with no request behind it: a background re-derive after a parser change, or a retention sweep that needs to read. Rotating a token also orphans what the old one wrapped unless the rotation rewraps. Phase 1 is shaped so this stays possible: everything derived is derived at upload, while the plaintext is in hand, and nothing in phase 1 requires the daemon to read a stored file on its own.

**D3. Retention.** **Decided: a setting, default forever.**

**D4. Subagent transcripts.** **Decided: phase 2.**
