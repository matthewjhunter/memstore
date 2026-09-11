package memstore_test

import (
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// A notice goes to the user's terminal unfenced, so stored text in it must not
// carry anything a terminal acts on: escape sequences, other control
// characters, bidi overrides, or zero-width characters that hide text.
func TestSanitizeNotice(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                          "plain text",
		"clear\x1b[2Jscreen":                  "clear[2Jscreen",
		"title\x1b]0;pwned\x07here":           "title]0;pwnedhere",
		"c1\xc2\x9b31mred":                    "c131mred",
		"rtl \xe2\x80\xaeevil\xe2\x80\xac ok": "rtl evil ok",
		"zero\xe2\x80\x8bwidth":               "zerowidth",
		"two\nlines\r\n\tand tab":             "two lines and tab",
		"  padded  ":                          "padded",
	} {
		if got := memstore.SanitizeNotice(in); got != want {
			t.Errorf("SanitizeNotice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoticeSnippet(t *testing.T) {
	if got := memstore.NoticeSnippet("short\nenough", 20); got != "short enough" {
		t.Errorf("short text = %q", got)
	}
	got := memstore.NoticeSnippet(strings.Repeat("é", 100), 10)
	if got != strings.Repeat("é", 7)+"..." {
		t.Errorf("long text = %q, want 7 runes and an ellipsis", got)
	}
}

func TestFormatNotice(t *testing.T) {
	items := []memstore.NoticeItem{
		{Kind: "fact", ID: 879, Text: "Trigger facts\x1b[31m enable loading"},
		{Kind: "task", ID: 12, Text: strings.Repeat("x", 200)},
	}
	got := memstore.FormatNotice("file context for\x1b[2J store.go", items)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header and two items:\n%s", len(lines), got)
	}
	if lines[0] != "memstore: file context for[2J store.go" {
		t.Errorf("header = %q", lines[0])
	}
	if lines[1] != "  [fact 879] Trigger facts[31m enable loading" {
		t.Errorf("fact line = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "  [task 12] xxx") || !strings.HasSuffix(lines[2], "...") ||
		len([]rune(lines[2])) > len("  [task 12] ")+memstore.NoticeSnippetMax {
		t.Errorf("long task line not truncated to %d runes: %q", memstore.NoticeSnippetMax, lines[2])
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("escape survived: %q", got)
	}

	if got := memstore.FormatNotice("nothing", nil); got != "" {
		t.Errorf("no items should give no notice, got %q", got)
	}
}

// A long list is cut, and says how much was left out.
func TestFormatNoticeCapsItems(t *testing.T) {
	var items []memstore.NoticeItem
	for i := range memstore.NoticeMaxItems + 4 {
		items = append(items, memstore.NoticeItem{Kind: "fact", ID: int64(i + 1), Text: "t"})
	}
	lines := strings.Split(memstore.FormatNotice("many", items), "\n")
	if len(lines) != memstore.NoticeMaxItems+2 {
		t.Fatalf("got %d lines, want header, %d items and a remainder line", len(lines), memstore.NoticeMaxItems)
	}
	if last := lines[len(lines)-1]; last != "  ...and 4 more" {
		t.Errorf("remainder line = %q", last)
	}
}
