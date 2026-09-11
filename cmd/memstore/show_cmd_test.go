package main

import (
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// The header is one line whatever the stored fields hold: a newline or tab in a
// subject would otherwise split it, and an escape would reach the terminal.
// The content keeps its lines.
func TestWriteFactDetail(t *testing.T) {
	var b strings.Builder
	writeFactDetail(&b, memstore.Fact{
		ID:        7,
		Subject:   "sub\nject\x1b[31m",
		Category:  "cat\tegory",
		Kind:      "ki\r\nnd",
		Subsystem: "sub\u202esystem",
		Content:   "line one\nline two\x1b]0;title\x07",
		CreatedAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
	})
	want := "[id=7] sub ject[31m | cat egory | 2026-09-10 | kind=ki nd | subsystem=subsystem\n" +
		"  line one\n" +
		"  line two]0;title\n"
	if got := b.String(); got != want {
		t.Errorf("writeFactDetail:\n got %q\nwant %q", got, want)
	}
}
