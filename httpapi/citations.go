package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strconv"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
)

// Reading [fact N] citations out of session transcripts: step 2 of
// docs/citation-feedback.md. The model writes the signal into its answers and
// the transcript reaches the daemon on upload. It is read in the daemon, which
// already holds the transcript, so no client has to remember to report
// anything; #158 took the same approach for injection counts.

var (
	citationRE = regexp.MustCompile(mcpserver.CitationPattern)
	// Code is where the form gets quoted rather than used: the convention's own
	// tests and examples, discussed in a session. A genuine citation is written
	// in prose, never in backticks.
	fencedCodeRE = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRE = regexp.MustCompile("`[^`\n]*`")
)

// extractCitations returns the citations in the assistant turns, with code
// blocks and inline code removed first, one per session and fact at its first
// appearance. User turns do not count: typing the form is not evidence that a
// memory shaped an answer.
func extractCitations(turns []memstore.SessionTurn) []memstore.FactCitation {
	var out []memstore.FactCitation
	seen := make(map[string]bool)
	for _, t := range turns {
		if t.Role != "assistant" {
			continue
		}
		text := inlineCodeRE.ReplaceAllString(fencedCodeRE.ReplaceAllString(t.Content, " "), " ")
		for _, m := range citationRE.FindAllStringSubmatch(text, -1) {
			id, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			key := t.SessionID + "\x00" + m[1]
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, memstore.FactCitation{
				SessionID: t.SessionID, FactID: id, TurnUUID: t.UUID, CitedAt: t.CreatedAt,
			})
		}
	}
	return out
}

// citable keeps the citations whose id resolves to an active fact the caller
// can see (decision D2), and counts the rest. Exposure is not required: MCP
// results cannot be tied to a Claude Code session, so for a whole channel it
// is unverifiable. store is the request-scoped store, so Get applies the
// caller's user and readability filters.
func citable(ctx context.Context, store memstore.Store, cites []memstore.FactCitation) (kept []memstore.FactCitation, rejected int) {
	for _, c := range cites {
		f, err := store.Get(ctx, c.FactID)
		if err != nil || f == nil || f.SupersededBy != nil {
			rejected++
			continue
		}
		kept = append(kept, c)
	}
	return kept, rejected
}

// recordCitations reads and records the citations in an uploaded transcript.
// Best-effort: a transcript upload must never fail over the citation log.
func (h *Handler) recordCitations(ctx context.Context, sessionID string, turns []memstore.SessionTurn) {
	rec, ok := sessionFromCtx(ctx, h.sessionStore).(memstore.CitationRecorder)
	if !ok {
		return
	}
	cites := extractCitations(turns)
	if len(cites) == 0 {
		return
	}
	kept, _ := citable(ctx, storeFromCtx(ctx, h.store), cites)
	if len(kept) == 0 {
		return
	}
	if _, err := rec.RecordCitations(ctx, kept); err != nil {
		h.log().Error("citations: recording failed", "session", sessionID, "err", err)
	}
}

// citationBackfillResult is the response of POST /v1/citations/backfill.
type citationBackfillResult struct {
	Sessions int `json:"sessions"` // sessions read
	Found    int `json:"found"`    // citations outside code, one per session and fact
	Recorded int `json:"recorded"` // new rows written
	Rejected int `json:"rejected"` // ids with no active fact the caller can see
}

// handleCitationBackfill reads the citations already in the caller's recorded
// sessions -- the history from before transcripts were read on upload -- with
// the same rules as the live path. Idempotent: a second run records nothing
// new.
func (h *Handler) handleCitationBackfill(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "session store not configured")
		return
	}
	ctx := r.Context()
	ss := sessionFromCtx(ctx, h.sessionStore)
	src, okSrc := ss.(memstore.CitationSource)
	rec, okRec := ss.(memstore.CitationRecorder)
	if !okSrc || !okRec {
		writeError(w, http.StatusNotImplemented, "session store does not keep citations")
		return
	}
	ids, err := src.CitingSessions(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	store := storeFromCtx(ctx, h.store)
	var res citationBackfillResult
	for _, id := range ids {
		turns, err := src.GetSessionTurns(ctx, id)
		if err != nil {
			h.log().Error("citation backfill failed", "session", id, "err", err)
			continue
		}
		res.Sessions++
		cites := extractCitations(turns)
		res.Found += len(cites)
		kept, rejected := citable(ctx, store, cites)
		res.Rejected += rejected
		if len(kept) == 0 {
			continue
		}
		n, err := rec.RecordCitations(ctx, kept)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		res.Recorded += n
	}
	writeJSON(w, http.StatusOK, res)
}
