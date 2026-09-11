package memstore_test

import (
	"testing"

	"github.com/matthewjhunter/memstore"
)

// Multi-line output keeps its lines and indentation and loses everything a
// terminal would act on.
func TestSanitizeTerminalText(t *testing.T) {
	in := "line one\nline two\x1b[2J\r\n\tindented\xe2\x80\xae end\xe2\x80\x8b"
	want := "line one\nline two[2J\n\tindented end"
	if got := memstore.SanitizeTerminalText(in); got != want {
		t.Errorf("SanitizeTerminalText = %q, want %q", got, want)
	}
}
