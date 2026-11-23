package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/transcript"
)

// archiveBackfillSource is the part of a session store the backfill reads:
// which sessions have text and no archive, and their turns.
type archiveBackfillSource interface {
	UnarchivedSessions(ctx context.Context) ([]string, error)
	GetSessionTurns(ctx context.Context, sessionID string) ([]memstore.SessionTurn, error)
}

type archiveBackfillResult struct {
	Sessions    int  `json:"sessions"`    // text-only sessions read
	Compacted   int  `json:"compacted"`   // of those, how many hold a compaction summary
	Compactions int  `json:"compactions"` // compaction summaries found
	Written     int  `json:"written"`     // sessions recorded (0 on a dry run)
	Errors      int  `json:"errors"`
	DryRun      bool `json:"dry_run"`
}

// handleSessionArchiveBackfill gives sessions uploaded as text, before raw
// transcripts were kept, the same session and compaction records a raw upload
// leaves. A compaction summary survives in the text archive as an ordinary
// user turn, and the continuation-prefix detector recognises it there.
//
// What it cannot give them is a file: the archive row has no path, the
// segments are ranges of turns, not of bytes, and there are no tool calls.
// With {"dry_run": true} it counts and writes nothing. It is safe to repeat;
// a session it has recorded is no longer unarchived.
func (h *Handler) handleSessionArchiveBackfill(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DryRun bool `json:"dry_run"`
	}
	if r.ContentLength != 0 && !readJSON(r, w, &input) {
		return
	}
	ctx := r.Context()
	ss := sessionFromCtx(ctx, h.sessionStore)
	src, okSrc := ss.(archiveBackfillSource)
	archiver, okArch := ss.(sessionArchiver)
	if h.sessionStore == nil || !okSrc || !okArch || archiver.UserID() <= 0 {
		writeError(w, http.StatusNotImplemented, "session store does not keep archives")
		return
	}
	ids, err := src.UnarchivedSessions(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	res := archiveBackfillResult{DryRun: input.DryRun}
	for _, id := range ids {
		turns, err := src.GetSessionTurns(ctx, id)
		if err != nil || len(turns) == 0 {
			if err != nil {
				h.log().Error("archive backfill: reading turns failed", "session", id, "err", err)
				res.Errors++
			}
			continue
		}
		parsed, err := transcriptFromTurns(turns)
		if err != nil {
			h.log().Error("archive backfill: deriving failed", "session", id, "err", err)
			res.Errors++
			continue
		}
		res.Sessions++
		if n := len(parsed.Compactions); n > 0 {
			res.Compacted++
			res.Compactions += n
		}
		if input.DryRun {
			continue
		}
		cwd := turns[0].CWD
		summaries := h.sessionSummaries(ctx, id)
		if len(summaries) > 0 {
			parsed.Title = firstLine(summaries[len(summaries)-1].Content)
		}
		if _, err := archiver.SaveArchive(ctx, memstore.SessionArchive{SessionID: id}, parsed); err != nil {
			h.log().Error("archive backfill: saving the archive failed", "session", id, "err", err)
			res.Errors++
			continue
		}
		if err := h.recordSessionFacts(ctx, id, cwd, parsed); err != nil {
			h.log().Error("archive backfill: recording facts failed", "session", id, "err", err)
			res.Errors++
			continue
		}
		h.linkSummaries(ctx, id, summaries)
		res.Written++
	}
	writeJSON(w, http.StatusOK, res)
}

// transcriptFromTurns derives a session from its archived text turns by
// writing them out as transcript lines and reading those. Line numbers in the
// result are positions in the turn list. Byte offsets would address a file
// that does not exist, so they are cleared.
func transcriptFromTurns(turns []memstore.SessionTurn) (*transcript.Session, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, t := range turns {
		line := map[string]any{
			"type": t.Role, "uuid": t.UUID, "sessionId": t.SessionID, "cwd": t.CWD,
			"message": map[string]any{"role": t.Role, "content": t.Content},
		}
		if !t.CreatedAt.IsZero() {
			line["timestamp"] = t.CreatedAt
		}
		if err := enc.Encode(line); err != nil {
			return nil, err
		}
	}
	parsed, err := transcript.Parse(&buf)
	if err != nil {
		return nil, err
	}
	parsed.Bytes = 0
	for i := range parsed.Segments {
		parsed.Segments[i].StartOffset, parsed.Segments[i].EndOffset = 0, 0
	}
	return parsed, nil
}

// sessionSummaries returns the summaries already generated for a session,
// oldest first.
func (h *Handler) sessionSummaries(ctx context.Context, sessionID string) []memstore.Fact {
	facts, err := storeFromCtx(ctx, h.store).List(ctx, memstore.QueryOpts{
		Kind: "summary", OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{{Key: "session_id", Op: "=", Value: sessionID}},
	})
	if err != nil {
		return nil
	}
	return facts
}

// linkSummaries points the session's current session fact at each summary.
func (h *Handler) linkSummaries(ctx context.Context, sessionID string, summaries []memstore.Fact) {
	if len(summaries) == 0 {
		return
	}
	store := storeFromCtx(ctx, h.store)
	sessions, err := store.List(ctx, memstore.QueryOpts{
		Kind: kindSession, OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{{Key: "session_id", Op: "=", Value: sessionID}},
	})
	if err != nil || len(sessions) == 0 {
		return
	}
	for _, s := range summaries {
		if _, err := store.LinkFacts(ctx, sessions[0].ID, s.ID, linkSummary, false, "session summary", nil); err != nil {
			h.log().Warn("archive backfill: linking a summary failed", "session", sessionID, "err", err)
		}
	}
}

// firstLine is the first non-empty line of s, which for a session summary is
// its one-sentence lead.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
