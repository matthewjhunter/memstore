// Package transcript reads a raw Claude Code session transcript -- one JSON
// object per line -- and derives the structure the session archive stores:
// the session's own metadata, its compactions, the segments between them and
// an index of its tool calls.
//
// It is pure: a reader in, a value out, no store and no clock. Everything it
// returns addresses the raw bytes by line number and byte offset, so a segment
// or a tool result can be read back out of the file without parsing the rest.
//
// The transcript format is not documented and not stable. What this package
// relies on is listed in docs/session-archive.md, and Version changes whenever
// the derivation does, so stored rows can say which reading produced them.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// Version identifies this package's reading of a transcript. Bump it when the
// same bytes would derive differently.
const Version = 1

// None marks a segment link that points at no compaction.
const None = -1

// Session is everything derived from one transcript.
type Session struct {
	ID    string // sessionId of the first entry that carries one
	Title string // the last ai-title entry
	CWD   string // cwd of the first entry that carries one

	StartedAt, EndedAt time.Time // earliest and latest entry timestamp

	Lines    int   // lines in the file, blank and unparseable ones included
	BadLines int   // non-blank lines that were not a JSON object
	Bytes    int64 // bytes read

	Compactions []Compaction
	Segments    []Segment
	Tools       []ToolCall
}

// Compaction is one point where the conversation was replaced by a summary of
// itself. FirstLine..LastLine are the lines that make it up -- the boundary
// marker and the summary, in whichever order the file has them, or the summary
// alone -- and belong to neither neighbouring segment.
type Compaction struct {
	Format      string // name of the detector that recognised it
	Summary     string
	SummaryUUID string
	Trigger     string // "auto" or "manual" when the format records it
	PreTokens   int    // context size before compaction, when recorded
	At          time.Time

	FirstLine, LastLine int
}

// Segment is a run of the transcript between compactions. After is the index
// in Session.Compactions of the compaction it started from, Before the one
// that followed it; either is None when there is none. Compaction Before is
// the model's own summary of this segment.
type Segment struct {
	Ordinal             int
	FirstLine, LastLine int   // 1-based, inclusive
	StartOffset         int64 // of FirstLine
	EndOffset           int64 // one past LastLine, newline included
	StartedAt, EndedAt  time.Time
	After, Before       int
}

// ToolCall is one tool_use block and, when the transcript has it, the
// tool_result that answered it. The input is kept; the result is addressed.
type ToolCall struct {
	ID        string
	Name      string
	Input     json.RawMessage
	Line      int // line of the assistant entry that made the call
	At        time.Time
	Segment   int  // ordinal of the segment holding Line, or None
	Sidechain bool // made on a sidechain (a subagent's turn)

	HasResult    bool
	IsError      bool
	ResultLine   int
	ResultOffset int64 // of ResultLine in the raw bytes
	ResultBytes  int   // size of the tool_result block as written
}

// entry is one parsed line: the fields any detector or index needs, and where
// the line sits in the file.
type entry struct {
	line   int
	offset int64
	end    int64

	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	ParentUUID       string          `json:"parentUuid"`
	SessionID        string          `json:"sessionId"`
	CWD              string          `json:"cwd"`
	Timestamp        time.Time       `json:"timestamp"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsSidechain      bool            `json:"isSidechain"`
	AITitle          string          `json:"aiTitle"`
	CompactMetadata  json.RawMessage `json:"compactMetadata"`
	Message          struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// block is one typed item of a message's content array.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// blocks returns the content array of a message, with each block's raw bytes
// alongside it. A string content has no blocks.
func (e *entry) blocks() ([]block, []json.RawMessage) {
	var raws []json.RawMessage
	if json.Unmarshal(e.Message.Content, &raws) != nil {
		return nil, nil
	}
	out := make([]block, len(raws))
	for i, r := range raws {
		_ = json.Unmarshal(r, &out[i])
	}
	return out, raws
}

// text returns the message's text: the string itself, or its text blocks
// joined.
func (e *entry) text() string {
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		return s
	}
	bs, _ := e.blocks()
	var parts []string
	for _, b := range bs {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Parse reads a transcript to its end. A line that is not a JSON object is
// counted in BadLines and otherwise ignored; the only error is a read error.
func Parse(r io.Reader) (*Session, error) {
	s := &Session{}
	entries, err := readEntries(r, s)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		e := &entries[i]
		if s.ID == "" {
			s.ID = e.SessionID
		}
		if s.CWD == "" {
			s.CWD = e.CWD
		}
		if e.Type == "ai-title" && e.AITitle != "" {
			s.Title = e.AITitle
		}
		if !e.Timestamp.IsZero() {
			if s.StartedAt.IsZero() || e.Timestamp.Before(s.StartedAt) {
				s.StartedAt = e.Timestamp
			}
			if e.Timestamp.After(s.EndedAt) {
				s.EndedAt = e.Timestamp
			}
		}
	}
	s.Compactions = detectCompactions(entries)
	s.Segments = segment(entries, s.Compactions)
	s.Tools = indexTools(entries, s.Segments)
	return s, nil
}

// readEntries splits the stream into lines without a length limit -- a tool
// result can be megabytes on one line -- recording where each line sits.
func readEntries(r io.Reader, s *Session) ([]entry, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	var entries []entry
	var offset int64
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			s.Lines++
			start := offset
			offset += int64(len(raw))
			if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
				e := entry{line: s.Lines, offset: start, end: offset}
				if json.Unmarshal(trimmed, &e) != nil {
					s.BadLines++
				} else {
					entries = append(entries, e)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	s.Bytes = offset
	return entries, nil
}

// segment cuts the entries into the runs between compactions. A compaction's
// own lines fall in no segment, and a run with no entries makes no segment.
func segment(entries []entry, compactions []Compaction) []Segment {
	var out []Segment
	cur := Segment{After: None, Before: None}
	open := false
	flush := func(before int) {
		if open {
			cur.Ordinal = len(out)
			cur.Before = before
			out = append(out, cur)
		}
		cur = Segment{After: before, Before: None}
		open = false
	}
	next := 0 // index of the next compaction not yet passed
	for i := range entries {
		e := &entries[i]
		for next < len(compactions) && e.line > compactions[next].LastLine {
			flush(next)
			next++
		}
		if next < len(compactions) && e.line >= compactions[next].FirstLine {
			continue // one of the compaction's own lines
		}
		if !open {
			cur.FirstLine, cur.StartOffset = e.line, e.offset
			open = true
		}
		cur.LastLine, cur.EndOffset = e.line, e.end
		if !e.Timestamp.IsZero() {
			if cur.StartedAt.IsZero() {
				cur.StartedAt = e.Timestamp
			}
			cur.EndedAt = e.Timestamp
		}
	}
	// Close the last run. It is followed by the next compaction not yet
	// passed when the session ends on one, and by nothing otherwise.
	if next < len(compactions) {
		flush(next)
	} else {
		flush(None)
	}
	return out
}

// indexTools pairs each tool_use block with the tool_result that names it.
func indexTools(entries []entry, segments []Segment) []ToolCall {
	var calls []ToolCall
	byID := map[string]int{}
	for i := range entries {
		e := &entries[i]
		if e.Type != "assistant" && e.Type != "user" {
			continue
		}
		bs, raws := e.blocks()
		for j, b := range bs {
			switch {
			case e.Type == "assistant" && b.Type == "tool_use":
				byID[b.ID] = len(calls)
				calls = append(calls, ToolCall{
					ID: b.ID, Name: b.Name, Input: b.Input,
					Line: e.line, At: e.Timestamp, Sidechain: e.IsSidechain,
					Segment: segmentOf(segments, e.line),
				})
			case e.Type == "user" && b.Type == "tool_result":
				k, ok := byID[b.ToolUseID]
				if !ok {
					continue
				}
				c := &calls[k]
				c.HasResult, c.IsError = true, b.IsError
				c.ResultLine, c.ResultOffset, c.ResultBytes = e.line, e.offset, len(raws[j])
			}
		}
	}
	return calls
}

func segmentOf(segments []Segment, line int) int {
	for _, g := range segments {
		if line >= g.FirstLine && line <= g.LastLine {
			return g.Ordinal
		}
	}
	return None
}
