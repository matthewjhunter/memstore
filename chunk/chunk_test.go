package chunk_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/chunk"
)

func TestWeight(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   \n\t ", 0},
		{"abc", 3},
		{"a b\nc\t", 3},
		{"héllo wörld", 10}, // runes, not bytes
	}
	for _, tc := range cases {
		if got := chunk.Weight([]byte(tc.in)); got != tc.want {
			t.Errorf("Weight(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLineIndex(t *testing.T) {
	content := []byte("first\nsecond\nthird")
	ix := chunk.NewLineIndex(content)

	cases := []struct {
		off, line, lineStart int
	}{
		{0, 1, 0},
		{4, 1, 0},
		{5, 1, 0},  // the newline belongs to line 1
		{6, 2, 6},  // 's' of second
		{12, 2, 6}, // newline of line 2
		{13, 3, 13},
		{17, 3, 13},
	}
	for _, tc := range cases {
		if got := ix.Line(tc.off); got != tc.line {
			t.Errorf("Line(%d) = %d, want %d", tc.off, got, tc.line)
		}
		if got := ix.LineStart(tc.off); got != tc.lineStart {
			t.Errorf("LineStart(%d) = %d, want %d", tc.off, got, tc.lineStart)
		}
	}
}

func TestTrimTrailingWS(t *testing.T) {
	content := []byte("abc  \n\n  def   \n\n")
	if got := chunk.TrimTrailingWS(content, 0, len(content)); got != 12 {
		t.Errorf("TrimTrailingWS end = %d, want 12 (after 'def')", got)
	}
	if got := chunk.TrimTrailingWS(content, 0, 6); got != 3 {
		t.Errorf("TrimTrailingWS end = %d, want 3 (after 'abc')", got)
	}
	// Never trims past start.
	if got := chunk.TrimTrailingWS(content, 5, 6); got != 5 {
		t.Errorf("TrimTrailingWS on all-whitespace span = %d, want start 5", got)
	}
}

// verifySpans asserts the invariants every chunker output must satisfy
// against the source bytes: exact span equality, dense ordinals, strictly
// advancing starts, sane line numbers. Overlap is checked only when
// allowOverlap is false.
func verifySpans(t *testing.T, content []byte, chunks []chunk.Chunk, allowOverlap bool) {
	t.Helper()
	ix := chunk.NewLineIndex(content)
	prevStart, prevEnd := -1, 0
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Errorf("chunk %d: ordinal %d", i, c.Ordinal)
		}
		if c.ByteStart <= prevStart {
			t.Errorf("chunk %d: start %d does not advance past %d", i, c.ByteStart, prevStart)
		}
		if !allowOverlap && c.ByteStart < prevEnd {
			t.Errorf("chunk %d: span [%d,%d) overlaps previous end %d", i, c.ByteStart, c.ByteEnd, prevEnd)
		}
		if c.ByteEnd <= c.ByteStart || c.ByteEnd > len(content) {
			t.Errorf("chunk %d: bad span [%d,%d)", i, c.ByteStart, c.ByteEnd)
			continue
		}
		if c.Content != string(content[c.ByteStart:c.ByteEnd]) {
			t.Errorf("chunk %d violates the verbatim invariant:\n got %q\nwant %q", i, c.Content, content[c.ByteStart:c.ByteEnd])
		}
		if want := ix.Line(c.ByteStart); c.LineStart != want {
			t.Errorf("chunk %d: LineStart %d, want %d", i, c.LineStart, want)
		}
		if want := ix.Line(c.ByteEnd - 1); c.LineEnd != want {
			t.Errorf("chunk %d: LineEnd %d, want %d", i, c.LineEnd, want)
		}
		prevStart, prevEnd = c.ByteStart, c.ByteEnd
	}
}

func TestLineWindow_Basic(t *testing.T) {
	// ~40 lines of ~100 non-ws chars: enough for a couple of windows.
	line := strings.Repeat("abcdefghij", 10)
	content := []byte(strings.Repeat(line+"\n", 40))

	lw := chunk.NewLineWindow()
	res, err := lw.Chunk("notes.txt", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(res.Chunks) < 2 {
		t.Fatalf("expected multiple windows, got %d", len(res.Chunks))
	}
	verifySpans(t, content, res.Chunks, true)

	// Overlap: each window after the first starts before the previous ends.
	for i := 1; i < len(res.Chunks); i++ {
		if res.Chunks[i].ByteStart >= res.Chunks[i-1].ByteEnd {
			t.Errorf("window %d starts at %d, after previous end %d: no overlap", i, res.Chunks[i].ByteStart, res.Chunks[i-1].ByteEnd)
		}
	}

	// Full coverage of non-whitespace text: every line appears in some window.
	joined := ""
	for _, c := range res.Chunks {
		joined += c.Content + "\n"
	}
	if !strings.Contains(joined, line) {
		t.Error("window content lost the line text")
	}
	last := res.Chunks[len(res.Chunks)-1]
	if last.ByteEnd != chunk.TrimTrailingWS(content, 0, len(content)) {
		t.Errorf("final window ends at %d, want end of text %d", last.ByteEnd, chunk.TrimTrailingWS(content, 0, len(content)))
	}
}

func TestLineWindow_Deterministic(t *testing.T) {
	content := []byte(strings.Repeat("some plain text content on a line\n", 200))
	lw := chunk.NewLineWindow()
	a, err := lw.Chunk("a.txt", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	b, err := lw.Chunk("b.txt", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("re-chunking identical bytes produced different output")
	}
}

func TestLineWindow_SmallAndEmpty(t *testing.T) {
	lw := chunk.NewLineWindow()

	res, err := lw.Chunk("small.txt", []byte("just one short line\n"))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(res.Chunks) != 1 || res.Chunks[0].Content != "just one short line" {
		t.Errorf("small file: %+v", res.Chunks)
	}

	for _, empty := range []string{"", "\n\n\n", "   \n\t\n"} {
		res, err := lw.Chunk("empty.txt", []byte(empty))
		if err != nil {
			t.Fatalf("Chunk(%q): %v", empty, err)
		}
		if len(res.Chunks) != 0 {
			t.Errorf("whitespace-only input %q produced %d chunks", empty, len(res.Chunks))
		}
	}
}

func TestLineWindow_OneEnormousLine(t *testing.T) {
	// A single line far over Target cannot be split (whole lines only): one
	// oversized chunk, accepted.
	content := []byte(strings.Repeat("x", 5*chunk.Target) + "\n")
	lw := chunk.NewLineWindow()
	res, err := lw.Chunk("big.txt", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(res.Chunks) != 1 {
		t.Fatalf("expected 1 oversized chunk, got %d", len(res.Chunks))
	}
	verifySpans(t, content, res.Chunks, true)
}
