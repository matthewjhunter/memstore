package memstore

import (
	"fmt"
	"strings"
	"unicode"
)

// Hook notices are the short summary a hook shows the user of what it just put
// in the model's context. Claude Code shows a hook's systemMessage to the user
// and does not give it to the model, so a notice costs no context. Each item
// carries its id so it can be looked up with `memstore show <id>`.

const (
	// NoticeMaxItems is the most items a notice lists; the rest are counted.
	NoticeMaxItems = 8
	// NoticeSnippetMax is the most runes of an item's text a notice shows.
	NoticeSnippetMax = 80
)

// NoticeItem is one line of a notice, rendered "[Kind ID] Text".
type NoticeItem struct {
	Kind string // "fact", "task" or "hint"
	ID   int64
	Text string
}

// SanitizeNotice makes stored text safe to print on one terminal line. A
// notice never reaches the model, so it is not fenced, but it is printed as it
// is. Control characters are removed, among them the escape that starts every
// terminal sequence, and so are Unicode format characters, among them the bidi
// overrides and zero-width characters that make text read as something it is
// not. Any run of whitespace becomes one space.
func SanitizeNotice(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// SanitizeTerminalText is SanitizeNotice for text printed over several lines:
// newlines and tabs are kept, every other control and format character is
// removed.
func SanitizeTerminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// NoticeSnippet is s sanitized and cut to at most max runes, with an ellipsis
// when it was cut.
func NoticeSnippet(s string, max int) string {
	t := []rune(SanitizeNotice(s))
	if len(t) <= max {
		return string(t)
	}
	if max < 4 {
		return string(t[:max])
	}
	return string(t[:max-3]) + "..."
}

// FormatNotice renders a notice: "memstore: header", then one indented line
// per item, at most NoticeMaxItems of them. No items means no notice.
func FormatNotice(header string, items []NoticeItem) string {
	if len(items) == 0 {
		return ""
	}
	lines := []string{"memstore: " + SanitizeNotice(header)}
	for _, it := range items[:min(len(items), NoticeMaxItems)] {
		line := fmt.Sprintf("  [%s %d]", SanitizeNotice(it.Kind), it.ID)
		if text := NoticeSnippet(it.Text, NoticeSnippetMax); text != "" {
			line += " " + text
		}
		lines = append(lines, line)
	}
	if rest := len(items) - NoticeMaxItems; rest > 0 {
		lines = append(lines, fmt.Sprintf("  ...and %d more", rest))
	}
	return strings.Join(lines, "\n")
}
