package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// textArchive adds what the backfill reads to the fake session store: the
// sessions held as text only, and their turns.
type textArchive struct {
	*archivingStore
	sessions map[string][]memstore.SessionTurn
}

func (a *textArchive) UnarchivedSessions(context.Context) ([]string, error) {
	archived := map[string]bool{}
	for _, arch := range a.archives {
		archived[arch.SessionID] = true
	}
	var ids []string
	for id := range a.sessions {
		if !archived[id] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (a *textArchive) GetSessionTurns(_ context.Context, id string) ([]memstore.SessionTurn, error) {
	return a.sessions[id], nil
}

func oldSessionTurns() []memstore.SessionTurn {
	at := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	mk := func(i int, role, text string) memstore.SessionTurn {
		return memstore.SessionTurn{SessionID: "old-1", UUID: "t" + string(rune('0'+i)), TurnIndex: i, Role: role, Content: text, CWD: "/work/herald", CreatedAt: at.Add(time.Duration(i) * time.Minute)}
	}
	return []memstore.SessionTurn{
		mk(0, "user", "set up the feed reader"),
		mk(1, "assistant", "done"),
		mk(2, "user", "This session is being continued from a previous conversation. The feed reader polls every ten minutes."),
		mk(3, "user", "now add grouping"),
	}
}

func TestArchiveBackfill_GivesATextOnlySessionItsRecords(t *testing.T) {
	base := &archivingStore{mockSessionStore: &mockSessionStore{}, user: 7}
	ta := &textArchive{archivingStore: base, sessions: map[string][]memstore.SessionTurn{"old-1": oldSessionTurns()}}
	h, store := factsHandlerWith(t, ta)

	ctx := context.Background()
	summaryID, err := store.Insert(ctx, memstore.Fact{
		Content: "Set up the herald feed reader.\n\nDecisions:\n- poll every ten minutes", Subject: "herald", Category: "project", Kind: "summary",
		Metadata: json.RawMessage(`{"session_id":"old-1","source":"session_summary"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A dry run counts and writes nothing.
	resp := doJSON(t, h, "POST", "/v1/sessions/archive/backfill", map[string]any{"dry_run": true})
	var dry map[string]any
	decodeJSON(t, resp, &dry)
	if dry["sessions"] != float64(1) || dry["compactions"] != float64(1) || dry["written"] != float64(0) {
		t.Fatalf("dry run = %v", dry)
	}
	if len(listKind(t, store, "session", true)) != 0 || len(ta.archives) != 0 {
		t.Fatal("a dry run wrote something")
	}

	resp = doJSON(t, h, "POST", "/v1/sessions/archive/backfill", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var res map[string]any
	decodeJSON(t, resp, &res)
	if res["written"] != float64(1) || res["compacted"] != float64(1) || res["errors"] != float64(0) {
		t.Fatalf("backfill = %v", res)
	}

	// There is no file, so the archive has no path and its segments no bytes.
	if len(ta.archives) != 1 || ta.archives[0].Path != "" || ta.archives[0].SessionID != "old-1" {
		t.Fatalf("archive = %+v", ta.archives)
	}
	p := ta.parsed[0]
	if len(p.Segments) != 2 || p.Segments[0].LastLine != 2 || p.Segments[1].FirstLine != 4 || p.Segments[1].EndOffset != 0 {
		t.Errorf("segments = %+v", p.Segments)
	}

	sessions := listKind(t, store, "session", true)
	if len(sessions) != 1 {
		t.Fatalf("session facts = %d", len(sessions))
	}
	// With no title in a text archive, the summary's lead stands in.
	if !strings.HasPrefix(sessions[0].Content, "Set up the herald feed reader.\n") || sessions[0].Subject != "herald" {
		t.Errorf("session fact = %q (subject %q)", sessions[0].Content, sessions[0].Subject)
	}
	comps := listKind(t, store, "compaction", true)
	if len(comps) != 1 || !strings.Contains(comps[0].Content, "polls every ten minutes") {
		t.Fatalf("compaction facts = %+v", comps)
	}
	if metaOf(t, comps[0])["format"] != "continuation-prefix" {
		t.Errorf("compaction metadata = %v", metaOf(t, comps[0]))
	}
	links, err := store.GetLinks(ctx, sessions[0].ID, memstore.LinkOutbound, "summary")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].TargetID != summaryID {
		t.Errorf("summary links = %+v", links)
	}

	// Running it again finds nothing left to do.
	resp = doJSON(t, h, "POST", "/v1/sessions/archive/backfill", nil)
	var again map[string]any
	decodeJSON(t, resp, &again)
	if again["sessions"] != float64(0) || len(listKind(t, store, "session", false)) != 1 {
		t.Errorf("second run = %v", again)
	}
}
