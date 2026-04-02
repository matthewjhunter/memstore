package httpapi

import (
	"net/http"
	"strconv"

	"github.com/matthewjhunter/memstore"
)

// handleStoreHint stores a context hint produced by the Ollama pipeline.
func (h *Handler) handleStoreHint(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "session store not configured")
		return
	}
	var input struct {
		SessionID       string             `json:"session_id"`
		CWD             string             `json:"cwd"`
		TurnIndex       int                `json:"turn_index"`
		HintText        string             `json:"hint_text"`
		RefIDs          []string           `json:"ref_ids"`
		RetrievedIDs    []string           `json:"retrieved_ids"`
		CandidateScores map[string]float64 `json:"candidate_scores"`
		SearchQuery     string             `json:"search_query"`
		RankerVersion   string             `json:"ranker_version"`
		Relevance       float64            `json:"relevance"`
		Desirability    float64            `json:"desirability"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.SessionID == "" || input.HintText == "" {
		writeError(w, http.StatusBadRequest, "session_id and hint_text are required")
		return
	}
	if input.TurnIndex < 0 {
		writeError(w, http.StatusBadRequest, "turn_index must be >= 0")
		return
	}
	hint := memstore.ContextHint{
		SessionID:       input.SessionID,
		CWD:             input.CWD,
		TurnIndex:       input.TurnIndex,
		HintText:        input.HintText,
		RefIDs:          input.RefIDs,
		RetrievedIDs:    input.RetrievedIDs,
		CandidateScores: input.CandidateScores,
		SearchQuery:     input.SearchQuery,
		RankerVersion:   input.RankerVersion,
		Relevance:       input.Relevance,
		Desirability:    input.Desirability,
	}
	id, err := h.sessionStore.StoreHint(r.Context(), hint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// handleGetHints returns unconsumed context hints matching ?session_id= or ?cwd= (or both).
// At least one parameter is required; when both are present, results from either are returned.
func (h *Handler) handleGetHints(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeJSON(w, http.StatusOK, []memstore.ContextHint{})
		return
	}
	q := r.URL.Query()
	sessionID := q.Get("session_id")
	cwd := q.Get("cwd")
	if sessionID == "" && cwd == "" {
		writeError(w, http.StatusBadRequest, "session_id or cwd is required")
		return
	}
	hints, err := h.sessionStore.GetPendingHints(r.Context(), sessionID, cwd)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hints == nil {
		hints = []memstore.ContextHint{}
	}
	writeJSON(w, http.StatusOK, hints)
}

// handleConsumeHint marks a context hint as consumed.
func (h *Handler) handleConsumeHint(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "session store not configured")
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id: "+idStr)
		return
	}
	if err := h.sessionStore.MarkHintConsumed(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "consumed"})
}

// handleRecordInjection records that a ref was injected into a session (dedup log).
func (h *Handler) handleRecordInjection(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "session store not configured")
		return
	}
	var input struct {
		SessionID string `json:"session_id"`
		RefID     string `json:"ref_id"`
		RefType   string `json:"ref_type"`
		Rank      int    `json:"rank"` // 0-based position in candidate list; -1 if unknown
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.SessionID == "" || input.RefID == "" || input.RefType == "" {
		writeError(w, http.StatusBadRequest, "session_id, ref_id, and ref_type are required")
		return
	}
	if err := h.sessionStore.RecordInjection(r.Context(), input.SessionID, input.RefID, input.RefType, input.Rank); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// handleRecordFeedback stores Claude's rating of an injected context item.
func (h *Handler) handleRecordFeedback(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "session store not configured")
		return
	}
	var input struct {
		RefID     string `json:"ref_id"`
		RefType   string `json:"ref_type"`
		SessionID string `json:"session_id"`
		Score     int    `json:"score"`
		Reason    string `json:"reason"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.RefID == "" || input.RefType == "" || input.SessionID == "" {
		writeError(w, http.StatusBadRequest, "ref_id, ref_type, and session_id are required")
		return
	}
	if input.Score != 1 && input.Score != -1 {
		writeError(w, http.StatusBadRequest, "score must be 1 or -1")
		return
	}
	fb := memstore.ContextFeedback{
		RefID:     input.RefID,
		RefType:   input.RefType,
		SessionID: input.SessionID,
		Score:     input.Score,
		Reason:    input.Reason,
	}
	if err := h.sessionStore.RecordFeedback(r.Context(), fb); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// handleBackfillFeedback runs the backfill-feedback pipeline, auto-rating all
// historical sessions that have unrated fact injections.
func (h *Handler) handleBackfillFeedback(w http.ResponseWriter, r *http.Request) {
	if h.extractQueue == nil {
		writeError(w, http.StatusServiceUnavailable, "extract queue not configured")
		return
	}
	result, err := h.extractQueue.BackfillFeedback(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
