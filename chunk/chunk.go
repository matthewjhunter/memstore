// Package chunk defines the chunker contract for the document corpus and the
// line-window fallback chunker. Structural chunkers live in subpackages
// (chunk/markdown, chunk/gosrc).
//
// Chunkers are pure functions of file bytes: no I/O, no network, no model.
// The corpus design depends on that -- the no-LLM-in-ingest pillar from
// docs/document-corpus.md is enforced by an import-graph test over this
// package tree, which is why nothing here may import the root memstore
// package (it carries an LLM client) or anything that reaches the network.
package chunk

import (
	"encoding/json"
	"sort"
	"unicode"
	"unicode/utf8"
)

// Sizing thresholds in non-whitespace characters (docs/document-chunking.md).
// Non-whitespace characters, not tokens: token counts would put a model
// dependency inside the ingest path and shift every boundary when the model
// changes. Not bytes: indentation would inflate measured size, severely so in
// nested code.
const (
	// Target is what the splitter aims for when it must split.
	Target = 2000
	// Max is the hard ceiling above which a section or declaration must
	// split. A single atomic block (fence, table, statement) over Max becomes
	// an oversized chunk; half a code fence retrieves as noise, an oversized
	// chunk is merely inefficient.
	Max = 8000
	// Min is the floor below which a piece merges with its siblings.
	Min = 400
)

// Chunk is one verbatim span of a file. Content must equal
// file[ByteStart:ByteEnd] exactly; the derived fields carry context for
// embed-time assembly and are never prepended to Content.
type Chunk struct {
	Ordinal   int
	Content   string
	ByteStart int
	ByteEnd   int
	LineStart int
	LineEnd   int

	// Markdown-derived context.
	HeadingPath  string // ancestor headings only, never the chunk's own
	HeadingLevel int
	Lang         string // set on split-out fences

	// Code-derived context.
	Package     string
	ImportPath  string
	Symbol      string
	Receiver    string
	DeclKind    string // func | method | type | const | var | import | package_doc
	Exported    bool
	Signature   string
	ScopePath   string // package > receiver > symbol
	ImportsUsed []string
}

// Result is one file's chunking output plus the document-level provenance the
// chunker derives from the bytes.
type Result struct {
	Chunks      []Chunk
	Title       string          // from front matter when present
	FrontMatter json.RawMessage // parsed front matter as JSON; nil when absent
	Generated   bool            // file carries a "Code generated ... DO NOT EDIT." line
}

// Chunker cuts one file's bytes into verbatim chunks.
//
// path is provenance context only -- it may inform derived metadata (import
// path, package naming) but must never influence where boundaries fall:
// chunking must be a pure function of content for file_sha256 to be a
// sufficient staleness check.
type Chunker interface {
	Chunk(path string, content []byte) (*Result, error)
	// Version identifies boundary-affecting behavior. Bump it on any change
	// that can move a cut; a differing version is grounds for re-ingest even
	// when the file hash matches.
	Version() int
	// Strategy names the chunker on the document row (e.g. "markdown", "go",
	// "line-window").
	Strategy() string
}

// Weight is the sizing measure: the number of non-whitespace characters
// (runes, not bytes) in b.
func Weight(b []byte) int {
	n := 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if !unicode.IsSpace(r) {
			n++
		}
		i += size
	}
	return n
}

// LineIndex maps byte offsets to 1-based line numbers for one file.
type LineIndex struct {
	starts []int // byte offset of each line start; starts[0] == 0
}

// NewLineIndex builds the index for content.
func NewLineIndex(content []byte) *LineIndex {
	starts := []int{0}
	for i, b := range content {
		if b == '\n' && i+1 < len(content) {
			starts = append(starts, i+1)
		}
	}
	return &LineIndex{starts: starts}
}

// Line returns the 1-based line number containing byte offset off.
func (ix *LineIndex) Line(off int) int {
	return sort.Search(len(ix.starts), func(i int) bool { return ix.starts[i] > off })
}

// LineStart returns the byte offset of the start of the line containing off.
func (ix *LineIndex) LineStart(off int) int {
	return ix.starts[ix.Line(off)-1]
}

// TrimTrailingWS returns the end offset of span [start, end) with trailing
// whitespace removed. It never trims past start.
func TrimTrailingWS(content []byte, start, end int) int {
	for end > start {
		r, size := lastRune(content[start:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	return end
}

func lastRune(b []byte) (rune, int) {
	r, size := utf8.DecodeLastRune(b)
	return r, size
}
