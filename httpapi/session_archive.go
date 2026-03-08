package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
)

// handleSessionHook accepts a raw Claude Code Stop hook payload and persists it.
func (h *Handler) handleSessionHook(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if !readJSON(r, w, &raw) {
		return
	}
	if h.sessionStore != nil {
		if err := h.sessionStore.SaveHook(r.Context(), []byte(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleSessionTranscript accepts a JSONL transcript, parses it into turns,
// and upserts them into the session_turns table.
func (h *Handler) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
		Content   string `json:"content"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	turns := parseJSONLTurns(input.SessionID, input.CWD, input.Content)
	if h.sessionStore != nil && len(turns) > 0 {
		if err := h.sessionStore.SaveTurns(r.Context(), input.SessionID, turns); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if h.extractQueue != nil {
			h.extractQueue.Enqueue(extractJob{
				SessionID: input.SessionID,
				CWD:       input.CWD,
				Turns:     turns,
			})
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "accepted",
		"turns":  len(turns),
	})
}

// parseJSONLTurns extracts user and assistant text turns from a Claude Code
// JSONL transcript. Tool calls, thinking blocks, progress events, and
// file-history snapshots are skipped.
func parseJSONLTurns(sessionID, cwd, content string) []memstore.SessionTurn {
	var turns []memstore.SessionTurn
	idx := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw struct {
			Type      string    `json:"type"`
			UUID      string    `json:"uuid"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		if raw.Type != "user" && raw.Type != "assistant" {
			continue
		}

		text := extractText(raw.Type, raw.Message.Content)
		if text == "" {
			continue
		}

		turns = append(turns, memstore.SessionTurn{
			SessionID: sessionID,
			UUID:      raw.UUID,
			TurnIndex: idx,
			Role:      raw.Message.Role,
			Content:   text,
			CWD:       cwd,
			CreatedAt: raw.Timestamp,
		})
		idx++
	}
	return turns
}

// extractText pulls displayable text from a message content field.
// User messages: content is a plain JSON string.
// Assistant messages: content is an array; we join all type="text" items.
func extractText(msgType string, raw json.RawMessage) string {
	if msgType == "user" {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return strings.TrimSpace(s)
		}
	}
	// Array form — collect text items (assistant responses, and user messages
	// that contain text blocks rather than plain strings).
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var parts []string
	for _, item := range items {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
