package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/transcript"
)

// Kinds and link type of the facts an archived session leaves behind
// (docs/session-archive.md). The session fact is the parent; it links to one
// compaction fact per compaction, and those hold the summaries themselves.
const (
	kindSession    = "session"
	kindCompaction = "compaction"
	linkCompaction = "compaction"
	linkSummary    = "summary" // session fact -> the generated summary of the session

	sourceSessionArchive = "session_archive"
)

// recallNever is the value of a fact's "recall" metadata field that keeps it
// out of prompt recall. It is data on the fact, so anything may carry it;
// recall does not hold a list of kinds to skip. Search is unaffected.
//
// Archive facts carry it because a compaction summary is long and touches
// everything its session did: it would match almost any prompt from the same
// project and crowd out the facts recall exists to surface.
const (
	metaRecall  = "recall"
	recallNever = "never"
)

// recallExcluded reports whether a fact asks to be left out of prompt recall.
func recallExcluded(f memstore.Fact) bool {
	if len(f.Metadata) == 0 {
		return false
	}
	var meta struct {
		Recall string `json:"recall"`
	}
	if json.Unmarshal(f.Metadata, &meta) != nil {
		return false
	}
	return meta.Recall == recallNever
}

// recordSessionFacts writes the session fact and a fact per compaction for
// one uploaded transcript, and links the session to each compaction.
//
// It is safe to repeat, because a resumed session uploads again. A compaction
// is stored once, keyed by the uuid of its summary. The session fact changes
// as the session grows -- a new title, a later end -- and is superseded by a
// new one when it does, which then takes the links; an upload that changes
// nothing writes nothing.
func (h *Handler) recordSessionFacts(ctx context.Context, sessionID, cwd string, parsed *transcript.Session) error {
	store := storeFromCtx(ctx, h.store)
	subject := projectNameFromCWD(cwd)
	if subject == "" {
		subject = "session"
	}

	content := renderSessionFact(sessionID, cwd, parsed)
	existing, err := store.List(ctx, memstore.QueryOpts{
		Kind: kindSession, OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{{Key: "session_id", Op: "=", Value: sessionID}},
	})
	if err != nil {
		return fmt.Errorf("session fact: %w", err)
	}
	var sessionFactID int64
	newSession := true
	if len(existing) > 0 && existing[0].Content == content {
		sessionFactID, newSession = existing[0].ID, false
	} else {
		meta, _ := json.Marshal(map[string]any{
			"session_id": sessionID, "source": sourceSessionArchive, metaRecall: recallNever,
			"cwd": cwd, "started_at": rfc3339(parsed.StartedAt), "ended_at": rfc3339(parsed.EndedAt),
		})
		sessionFactID, err = store.Insert(ctx, memstore.Fact{
			Content: content, Subject: subject, Category: "session", Kind: kindSession, Metadata: meta,
		})
		if err != nil {
			return fmt.Errorf("session fact: %w", err)
		}
		for _, old := range existing {
			if err := store.Supersede(ctx, old.ID, sessionFactID); err != nil {
				return fmt.Errorf("session fact: superseding %d: %w", old.ID, err)
			}
		}
	}

	stored, err := store.List(ctx, memstore.QueryOpts{
		Kind:            kindCompaction,
		MetadataFilters: []memstore.MetadataFilter{{Key: "session_id", Op: "=", Value: sessionID}},
	})
	if err != nil {
		return fmt.Errorf("compaction facts: %w", err)
	}
	byKey := make(map[string]int64, len(stored))
	for _, f := range stored {
		var meta struct {
			SummaryUUID string `json:"summary_uuid"`
			Ordinal     int    `json:"ordinal"`
		}
		_ = json.Unmarshal(f.Metadata, &meta)
		byKey[compactionKey(meta.SummaryUUID, meta.Ordinal)] = f.ID
	}
	for i, c := range parsed.Compactions {
		if strings.TrimSpace(c.Summary) == "" {
			continue
		}
		id, have := byKey[compactionKey(c.SummaryUUID, i)]
		if !have {
			// A fact is capped at memstore.MaxContentLength and a real
			// summary runs to several times that. The fact is the searchable
			// handle and holds what fits; the whole summary is in
			// session_compactions, and the fact says so when it was cut.
			content, cut := fitFact(c.Summary)
			m := map[string]any{
				"session_id": sessionID, "source": sourceSessionArchive, metaRecall: recallNever,
				"ordinal": i, "summary_uuid": c.SummaryUUID, "format": c.Format,
				"trigger": c.Trigger, "pre_tokens": c.PreTokens, "compacted_at": rfc3339(c.At),
			}
			if cut {
				m["truncated"] = true
				m["full_chars"] = utf8.RuneCountInString(c.Summary)
			}
			meta, _ := json.Marshal(m)
			id, err = store.Insert(ctx, memstore.Fact{
				Content: content, Subject: subject, Category: "session", Kind: kindCompaction, Metadata: meta,
			})
			if err != nil {
				return fmt.Errorf("compaction fact %d: %w", i, err)
			}
		}
		// A link is written when either end is new. Links have no uniqueness
		// of their own, so this is what keeps a repeated upload from
		// stacking duplicates.
		if newSession || !have {
			if _, err := store.LinkFacts(ctx, sessionFactID, id, linkCompaction, false,
				fmt.Sprintf("compaction %d", i+1), map[string]any{"ordinal": i}); err != nil {
				return fmt.Errorf("linking compaction %d: %w", i, err)
			}
		}
	}
	return nil
}

// fitFact returns s cut to what a fact may hold, and whether it was cut. The
// limit is in characters, as the database counts them. A cut falls at the
// last line break that fits, so the fact ends on a whole line of the summary
// and not part-way through one.
func fitFact(s string) (string, bool) {
	if utf8.RuneCountInString(s) <= memstore.MaxContentLength {
		return s, false
	}
	end, n := 0, 0
	for i := range s {
		if n == memstore.MaxContentLength {
			end = i
			break
		}
		n++
	}
	head := s[:end]
	if nl := strings.LastIndexByte(head, '\n'); nl > len(head)/2 {
		head = head[:nl]
	}
	return head, true
}

// compactionKey identifies a compaction within its session: by its summary's
// uuid, which survives the session growing, or by position for a format that
// gives it none.
func compactionKey(summaryUUID string, ordinal int) string {
	if summaryUUID != "" {
		return "uuid:" + summaryUUID
	}
	return fmt.Sprintf("ordinal:%d", ordinal)
}

// renderSessionFact is the session fact's text: the title, then what is known
// about the session without reading it. It holds nothing that changes unless
// the session does, so an unchanged upload renders the same text.
func renderSessionFact(sessionID, cwd string, parsed *transcript.Session) string {
	title := strings.TrimSpace(parsed.Title)
	if title == "" {
		title = "Untitled session"
	}
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "- Session %s\n", sessionID)
	if !parsed.StartedAt.IsZero() {
		fmt.Fprintf(&b, "- %s to %s\n", rfc3339(parsed.StartedAt), rfc3339(parsed.EndedAt))
	}
	if cwd != "" {
		fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
	}
	fmt.Fprintf(&b, "- %s, %s, %s\n",
		plural(len(parsed.Compactions), "compaction"), plural(len(parsed.Segments), "segment"), plural(len(parsed.Tools), "tool call"))
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
