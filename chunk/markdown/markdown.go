// Package markdown implements the markdown chunker from
// docs/document-chunking.md: front matter parsed into document fields,
// sections segmented at headings, oversized sections split at top-level
// block boundaries, undersized sibling sections merged, fences and tables
// never split.
package markdown

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"gopkg.in/yaml.v3"

	"github.com/matthewjhunter/memstore/chunk"
)

// Version identifies boundary-affecting behavior. Bump on any change that
// can move a cut -- goldmark upgrades that shift the AST count too.
const Version = 1

// Chunker chunks CommonMark (plus tables) markdown.
type Chunker struct {
	md goldmark.Markdown
}

// New returns a markdown chunker.
func New() *Chunker {
	return &Chunker{
		// Only the block parser is used; the renderer is never invoked.
		// The table extension makes GFM tables single atomic blocks.
		md: goldmark.New(goldmark.WithExtensions(extension.Table)),
	}
}

// Version implements chunk.Chunker.
func (c *Chunker) Version() int { return Version }

// Strategy implements chunk.Chunker.
func (c *Chunker) Strategy() string { return "markdown" }

var _ chunk.Chunker = (*Chunker)(nil)

// block is one top-level markdown block, located by absolute byte span.
type block struct {
	start, end int
	weight     int
	isFence    bool
	lang       string // fence info-string language, when isFence
	heading    int    // heading level when this block is a heading, else 0
	headText   string
}

// section is a run of blocks from one heading (or the preamble) to the next.
type section struct {
	headingPath  string // ancestors only
	headingLevel int
	blocks       []block
}

func (s *section) weight() int {
	n := 0
	for _, b := range s.blocks {
		n += b.weight
	}
	return n
}

// Chunk implements chunk.Chunker.
func (c *Chunker) Chunk(_ string, content []byte) (*chunk.Result, error) {
	res := &chunk.Result{}

	bodyStart := extractFrontMatter(content, res)
	body := content[bodyStart:]

	doc := c.md.Parser().Parse(text.NewReader(body))
	blocks := c.topLevelBlocks(doc, body, bodyStart, content)
	if len(blocks) == 0 {
		return res, nil
	}

	sections := buildSections(blocks)
	sections = mergeUndersized(sections)

	ix := chunk.NewLineIndex(content)
	for _, sec := range sections {
		emitSection(res, content, ix, sec)
	}
	return res, nil
}

// extractFrontMatter parses a leading YAML (---) or TOML (+++) front matter
// fence into Result.FrontMatter/Title and returns the byte offset where the
// markdown body begins. Malformed front matter is treated as content: the
// chunker must be deterministic, never wrong-but-plausible.
func extractFrontMatter(content []byte, res *chunk.Result) int {
	var delim string
	switch {
	case bytes.HasPrefix(content, []byte("---\n")) || bytes.HasPrefix(content, []byte("---\r\n")):
		delim = "---"
	case bytes.HasPrefix(content, []byte("+++\n")) || bytes.HasPrefix(content, []byte("+++\r\n")):
		delim = "+++"
	default:
		return 0
	}

	firstEOL := bytes.IndexByte(content, '\n') + 1
	rest := content[firstEOL:]
	end := -1 // offset in rest of the closing delimiter line
	for pos := 0; pos < len(rest); {
		eol := bytes.IndexByte(rest[pos:], '\n')
		lineEnd := len(rest)
		next := len(rest)
		if eol >= 0 {
			lineEnd = pos + eol
			next = pos + eol + 1
		}
		line := strings.TrimRight(string(rest[pos:lineEnd]), "\r")
		if line == delim {
			end = pos
			// Body starts after the closing delimiter line.
			bodyOff := firstEOL + next
			raw := rest[:end]

			var parsed map[string]any
			var err error
			if delim == "---" {
				err = yaml.Unmarshal(raw, &parsed)
			} else {
				err = toml.Unmarshal(raw, &parsed)
			}
			if err != nil || parsed == nil {
				return 0
			}
			jsonBytes, err := json.Marshal(parsed)
			if err != nil {
				return 0
			}
			res.FrontMatter = jsonBytes
			if title, ok := parsed["title"].(string); ok {
				res.Title = title
			}
			return bodyOff
		}
		pos = next
	}
	return 0
}

// topLevelBlocks walks the document's direct children and locates each one
// as an absolute byte span in content. bodyStart is the offset of body
// within content; spans are returned in content coordinates.
//
// goldmark does not expose full block extents, so spans are derived from
// block starts: block i runs to the start of block i+1, trimmed of trailing
// whitespace (which is why inter-block whitespace lands in no chunk). Blocks
// whose start cannot be located (thematic breaks, which carry no text
// segments; degenerate empty fences) are subsumed into the preceding block's
// span, which keeps the partition verbatim at the cost of a stray "***" in a
// neighbor chunk.
func (c *Chunker) topLevelBlocks(doc ast.Node, body []byte, bodyStart int, content []byte) []block {
	type located struct {
		node  ast.Node
		start int // body coordinates, at a line start
	}
	var nodes []located
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		if s := blockStart(n, body); s >= 0 {
			nodes = append(nodes, located{node: n, start: s})
		}
	}

	var blocks []block
	for i, ln := range nodes {
		end := len(body)
		if i+1 < len(nodes) {
			end = nodes[i+1].start
		}
		start := bodyStart + ln.start
		absEnd := chunk.TrimTrailingWS(content, start, bodyStart+end)
		if absEnd <= start {
			continue
		}
		b := block{
			start:  start,
			end:    absEnd,
			weight: chunk.Weight(content[start:absEnd]),
		}
		switch n := ln.node.(type) {
		case *ast.FencedCodeBlock:
			b.isFence = true
			if n.Info != nil {
				info := string(n.Info.Value(body))
				if f := strings.Fields(info); len(f) > 0 {
					b.lang = f[0]
				}
			}
		case *ast.Heading:
			b.heading = n.Level
			b.headText = string(nodeText(n, body))
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// blockStart returns the byte offset (body coordinates, at a line start) of
// a top-level block, or -1 when it cannot be located.
func blockStart(n ast.Node, body []byte) int {
	if f, ok := n.(*ast.FencedCodeBlock); ok {
		// Lines() covers the inner code only; the opening fence is the line
		// before the first content line, or the line carrying the info
		// string.
		if f.Info != nil {
			return lineStartAt(body, segStart(f.Info.Segment.Start, body))
		}
		if f.Lines().Len() > 0 {
			firstContent := lineStartAt(body, f.Lines().At(0).Start)
			return prevLineStart(body, firstContent)
		}
		return -1
	}
	min := -1
	walkSegments(n, func(start int) {
		if min < 0 || start < min {
			min = start
		}
	})
	if min < 0 {
		return -1
	}
	return lineStartAt(body, min)
}

// segStart clamps a segment position into body range.
func segStart(pos int, body []byte) int {
	if pos > len(body) {
		return len(body)
	}
	return pos
}

// walkSegments visits the start offset of every text segment in n's subtree.
func walkSegments(n ast.Node, visit func(start int)) {
	if n.Type() == ast.TypeBlock {
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			visit(lines.At(i).Start)
		}
	}
	if t, ok := n.(*ast.Text); ok {
		visit(t.Segment.Start)
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		walkSegments(c, visit)
	}
}

// lineStartAt returns the offset of the start of the line containing off.
func lineStartAt(body []byte, off int) int {
	if off > len(body) {
		off = len(body)
	}
	i := bytes.LastIndexByte(body[:off], '\n')
	return i + 1
}

// prevLineStart returns the start of the line before the line starting at
// lineStart.
func prevLineStart(body []byte, lineStart int) int {
	if lineStart == 0 {
		return 0
	}
	return lineStartAt(body, lineStart-1)
}

// nodeText collects the plain text of an inline subtree (heading titles).
func nodeText(n ast.Node, body []byte) []byte {
	var buf bytes.Buffer
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		if t, ok := n.(*ast.Text); ok {
			buf.Write(t.Segment.Value(body))
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			walk(c)
		}
	}
	walk(n)
	return buf.Bytes()
}

// buildSections partitions blocks at every heading. Each heading starts a new
// section whose heading_path carries ancestors only -- the section's own
// heading line is the first bytes of its content.
func buildSections(blocks []block) []section {
	type stackEntry struct {
		level int
		text  string
	}
	var stack []stackEntry
	var sections []section
	cur := &section{}

	flush := func() {
		if len(cur.blocks) > 0 {
			sections = append(sections, *cur)
		}
	}

	for _, b := range blocks {
		if b.heading > 0 {
			flush()
			for len(stack) > 0 && stack[len(stack)-1].level >= b.heading {
				stack = stack[:len(stack)-1]
			}
			parts := make([]string, 0, len(stack))
			for _, e := range stack {
				parts = append(parts, e.text)
			}
			cur = &section{
				headingPath:  strings.Join(parts, " > "),
				headingLevel: b.heading,
				blocks:       []block{b},
			}
			stack = append(stack, stackEntry{level: b.heading, text: b.headText})
			continue
		}
		cur.blocks = append(cur.blocks, b)
	}
	flush()
	return sections
}

// mergeUndersized merges a section below Min into the section that follows
// it when the two are siblings -- same heading level, same ancestors -- and
// keeps merging while the total stays under Target. Stub sections that are
// only a heading and a sentence retrieve badly alone. The merged section
// keeps the first section's heading path (the open question in
// docs/document-chunking.md; deferred until real corpora show how often it
// fires).
func mergeUndersized(sections []section) []section {
	var out []section
	for i := 0; i < len(sections); i++ {
		sec := sections[i]
		for sec.weight() < chunk.Min && i+1 < len(sections) {
			next := sections[i+1]
			if next.headingLevel != sec.headingLevel || next.headingPath != sec.headingPath {
				break
			}
			if sec.weight()+next.weight() > chunk.Target {
				break
			}
			sec.blocks = append(sec.blocks, next.blocks...)
			i++
		}
		out = append(out, sec)
	}
	return out
}

// emitSection turns one section into chunks. A section at or below Max is a
// single chunk -- unless it contains a fence over Target, which becomes its
// own chunk with lang set (those are the genuinely standalone examples, and
// lang makes them routable to the code space). A section over Max splits at
// top-level block boundaries into pieces packed toward Target; an atomic
// block over Max is emitted oversized rather than split.
func emitSection(res *chunk.Result, content []byte, ix *chunk.LineIndex, sec section) {
	bigFence := false
	for _, b := range sec.blocks {
		if b.isFence && b.weight > chunk.Target {
			bigFence = true
			break
		}
	}

	if !bigFence && sec.weight() <= chunk.Max {
		emitSpan(res, content, ix, sec, sec.blocks[0].start, sec.blocks[len(sec.blocks)-1].end, "")
		return
	}

	var piece []block
	pieceWeight := 0
	flush := func() {
		if len(piece) == 0 {
			return
		}
		emitSpan(res, content, ix, sec, piece[0].start, piece[len(piece)-1].end, "")
		piece, pieceWeight = nil, 0
	}
	for _, b := range sec.blocks {
		if b.isFence && b.weight > chunk.Target {
			flush()
			emitSpan(res, content, ix, sec, b.start, b.end, b.lang)
			continue
		}
		if pieceWeight > 0 && pieceWeight+b.weight > chunk.Max {
			flush()
		}
		piece = append(piece, b)
		pieceWeight += b.weight
		if pieceWeight >= chunk.Target {
			flush()
		}
	}
	flush()
}

func emitSpan(res *chunk.Result, content []byte, ix *chunk.LineIndex, sec section, start, end int, lang string) {
	res.Chunks = append(res.Chunks, chunk.Chunk{
		Ordinal:      len(res.Chunks),
		Content:      string(content[start:end]),
		ByteStart:    start,
		ByteEnd:      end,
		LineStart:    ix.Line(start),
		LineEnd:      ix.Line(end - 1),
		HeadingPath:  sec.headingPath,
		HeadingLevel: sec.headingLevel,
		Lang:         lang,
	})
}
