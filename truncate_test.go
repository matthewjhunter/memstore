package memstore_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/matthewjhunter/memstore"
)

// Slicing a string at a raw byte offset cuts whatever happens to be there,
// including the middle of a multi-byte rune. The tail then decodes as U+FFFD,
// and for the extract queue that mojibake goes into an LLM prompt and into
// stored fact content.
func TestTruncate_CutsOnARuneBoundary(t *testing.T) {
	// Four 3-byte runes: a cut at 5 bytes lands inside the second one.
	s := "日本語だ"
	got := memstore.Truncate(s, 5)
	if !utf8.ValidString(got) {
		t.Errorf("Truncate(%q, 5) = %q, which is not valid UTF-8", s, got)
	}
	if want := "日" + memstore.TruncMarker; got != want {
		t.Errorf("Truncate(%q, 5) = %q, want %q -- the straddling rune is dropped whole", s, got, want)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("Truncate produced a replacement char: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exact", 5, "exact"},
		{"truncated", 4, "trun..."},
		{"日本語", 9, "日本語"},   // fits exactly, no marker
		{"日本語", 8, "日本..."}, // one byte short, drops the last rune
		{"日本語", 2, memstore.TruncMarker},
		{"日本語", 0, memstore.TruncMarker},
		{"", 0, ""},
	}
	for _, c := range cases {
		if got := memstore.Truncate(c.in, c.max); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// TruncateRunes budgets the whole result, marker included, because its
// callers are fitting a column: fmt measures %-18s in runes, so a helper that
// counted bytes would overflow the column on any non-ASCII subject.
func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactlyten", 10, "exactlyten"},
		{"abcdefghijkl", 10, "abcdefg..."},
		{"日本語だよここ", 5, "日本..."},
		{"abcdef", 3, "abc"}, // no room for content plus marker
		{"abcdef", 2, "ab"},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		got := memstore.TruncateRunes(c.in, c.max)
		if got != c.want {
			t.Errorf("TruncateRunes(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if n := utf8.RuneCountInString(got); n > c.max && c.max > 0 {
			t.Errorf("TruncateRunes(%q, %d) = %q, %d runes -- over budget", c.in, c.max, got, n)
		}
		if !utf8.ValidString(got) {
			t.Errorf("TruncateRunes(%q, %d) = %q, not valid UTF-8", c.in, c.max, got)
		}
	}
}
