package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/matthewjhunter/memstore"
)

// maxClaimRefs bounds one claim. The file-trigger and startup lists run to
// tens of refs; the cap only keeps a request from being unbounded.
const maxClaimRefs = 500

// handleClaimInjections records the refs one channel showed a session and
// returns those new to it: step 3 of docs/citation-feedback.md. The
// file-trigger channel shows each fact in full, so its new facts are also
// marked seen and recall does not repeat them. The startup list shows task
// titles only and marks nothing.
func (h *Handler) handleClaimInjections(w http.ResponseWriter, r *http.Request) {
	if h.sessionStore == nil {
		writeError(w, http.StatusServiceUnavailable, "session store not configured")
		return
	}
	var input struct {
		SessionID string   `json:"session_id"`
		Channel   string   `json:"channel"`
		RefType   string   `json:"ref_type"`
		RefIDs    []string `json:"ref_ids"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	switch {
	case input.SessionID == "" || input.RefType == "":
		writeError(w, http.StatusBadRequest, "session_id and ref_type are required")
		return
	case !memstore.ValidChannel(input.Channel):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown channel %q", input.Channel))
		return
	case len(input.RefIDs) > maxClaimRefs:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d ref_ids per claim", maxClaimRefs))
		return
	}
	if slices.Contains(input.RefIDs, "") {
		writeError(w, http.StatusBadRequest, "ref_ids must not contain an empty id")
		return
	}

	claimer, ok := sessionFromCtx(r.Context(), h.sessionStore).(memstore.InjectionClaimer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "session store does not record claims")
		return
	}
	fresh, err := claimer.ClaimInjections(r.Context(), input.SessionID, input.Channel, input.RefType, input.RefIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fresh == nil {
		fresh = []string{}
	}

	if h.sessionCtx != nil && input.Channel == memstore.ChannelFileTrigger && input.RefType == memstore.RefTypeFact {
		ids := make([]int64, 0, len(fresh))
		for _, s := range fresh {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		h.sessionCtx.MarkSeen(input.SessionID, ids)
	}
	writeJSON(w, http.StatusOK, map[string][]string{"new": fresh})
}
