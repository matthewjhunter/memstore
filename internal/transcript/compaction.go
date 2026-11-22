package transcript

import (
	"encoding/json"
	"sort"
	"strings"
)

// A detector recognises one way a transcript records a compaction. The set is
// fixed at compile time: supporting another format is writing another
// detector and adding it to detectors, not configuration. Each compaction
// carries the name of the detector that found it, so a stored row says how it
// was recognised.
//
// claimed holds the lines earlier detectors have already taken; a detector
// must skip them and add the lines of every compaction it returns.
type detector interface {
	name() string
	detect(entries []entry, claimed map[int]bool) []Compaction
}

// detectors run in order. Earlier ones are the more specific: a format with an
// explicit marker is read by its marker, and the fallbacks only see what no
// marker claimed.
var detectors = []detector{
	claudeCodeMarker{},
	continuationPrefix{},
}

func detectCompactions(entries []entry) []Compaction {
	claimed := map[int]bool{}
	var out []Compaction
	for _, d := range detectors {
		found := d.detect(entries, claimed)
		for i := range found {
			found[i].Format = d.name()
		}
		out = append(out, found...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstLine < out[j].FirstLine })
	return out
}

// claudeCodeMarker reads Claude Code's explicit form: a system entry with
// subtype compact_boundary, carrying compactMetadata, and a user entry flagged
// isCompactSummary whose parentUuid is the boundary's uuid. The two are
// written within a millisecond of each other and appear in either order. A
// flagged summary with no boundary entry is still a compaction.
type claudeCodeMarker struct{}

func (claudeCodeMarker) name() string { return "claude-code-marker" }

func (claudeCodeMarker) detect(entries []entry, claimed map[int]bool) []Compaction {
	boundaries := map[string]*entry{} // by uuid
	for i := range entries {
		if e := &entries[i]; e.Type == "system" && e.Subtype == "compact_boundary" {
			boundaries[e.UUID] = e
		}
	}
	var out []Compaction
	for i := range entries {
		e := &entries[i]
		if !e.IsCompactSummary || claimed[e.line] {
			continue
		}
		c := Compaction{
			Summary: e.text(), SummaryUUID: e.UUID, At: e.Timestamp,
			FirstLine: e.line, LastLine: e.line,
		}
		if b, ok := boundaries[e.ParentUUID]; ok && e.ParentUUID != "" && !claimed[b.line] {
			c.FirstLine, c.LastLine = min(b.line, e.line), max(b.line, e.line)
			var meta struct {
				Trigger   string `json:"trigger"`
				PreTokens int    `json:"preTokens"`
			}
			_ = json.Unmarshal(b.CompactMetadata, &meta)
			c.Trigger, c.PreTokens = meta.Trigger, meta.PreTokens
			claimed[b.line] = true
		}
		claimed[e.line] = true
		out = append(out, c)
	}
	return out
}

// continuationSentence opens every compaction summary Claude Code writes.
const continuationSentence = "This session is being continued from a previous conversation"

// continuationPrefix recognises a summary by how it begins. It reads
// transcripts from before the markers existed, and text-only archives, where
// a summary is an ordinary user message.
type continuationPrefix struct{}

func (continuationPrefix) name() string { return "continuation-prefix" }

func (continuationPrefix) detect(entries []entry, claimed map[int]bool) []Compaction {
	var out []Compaction
	for i := range entries {
		e := &entries[i]
		if e.Type != "user" || claimed[e.line] {
			continue
		}
		text := e.text()
		if !strings.HasPrefix(strings.TrimSpace(text), continuationSentence) {
			continue
		}
		claimed[e.line] = true
		out = append(out, Compaction{
			Summary: text, SummaryUUID: e.UUID, At: e.Timestamp,
			FirstLine: e.line, LastLine: e.line,
		})
	}
	return out
}
