package pgstore

import (
	"bytes"
	"strings"
)

// PostgreSQL cannot store a NUL byte in TEXT, and cannot represent \u0000 in
// JSONB either: both raise SQLSTATE 22021, "invalid byte sequence for encoding
// UTF8: 0x00". Every other UTF-8 byte sequence is fine, so this is the one
// character that has to be dealt with before it reaches the wire.
//
// It reaches us for real. Claude Code transcripts are JSONL, and a turn that
// captured terminal output or a binary paste carries \u0000 as a JSON escape;
// decoding it produces an actual NUL in the Go string, which the insert then
// rejects. One such transcript failed every upload it was ever tried on, and
// because the drain retries the same entry first, it blocked the ones behind it.
//
// Stripping is the only option -- there is no encoding of NUL that Postgres
// will accept -- and it is the right one here: the byte is junk captured from a
// terminal, not content, and replacing it with U+FFFD would put visible noise
// into text that extraction later reads.

// stripNUL removes NUL characters from s.
//
// The check is separate from the rewrite so the overwhelmingly common case --
// no NUL anywhere -- returns the original string without allocating.
func stripNUL(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// stripNULJSON is stripNUL for encoded JSON destined for a JSONB column.
//
// It works on the encoded bytes rather than on decoded values, so it has to
// remove two distinct forms: a raw NUL byte, which would be invalid JSON in any
// case, and the \u0000 escape, which is perfectly valid JSON that JSONB still
// refuses to store.
func stripNULJSON(b []byte) []byte {
	escaped := []byte(`\u0000`)
	if !bytes.ContainsRune(b, 0) && !bytes.Contains(b, escaped) {
		return b
	}
	b = bytes.ReplaceAll(b, []byte{0}, nil)
	return bytes.ReplaceAll(b, escaped, nil)
}
