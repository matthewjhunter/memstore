package transcript_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore/internal/transcript"
)

// line marshals one transcript entry. Tests build transcripts from these so
// the shape under test is visible in the test and no recorded session has to
// live in the repository.
func line(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func userText(t *testing.T, uuid, ts, text string) string {
	return line(t, map[string]any{
		"type": "user", "uuid": uuid, "timestamp": ts, "sessionId": "s-1", "cwd": "/work/repo",
		"message": map[string]any{"role": "user", "content": text},
	})
}

func assistantText(t *testing.T, uuid, ts, text string) string {
	return line(t, map[string]any{
		"type": "assistant", "uuid": uuid, "timestamp": ts, "sessionId": "s-1",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}},
	})
}

func boundary(t *testing.T, uuid, ts string) string {
	return line(t, map[string]any{
		"type": "system", "subtype": "compact_boundary", "uuid": uuid, "timestamp": ts, "sessionId": "s-1",
		"compactMetadata": map[string]any{"trigger": "auto", "preTokens": 246811},
	})
}

func summary(t *testing.T, uuid, parent, ts, text string) string {
	return line(t, map[string]any{
		"type": "user", "uuid": uuid, "parentUuid": parent, "timestamp": ts, "sessionId": "s-1",
		"isCompactSummary": true,
		"message":          map[string]any{"role": "user", "content": text},
	})
}

func parse(t *testing.T, lines ...string) *transcript.Session {
	t.Helper()
	s, err := transcript.Parse(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

const continuation = "This session is being continued from a previous conversation that ran out of context. "

func TestSessionMetadata(t *testing.T) {
	s := parse(t,
		line(t, map[string]any{"type": "ai-title", "aiTitle": "first guess"}),
		userText(t, "u1", "2026-11-01T10:00:00Z", "look at the hook logs"),
		assistantText(t, "a1", "2026-11-01T10:00:05Z", "looking"),
		line(t, map[string]any{"type": "ai-title", "aiTitle": "Review the hook logs"}),
		assistantText(t, "a2", "2026-11-01T10:07:00Z", "done"),
	)
	if s.ID != "s-1" || s.CWD != "/work/repo" {
		t.Errorf("id/cwd = %q / %q", s.ID, s.CWD)
	}
	if s.Title != "Review the hook logs" {
		t.Errorf("title = %q, want the last ai-title", s.Title)
	}
	if !s.StartedAt.Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)) || !s.EndedAt.Equal(time.Date(2026, 11, 1, 10, 7, 0, 0, time.UTC)) {
		t.Errorf("span = %v .. %v", s.StartedAt, s.EndedAt)
	}
	if s.Lines != 5 {
		t.Errorf("lines = %d, want 5", s.Lines)
	}
}

// An uncompacted session is one segment covering all of it, linked to no
// compaction on either side.
func TestUncompactedSessionIsOneSegment(t *testing.T) {
	s := parse(t,
		userText(t, "u1", "2026-11-01T10:00:00Z", "hello there"),
		assistantText(t, "a1", "2026-11-01T10:00:05Z", "hi"),
	)
	if len(s.Compactions) != 0 {
		t.Fatalf("compactions = %d, want 0", len(s.Compactions))
	}
	if len(s.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(s.Segments))
	}
	seg := s.Segments[0]
	if seg.FirstLine != 1 || seg.LastLine != 2 || seg.After != -1 || seg.Before != -1 {
		t.Errorf("segment = %+v", seg)
	}
}

// Two compactions make three segments. Each segment names the compaction it
// started from and the one that followed; the compaction's own two lines
// belong to neither neighbour.
func TestCompactionsSplitTheSessionIntoLinkedSegments(t *testing.T) {
	s := parse(t,
		userText(t, "u1", "2026-11-01T10:00:00Z", "start the work"),                    // 1
		assistantText(t, "a1", "2026-11-01T10:01:00Z", "working"),                      // 2
		boundary(t, "b1", "2026-11-01T11:00:00Z"),                                      // 3
		summary(t, "c1", "b1", "2026-11-01T11:00:00Z", continuation+"First summary."),  // 4
		userText(t, "u2", "2026-11-01T11:01:00Z", "carry on"),                          // 5
		boundary(t, "b2", "2026-11-01T12:00:00Z"),                                      // 6
		summary(t, "c2", "b2", "2026-11-01T12:00:00Z", continuation+"Second summary."), // 7
		assistantText(t, "a3", "2026-11-01T12:01:00Z", "finished"),                     // 8
	)
	if len(s.Compactions) != 2 {
		t.Fatalf("compactions = %d, want 2", len(s.Compactions))
	}
	c := s.Compactions[0]
	if c.Format != "claude-code-marker" || c.Trigger != "auto" || c.PreTokens != 246811 {
		t.Errorf("compaction 0 = %+v", c)
	}
	if !strings.HasSuffix(c.Summary, "First summary.") || c.SummaryUUID != "c1" {
		t.Errorf("compaction 0 summary = %q (uuid %q)", c.Summary, c.SummaryUUID)
	}
	if c.FirstLine != 3 || c.LastLine != 4 {
		t.Errorf("compaction 0 lines = %d..%d, want 3..4", c.FirstLine, c.LastLine)
	}

	type span struct{ first, last, after, before int }
	want := []span{{1, 2, -1, 0}, {5, 5, 0, 1}, {8, 8, 1, -1}}
	if len(s.Segments) != len(want) {
		t.Fatalf("segments = %d, want %d", len(s.Segments), len(want))
	}
	for i, w := range want {
		g := s.Segments[i]
		if g.Ordinal != i || g.FirstLine != w.first || g.LastLine != w.last || g.After != w.after || g.Before != w.before {
			t.Errorf("segment %d = %+v, want lines %d..%d after %d before %d", i, g, w.first, w.last, w.after, w.before)
		}
	}
}

// The summary is written a millisecond before its boundary in some files, so
// either order has to read as one compaction, not two.
func TestSummaryBeforeItsBoundaryIsOneCompaction(t *testing.T) {
	s := parse(t,
		userText(t, "u1", "2026-11-01T10:00:00Z", "start"),
		summary(t, "c1", "b1", "2026-11-01T11:00:00Z", continuation+"Summary."),
		boundary(t, "b1", "2026-11-01T11:00:00Z"),
		userText(t, "u2", "2026-11-01T11:01:00Z", "carry on"),
	)
	if len(s.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(s.Compactions))
	}
	if c := s.Compactions[0]; c.FirstLine != 2 || c.LastLine != 3 || c.Trigger != "auto" {
		t.Errorf("compaction = %+v", c)
	}
	if len(s.Segments) != 2 || s.Segments[1].FirstLine != 4 {
		t.Errorf("segments = %+v", s.Segments)
	}
}

// Older transcripts, and the text-only archive, carry no markers: the summary
// is an ordinary user message recognisable only by how it begins.
func TestContinuationPrefixIsACompactionWithoutMarkers(t *testing.T) {
	s := parse(t,
		userText(t, "u1", "2026-11-01T10:00:00Z", "start"),
		userText(t, "c1", "2026-11-01T11:00:00Z", continuation+"Summary."),
		userText(t, "u2", "2026-11-01T11:01:00Z", "carry on"),
	)
	if len(s.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(s.Compactions))
	}
	if c := s.Compactions[0]; c.Format != "continuation-prefix" || c.FirstLine != 2 || c.LastLine != 2 {
		t.Errorf("compaction = %+v", c)
	}
}

// A marked summary also begins with the continuation sentence. It is one
// compaction, credited to the detector that saw the marker.
func TestAMarkedSummaryIsNotCountedAgainByItsPrefix(t *testing.T) {
	s := parse(t,
		boundary(t, "b1", "2026-11-01T11:00:00Z"),
		summary(t, "c1", "b1", "2026-11-01T11:00:00Z", continuation+"Summary."),
	)
	if len(s.Compactions) != 1 || s.Compactions[0].Format != "claude-code-marker" {
		t.Fatalf("compactions = %+v", s.Compactions)
	}
}

// A session that ends on a compaction has no segment after it: an empty
// segment would be a row describing nothing.
func TestNoEmptySegmentAfterATrailingCompaction(t *testing.T) {
	s := parse(t,
		userText(t, "u1", "2026-11-01T10:00:00Z", "start"),
		boundary(t, "b1", "2026-11-01T11:00:00Z"),
		summary(t, "c1", "b1", "2026-11-01T11:00:00Z", continuation+"Summary."),
	)
	if len(s.Segments) != 1 || s.Segments[0].Before != 0 {
		t.Errorf("segments = %+v", s.Segments)
	}
}

func TestToolCallsAreIndexedWithTheirResults(t *testing.T) {
	call := line(t, map[string]any{
		"type": "assistant", "uuid": "a1", "timestamp": "2026-11-01T10:00:05Z", "sessionId": "s-1",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "checking"},
			{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "git status"}},
			{"type": "tool_use", "id": "toolu_2", "name": "Read", "input": map[string]any{"file_path": "/work/repo/a.go"}},
		}},
	})
	result := line(t, map[string]any{
		"type": "user", "uuid": "u2", "timestamp": "2026-11-01T10:00:07Z", "sessionId": "s-1",
		"message": map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": "On branch main"},
			{"type": "tool_result", "tool_use_id": "toolu_2", "is_error": true, "content": "no such file"},
		}},
	})
	s := parse(t, userText(t, "u1", "2026-11-01T10:00:00Z", "check the tree"), call, result)

	if len(s.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(s.Tools))
	}
	bash, read := s.Tools[0], s.Tools[1]
	if bash.ID != "toolu_1" || bash.Name != "Bash" || bash.Line != 2 || bash.Segment != 0 {
		t.Errorf("bash = %+v", bash)
	}
	var in struct{ Command string }
	if err := json.Unmarshal(bash.Input, &in); err != nil || in.Command != "git status" {
		t.Errorf("bash input = %s (%v)", bash.Input, err)
	}
	if !bash.HasResult || bash.IsError || bash.ResultLine != 3 || bash.ResultBytes == 0 {
		t.Errorf("bash result = %+v", bash)
	}
	if !read.HasResult || !read.IsError {
		t.Errorf("read result = %+v", read)
	}
	if !bash.At.Equal(time.Date(2026, 11, 1, 10, 0, 5, 0, time.UTC)) {
		t.Errorf("bash at = %v", bash.At)
	}
}

// A call the session ended on, or one whose result was lost, is still a call.
func TestAToolCallWithNoResultIsKept(t *testing.T) {
	call := line(t, map[string]any{
		"type": "assistant", "uuid": "a1", "timestamp": "2026-11-01T10:00:05Z",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "sleep 100"}},
		}},
	})
	s := parse(t, call)
	if len(s.Tools) != 1 || s.Tools[0].HasResult {
		t.Errorf("tools = %+v", s.Tools)
	}
}

// Offsets are what let a segment or a tool result be read back out of the raw
// file without parsing the whole of it.
func TestOffsetsAddressTheRawBytes(t *testing.T) {
	lines := []string{
		userText(t, "u1", "2026-11-01T10:00:00Z", "start"),
		boundary(t, "b1", "2026-11-01T11:00:00Z"),
		summary(t, "c1", "b1", "2026-11-01T11:00:00Z", continuation+"Summary."),
		userText(t, "u2", "2026-11-01T11:01:00Z", "carry on"),
		assistantText(t, "a2", "2026-11-01T11:02:00Z", "ok"),
	}
	raw := strings.Join(lines, "\n") + "\n"
	s, err := transcript.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	seg := s.Segments[1]
	got := raw[seg.StartOffset:seg.EndOffset]
	if want := lines[3] + "\n" + lines[4] + "\n"; got != want {
		t.Errorf("segment bytes =\n%q\nwant\n%q", got, want)
	}
	if s.Bytes != int64(len(raw)) {
		t.Errorf("bytes = %d, want %d", s.Bytes, len(raw))
	}
}

// A line that is not JSON is counted and skipped. Line numbers and offsets
// after it must still be right, since they address the file as it is.
func TestABadLineDoesNotShiftWhatFollows(t *testing.T) {
	good := userText(t, "u2", "2026-11-01T11:01:00Z", "carry on")
	raw := userText(t, "u1", "2026-11-01T10:00:00Z", "start") + "\n{not json\n\n" + good + "\n"
	s, err := transcript.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.BadLines != 1 || s.Lines != 4 {
		t.Errorf("bad = %d lines = %d, want 1 and 4", s.BadLines, s.Lines)
	}
	seg := s.Segments[0]
	if seg.LastLine != 4 || !strings.HasSuffix(raw[seg.StartOffset:seg.EndOffset], good+"\n") {
		t.Errorf("segment = %+v", seg)
	}
}

// A tool result can be megabytes on one line; a line-length limit would
// truncate the transcript at the first large one.
func TestAVeryLongLineIsRead(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	s := parse(t, userText(t, "u1", "2026-11-01T10:00:00Z", big), assistantText(t, "a1", "2026-11-01T10:00:05Z", "ok"))
	if s.Lines != 2 || len(s.Segments) != 1 || s.Segments[0].LastLine != 2 {
		t.Errorf("lines = %d segments = %+v", s.Lines, s.Segments)
	}
}

// TestLocalTranscripts parses real transcripts when pointed at a directory of
// them. It checks only that every file parses and that the structure is
// consistent; it prints counts for comparison against an independent tally.
func TestLocalTranscripts(t *testing.T) {
	dir := os.Getenv("MEMSTORE_TRANSCRIPT_SAMPLES")
	if dir == "" {
		t.Skip("MEMSTORE_TRANSCRIPT_SAMPLES not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	var sessions, compacted, compactions, tools, unanswered, bad int
	formats := map[string]int{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		s, err := transcript.Parse(fh)
		fh.Close()
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		sessions++
		bad += s.BadLines
		if len(s.Compactions) > 0 {
			compacted++
		}
		compactions += len(s.Compactions)
		for _, c := range s.Compactions {
			formats[c.Format]++
			if c.Summary == "" {
				t.Errorf("%s: compaction at line %d has no summary", filepath.Base(f), c.FirstLine)
			}
		}
		tools += len(s.Tools)
		for _, tc := range s.Tools {
			if !tc.HasResult {
				unanswered++
			}
		}
		prev := 0
		for _, g := range s.Segments {
			if g.FirstLine <= prev || g.LastLine < g.FirstLine || g.EndOffset <= g.StartOffset {
				t.Errorf("%s: segment %+v out of order", filepath.Base(f), g)
			}
			prev = g.LastLine
		}
	}
	t.Logf("sessions %d, compacted %d, compactions %d %v, tool calls %d (%d with no result), bad lines %d",
		sessions, compacted, compactions, formats, tools, unanswered, bad)
}
