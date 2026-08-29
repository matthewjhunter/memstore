package memstore

import "unicode/utf8"

// TruncMarker is appended to a string that had to be cut short.
const TruncMarker = "..."

// Truncate limits s to maxBytes bytes of content and appends TruncMarker if
// anything was dropped, so the result can be up to maxBytes+len(TruncMarker)
// bytes. The cut is moved back to a rune boundary: slicing a string at a raw
// byte offset splits whatever rune straddles it, and the broken tail decodes
// as U+FFFD wherever the value lands next -- an LLM prompt or a stored fact,
// in the extract queue's case.
//
// Use this where the budget is a byte cap on content. Where the whole result
// has to fit a width, use TruncateRunes.
func Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return truncBytes(s, maxBytes) + TruncMarker
}

// TruncateRunes limits the whole result -- content and marker together -- to
// maxRunes runes. Runes rather than bytes because its callers are fitting a
// column and fmt measures a %-Ns width in runes; a byte budget would overflow
// the column on any non-ASCII input.
//
// With no room for content alongside the marker, the marker is dropped rather
// than the content: at that width a bare "..." says nothing at all.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	marker := utf8.RuneCountInString(TruncMarker)
	if maxRunes <= marker {
		return string([]rune(s)[:maxRunes])
	}
	return string([]rune(s)[:maxRunes-marker]) + TruncMarker
}

// truncBytes returns s cut to at most n bytes, backing up off any partial
// rune at the cut.
func truncBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
