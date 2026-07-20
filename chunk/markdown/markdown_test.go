package markdown_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/chunk"
	"github.com/matthewjhunter/memstore/chunk/markdown"
)

// readOwnDesignDoc returns a real markdown corpus sample: the design doc
// this chunker implements.
func readOwnDesignDoc(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../docs/document-corpus.md")
	if err != nil {
		t.Fatalf("reading design doc: %v", err)
	}
	return b
}

func mustChunk(t *testing.T, content string) *chunk.Result {
	t.Helper()
	res, err := markdown.New().Chunk("doc.md", []byte(content))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	return res
}

// verifySpans is the shared invariant battery: verbatim spans, dense
// ordinals, strict advance, no overlap (markdown never overlaps).
func verifySpans(t *testing.T, content string, chunks []chunk.Chunk) {
	t.Helper()
	prevStart, prevEnd := -1, 0
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Errorf("chunk %d: ordinal %d", i, c.Ordinal)
		}
		if c.ByteStart <= prevStart || c.ByteStart < prevEnd {
			t.Errorf("chunk %d: span [%d,%d) does not advance past [%d,%d)", i, c.ByteStart, c.ByteEnd, prevStart, prevEnd)
		}
		if c.ByteEnd <= c.ByteStart || c.ByteEnd > len(content) {
			t.Fatalf("chunk %d: bad span [%d,%d)", i, c.ByteStart, c.ByteEnd)
		}
		if c.Content != content[c.ByteStart:c.ByteEnd] {
			t.Errorf("chunk %d violates the verbatim invariant:\n got: %q\nwant: %q", i, c.Content, content[c.ByteStart:c.ByteEnd])
		}
		prevStart, prevEnd = c.ByteStart, c.ByteEnd
	}
}

func TestMarkdown_SectionPerHeading(t *testing.T) {
	content := `# Title

Intro paragraph with enough words to matter for the retrieval test battery here.

## First section

Body of the first section, which discusses the first topic in reasonable depth and detail.

## Second section

Body of the second section, which discusses an unrelated second topic at similar length.
`
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)

	// Stub sections merge (all are far below Min and are siblings at their
	// levels), so assert structure rather than an exact count: every chunk
	// starts at its own heading line, and heading_path carries ancestors
	// only.
	if len(res.Chunks) == 0 {
		t.Fatal("no chunks")
	}
	first := res.Chunks[0]
	if !strings.HasPrefix(first.Content, "# Title") {
		t.Errorf("first chunk does not start at its heading: %q", first.Content)
	}
	if first.HeadingPath != "" {
		t.Errorf("top-level section carries ancestors %q; want none", first.HeadingPath)
	}
	for _, c := range res.Chunks {
		if strings.Contains(c.HeadingPath, "First section") && !strings.Contains(c.Content, "## First section") {
			// A chunk under "First section" would carry it as ancestor only
			// if it were a subsection; there are none in this document.
			t.Errorf("heading_path contains the chunk's own heading: %+v", c)
		}
	}
}

func TestMarkdown_HeadingPathAncestorsOnly(t *testing.T) {
	// Make each section big enough (> Min in non-ws chars) that nothing
	// merges, so the ancestor paths stay observable.
	pad := strings.Repeat("Detailed prose sentence with content words repeated for sizing purposes here. ", 8)
	content := fmt.Sprintf(`# Design

%s

## Schema

%s

### Vectors come later

%s
`, pad, pad, pad)

	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if len(res.Chunks) != 3 {
		t.Fatalf("expected 3 section chunks, got %d: %+v", len(res.Chunks), res.Chunks)
	}

	want := []struct {
		path  string
		level int
	}{
		{"", 1},
		{"Design", 2},
		{"Design > Schema", 3},
	}
	for i, w := range want {
		if res.Chunks[i].HeadingPath != w.path || res.Chunks[i].HeadingLevel != w.level {
			t.Errorf("chunk %d: heading_path=%q level=%d, want %q/%d",
				i, res.Chunks[i].HeadingPath, res.Chunks[i].HeadingLevel, w.path, w.level)
		}
	}
}

func TestMarkdown_FrontMatterYAML(t *testing.T) {
	content := `---
title: The Auth Design
tags: [auth, oidc]
---

# Body

Prose after front matter.
`
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)

	if res.Title != "The Auth Design" {
		t.Errorf("title = %q", res.Title)
	}
	var fm map[string]any
	if err := json.Unmarshal(res.FrontMatter, &fm); err != nil {
		t.Fatalf("front matter not valid JSON: %v", err)
	}
	if fm["title"] != "The Auth Design" {
		t.Errorf("front matter mangled: %v", fm)
	}
	// Front matter is not emitted as a chunk; the first chunk starts at the
	// markdown body.
	if len(res.Chunks) == 0 {
		t.Fatal("no chunks")
	}
	if res.Chunks[0].ByteStart == 0 || !strings.HasPrefix(res.Chunks[0].Content, "# Body") {
		t.Errorf("front matter leaked into chunks: %+v", res.Chunks[0])
	}
}

func TestMarkdown_FrontMatterTOML(t *testing.T) {
	content := `+++
title = "TOML Doc"
+++

Body prose.
`
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if res.Title != "TOML Doc" {
		t.Errorf("title = %q", res.Title)
	}
}

func TestMarkdown_MalformedFrontMatterIsContent(t *testing.T) {
	content := "---\nnot: [valid: yaml: here\n---\n\nBody.\n"
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if res.FrontMatter != nil {
		t.Errorf("malformed front matter parsed: %s", res.FrontMatter)
	}
}

func TestMarkdown_MidsizeSectionIsOneChunk(t *testing.T) {
	// A section between Min and Max emits as exactly one chunk.
	body := strings.Repeat("A sentence of prose with several content words in it. ", 30) // ~1350 non-ws
	content := "## Section\n\n" + body + "\n"
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if len(res.Chunks) != 1 {
		t.Fatalf("mid-size section split into %d chunks", len(res.Chunks))
	}
	w := chunk.Weight([]byte(res.Chunks[0].Content))
	if w < chunk.Min || w > chunk.Max {
		t.Errorf("section weight %d outside [min,max]", w)
	}
}

func TestMarkdown_OversizedSectionSplitsAtBlockBoundaries(t *testing.T) {
	para := strings.Repeat("Words of prose accumulate toward the sizing ceiling in this paragraph. ", 20) // ~1300 non-ws each
	var sb strings.Builder
	sb.WriteString("## Big section\n\n")
	for i := 0; i < 10; i++ { // ~13000 non-ws total: must split
		sb.WriteString(para)
		sb.WriteString("\n\n")
	}
	content := sb.String()

	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if len(res.Chunks) < 2 {
		t.Fatalf("oversized section did not split: %d chunks", len(res.Chunks))
	}
	for i, c := range res.Chunks {
		if w := chunk.Weight([]byte(c.Content)); w > chunk.Max {
			t.Errorf("chunk %d weight %d exceeds max", i, w)
		}
		// Splits happen at paragraph boundaries: every chunk starts either at
		// the heading or at a paragraph start, never mid-paragraph.
		if i > 0 && !strings.HasPrefix(c.Content, "Words of prose") {
			t.Errorf("chunk %d does not start at a block boundary: %.40q", i, c.Content)
		}
	}
}

func TestMarkdown_FenceNeverSplit(t *testing.T) {
	// A section whose bulk is one enormous fence: the fence must arrive
	// intact in a single chunk even though it exceeds Max.
	var code strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&code, "    line_%04d := compute(%d) // fenced code line with some width\n", i, i)
	}
	content := "## Example\n\nIntro sentence.\n\n```go\n" + code.String() + "```\n\nTrailing prose.\n"

	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)

	var fenceChunks int
	for _, c := range res.Chunks {
		open := strings.Count(c.Content, "```")
		if open%2 != 0 {
			t.Errorf("chunk boundary falls inside a fence:\n%.80q ... %.80q", c.Content, c.Content[len(c.Content)-80:])
		}
		if strings.Contains(c.Content, "line_0000") {
			fenceChunks++
			if !strings.Contains(c.Content, "line_0399") {
				t.Error("fence was split across chunks")
			}
			if c.Lang != "go" {
				t.Errorf("split-out fence lang = %q, want go", c.Lang)
			}
		}
	}
	if fenceChunks != 1 {
		t.Errorf("fence appears in %d chunks, want 1", fenceChunks)
	}
}

func TestMarkdown_SmallFenceStaysWithProse(t *testing.T) {
	content := "## Usage\n\nRun the command:\n\n```sh\nmemstore ingest .\n```\n\nAnd check the output.\n"
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if len(res.Chunks) != 1 {
		t.Fatalf("small fence split out of its section: %d chunks", len(res.Chunks))
	}
	if res.Chunks[0].Lang != "" {
		t.Errorf("in-section chunk has lang %q; lang marks split-out fences only", res.Chunks[0].Lang)
	}
	if !strings.Contains(res.Chunks[0].Content, "memstore ingest .") {
		t.Error("fence content missing from section chunk")
	}
}

func TestMarkdown_StubSectionsMerge(t *testing.T) {
	content := `## A

One line.

## B

Another line.

## C

Third line.
`
	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	if len(res.Chunks) != 1 {
		t.Fatalf("three stub siblings should merge into one chunk, got %d", len(res.Chunks))
	}
	c := res.Chunks[0]
	for _, want := range []string{"## A", "## B", "## C"} {
		if !strings.Contains(c.Content, want) {
			t.Errorf("merged chunk missing %q", want)
		}
	}
}

func TestMarkdown_TableIsAtomic(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("| col_a | col_b |\n|---|---|\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&rows, "| value_a_%04d_with_padding_text | value_b_%04d_with_padding_text |\n", i, i)
	}
	content := "## Data\n\n" + rows.String() + "\nAfter table.\n"

	res := mustChunk(t, content)
	verifySpans(t, content, res.Chunks)
	var tableChunks int
	for _, c := range res.Chunks {
		if strings.Contains(c.Content, "value_a_0000") {
			tableChunks++
			if !strings.Contains(c.Content, "value_a_0299") {
				t.Error("table was split across chunks")
			}
		}
	}
	if tableChunks != 1 {
		t.Errorf("table appears in %d chunks, want 1", tableChunks)
	}
}

func TestMarkdown_Deterministic(t *testing.T) {
	content := readOwnDesignDoc(t)
	c := markdown.New()
	a, err := c.Chunk("a.md", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	b, err := c.Chunk("b.md", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("re-chunking identical bytes produced different output")
	}
}

// TestMarkdown_RealDesignDoc runs the chunker over this repo's own design
// doc -- setext edge cases, fences, nested structure -- and checks only the
// invariants, not specific boundaries.
func TestMarkdown_RealDesignDoc(t *testing.T) {
	content := readOwnDesignDoc(t)
	res, err := markdown.New().Chunk("docs/document-corpus.md", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	verifySpans(t, string(content), res.Chunks)
	if len(res.Chunks) < 5 {
		t.Errorf("suspiciously few chunks for a real design doc: %d", len(res.Chunks))
	}
	for i, c := range res.Chunks {
		if strings.Count(c.Content, "```")%2 != 0 {
			t.Errorf("chunk %d: boundary inside a fence", i)
		}
	}
}

func TestMarkdown_EmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "\n\n\n", "   \n"} {
		res := mustChunk(t, in)
		if len(res.Chunks) != 0 {
			t.Errorf("input %q produced %d chunks", in, len(res.Chunks))
		}
	}
}
