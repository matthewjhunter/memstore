package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/rawstore"
	"github.com/matthewjhunter/memstore/internal/transcript"
)

// defaultMaxTranscriptBytes bounds a transcript's decompressed size. The
// largest seen in a month of real sessions was 51 MB; this leaves an order of
// magnitude over that, and under what the daemon can hold in memory twice.
const defaultMaxTranscriptBytes = 512 << 20

// WithTranscriptStore enables raw transcript storage (docs/session-archive.md).
// Without it PUT /v1/sessions/{id}/raw answers 501 and clients fall back to
// the text upload.
func WithTranscriptStore(rs *rawstore.Store) HandlerOpt {
	return func(h *Handler) { h.rawStore = rs }
}

// WithMaxTranscriptBytes caps the decompressed size of an uploaded transcript.
func WithMaxTranscriptBytes(n int64) HandlerOpt {
	return func(h *Handler) { h.maxTranscriptBytes = n }
}

// sessionArchiver is the part of a session store that records a stored
// transcript. It is asked for by type assertion, like the other optional
// session-store capabilities: a store that cannot say whose file it is
// keeping does not take one.
type sessionArchiver interface {
	UserID() int64
	SaveArchive(ctx context.Context, a memstore.SessionArchive, parsed *transcript.Session) (int64, error)
}

// handleSessionRaw takes a session's whole raw transcript, gzip-compressed, as
// the request body. It stores those bytes as they arrived, records what the
// transcript holds -- its compactions' positions, segments and tool calls --
// and then does everything the text upload does, from the same bytes.
//
// Everything derived is derived here, while the transcript is in hand.
// Nothing later needs the daemon to open the stored file on its own, which is
// what leaves room for the file to be encrypted to a key the daemon does not
// hold.
func (h *Handler) handleSessionRaw(w http.ResponseWriter, r *http.Request) {
	archiver, ok := sessionFromCtx(r.Context(), h.sessionStore).(sessionArchiver)
	if h.rawStore == nil || !ok || archiver.UserID() <= 0 {
		writeError(w, http.StatusNotImplemented, "raw transcript storage is not configured")
		return
	}
	sessionID := r.PathValue("id")
	if !rawstore.ValidSessionID(sessionID) {
		writeError(w, http.StatusBadRequest, "session id must be a UUID")
		return
	}

	stored, err := io.ReadAll(r.Body)
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}
	limit := h.maxTranscriptBytes
	if limit <= 0 {
		limit = defaultMaxTranscriptBytes
	}
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body is not gzip: "+err.Error())
		return
	}
	// One byte past the limit is enough to know the limit was passed, and is
	// all a decompression bomb gets to expand to.
	raw, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid gzip: "+err.Error())
		return
	}
	if int64(len(raw)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "transcript too large")
		return
	}

	parsed, err := transcript.Parse(bytes.NewReader(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, "parsing transcript: "+err.Error())
		return
	}
	path, err := h.rawStore.Put(archiver.UserID(), sessionID, stored)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(raw)
	if _, err := archiver.SaveArchive(r.Context(), memstore.SessionArchive{
		SessionID:   sessionID,
		Path:        path,
		StoredBytes: int64(len(stored)),
		RawBytes:    int64(len(raw)),
		SHA256:      hex.EncodeToString(sum[:]),
	}, parsed); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		cwd = parsed.CWD
	}
	// Before the turns are queued for extraction: a failure here fails the
	// upload, and the client's retry must not queue the session twice.
	if err := h.recordSessionFacts(r.Context(), sessionID, cwd, parsed); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	turns := parseJSONLTurns(sessionID, cwd, string(raw))
	if err := h.ingestTurns(r.Context(), sessionID, cwd, r.URL.Query().Get("persona"), turns); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":      "accepted",
		"turns":       len(turns),
		"compactions": len(parsed.Compactions),
		"segments":    len(parsed.Segments),
		"tool_calls":  len(parsed.Tools),
	})
}
