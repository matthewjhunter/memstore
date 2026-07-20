package chunk

// LineWindow is the fallback chunker for plain text with no structure to cut
// on: non-markdown text files, and source files that fail to parse
// (docs/code-chunking.md). It packs whole lines into windows of roughly
// Target non-whitespace characters and, unlike the structural chunkers,
// keeps a small overlap between windows -- with no structural boundary to
// cut at, the overlap is what keeps a match near a window edge retrievable
// with its context.
type LineWindow struct{}

// LineWindowVersion identifies boundary-affecting behavior of the
// line-window chunker.
const LineWindowVersion = 1

// lineWindowOverlap is the approximate size of the tail, in non-whitespace
// characters, that each window shares with the next.
const lineWindowOverlap = Target / 10

// NewLineWindow returns the fallback chunker.
func NewLineWindow() *LineWindow { return &LineWindow{} }

// Version implements Chunker.
func (l *LineWindow) Version() int { return LineWindowVersion }

// Strategy implements Chunker.
func (l *LineWindow) Strategy() string { return "line-window" }

// Chunk implements Chunker. path is unused: the fallback derives nothing.
func (l *LineWindow) Chunk(_ string, content []byte) (*Result, error) {
	ix := NewLineIndex(content)

	// Collect line spans, skipping nothing: blank lines belong to whichever
	// window spans them.
	type line struct {
		start, end int // [start, end) including the newline
		weight     int
	}
	var lines []line
	pos := 0
	for pos < len(content) {
		end := pos
		for end < len(content) && content[end] != '\n' {
			end++
		}
		if end < len(content) {
			end++ // include the newline
		}
		lines = append(lines, line{start: pos, end: end, weight: Weight(content[pos:end])})
		pos = end
	}

	res := &Result{}
	emit := func(startLine, endLine int) { // [startLine, endLine) indices into lines
		start := lines[startLine].start
		end := TrimTrailingWS(content, start, lines[endLine-1].end)
		if end <= start {
			return // whitespace-only window
		}
		res.Chunks = append(res.Chunks, Chunk{
			Ordinal:   len(res.Chunks),
			Content:   string(content[start:end]),
			ByteStart: start,
			ByteEnd:   end,
			LineStart: ix.Line(start),
			LineEnd:   ix.Line(end - 1),
		})
	}

	winStart := 0
	for winStart < len(lines) {
		i := winStart
		weight := 0
		for i < len(lines) {
			weight += lines[i].weight
			i++
			if weight >= Target {
				break
			}
		}
		emit(winStart, i)
		if i >= len(lines) {
			break
		}
		// Overlap: back up over the tail of this window until roughly
		// lineWindowOverlap non-whitespace characters are shared, but always
		// advance by at least one line so spans strictly increase.
		next := i
		tail := 0
		for next-1 > winStart && tail+lines[next-1].weight <= lineWindowOverlap {
			next--
			tail += lines[next].weight
		}
		if next <= winStart {
			next = winStart + 1
		}
		winStart = next
	}

	return res, nil
}

var _ Chunker = (*LineWindow)(nil)
