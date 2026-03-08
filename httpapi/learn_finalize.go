package httpapi

import (
	"net/http"

	"github.com/matthewjhunter/memstore"
)

func (h *Handler) handleLearnFinalize(w http.ResponseWriter, r *http.Request) {
	if h.learnSessions == nil {
		writeError(w, http.StatusServiceUnavailable, "learn sessions not enabled")
		return
	}
	var input memstore.LearnFinalizeOpts
	if !readJSON(r, w, &input) {
		return
	}
	if input.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	sess := h.learnSessions.Consume(input.SessionID)
	if sess == nil {
		writeJSON(w, http.StatusOK, &memstore.LearnFinalizeResult{})
		return
	}

	// Convert session refs to core type.
	refs := make([]memstore.LearnedFactRef, len(sess.facts))
	for i, f := range sess.facts {
		refs[i] = memstore.LearnedFactRef{
			FactID:  f.FactID,
			Surface: f.Surface,
			RelPath: f.RelPath,
		}
	}

	result, err := memstore.SynthesizeSession(r.Context(), h.store, h.embedder, h.generator, refs, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}
