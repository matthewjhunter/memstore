package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// citingSessionStore is a session store that records citations and serves
// stored turns for the backfill.
type citingSessionStore struct {
	mockSessionStore
	recorded []memstore.FactCitation
	turns    map[string][]memstore.SessionTurn
}

func (s *citingSessionStore) RecordCitations(_ context.Context, c []memstore.FactCitation) (int, error) {
	s.recorded = append(s.recorded, c...)
	return len(c), nil
}

func (s *citingSessionStore) CitingSessions(context.Context) ([]string, error) {
	ids := make([]string, 0, len(s.turns))
	for id := range s.turns {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *citingSessionStore) GetSessionTurns(_ context.Context, sessionID string) ([]memstore.SessionTurn, error) {
	return s.turns[sessionID], nil
}

// assistantLine is one assistant message in Claude Code's JSONL transcript form.
func assistantLine(t *testing.T, uuid, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "assistant",
		"uuid": uuid,
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// insertCitedFacts stores a live fact and a superseded one, returning their ids.
func insertCitedFacts(t *testing.T, store interface {
	Insert(context.Context, memstore.Fact) (int64, error)
	Supersede(context.Context, int64, int64) error
}) (live, superseded int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	if live, err = store.Insert(ctx, memstore.Fact{Content: "Prefer small, separate commits.", Subject: "matthew", Category: "preference"}); err != nil {
		t.Fatal(err)
	}
	if superseded, err = store.Insert(ctx, memstore.Fact{Content: "Use one big commit per feature.", Subject: "matthew", Category: "preference"}); err != nil {
		t.Fatal(err)
	}
	newer, err := store.Insert(ctx, memstore.Fact{Content: "Commits are small and logical.", Subject: "matthew", Category: "preference"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Supersede(ctx, superseded, newer); err != nil {
		t.Fatal(err)
	}
	return live, superseded
}

// TestTranscriptRecordsCitations pins step 2 of docs/citation-feedback.md: the
// daemon reads citations out of an uploaded transcript itself, so no client has
// to remember to report them. Decision D2: a citation counts when its id
// resolves to an active fact the user can see; a superseded fact or an id that
// resolves to nothing is dropped.
func TestTranscriptRecordsCitations(t *testing.T) {
	ss := &citingSessionStore{}
	h, store := newTestHandlerWithSession(t, ss)
	live, superseded := insertCitedFacts(t, store)

	content := assistantLine(t, "a1", fmt.Sprintf("Per [fact %d], [fact %d] and [fact 999999].", live, superseded))
	resp := doJSON(t, h, "POST", "/v1/sessions/transcript", map[string]any{
		"session_id": "s-cite", "cwd": "/w/memstore", "content": content,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	if len(ss.recorded) != 1 {
		t.Fatalf("recorded %d citation(s), want 1 (the live fact only): %+v", len(ss.recorded), ss.recorded)
	}
	c := ss.recorded[0]
	if c.FactID != live || c.SessionID != "s-cite" || c.TurnUUID != "a1" {
		t.Errorf("recorded %+v, want fact %d in s-cite from turn a1", c, live)
	}
}

// The backfill reads citations already in session history, applying the same
// rules as the live path, and reports what it found and what it kept.
func TestCitationBackfill(t *testing.T) {
	ss := &citingSessionStore{}
	h, store := newTestHandlerWithSession(t, ss)
	live, _ := insertCitedFacts(t, store)
	ss.turns = map[string][]memstore.SessionTurn{
		"old-1": {{SessionID: "old-1", UUID: "x1", Role: "assistant", Content: fmt.Sprintf("As [fact %d] says.", live)}},
		"old-2": {{SessionID: "old-2", UUID: "x2", Role: "assistant", Content: "Only quoted: `[fact 999999]`."}},
		"old-3": {{SessionID: "old-3", UUID: "x3", Role: "assistant", Content: "[fact 999999] applies."}},
	}

	resp := doJSON(t, h, "POST", "/v1/citations/backfill", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out struct {
		Sessions int `json:"sessions"`
		Found    int `json:"found"`
		Recorded int `json:"recorded"`
		Rejected int `json:"rejected"`
	}
	decodeJSON(t, resp, &out)
	if out.Sessions != 3 || out.Found != 2 || out.Recorded != 1 || out.Rejected != 1 {
		t.Errorf("backfill = %+v, want 3 sessions, 2 found, 1 recorded, 1 rejected", out)
	}
	if len(ss.recorded) != 1 || ss.recorded[0].FactID != live || ss.recorded[0].SessionID != "old-1" {
		t.Errorf("recorded %+v, want fact %d in old-1", ss.recorded, live)
	}
}
