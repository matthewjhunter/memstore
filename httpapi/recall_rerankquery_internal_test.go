package httpapi

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRerankQueryCutsOnRuneBoundaries(t *testing.T) {
	// Three-byte runes, so a cut at an arbitrary byte lands inside one.
	prompt := strings.Repeat("日本語", 500)
	for _, limit := range []int{50, 51, 52, 53, 200} {
		got := rerankQuery(prompt, limit)
		if len(got) > limit {
			t.Errorf("limit %d: got %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: result is not valid UTF-8", limit)
		}
	}
}
