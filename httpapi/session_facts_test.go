package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/rawstore"
	"github.com/matthewjhunter/memstore/internal/teststore"
)

// compactedTranscript is a session about a widget subsystem that was
// compacted once, and with more set, resumed and compacted again.
func compactedTranscript(t *testing.T, more bool) string {
	t.Helper()
	enc := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	lines := []string{
		enc(map[string]any{"type": "ai-title", "aiTitle": "Widget retry design"}),
		enc(map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-11-01T10:00:00Z", "sessionId": rawSession, "cwd": "/work/widget",
			"message": map[string]any{"role": "user", "content": "design the widget retries"}}),
		enc(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "b1", "timestamp": "2026-11-01T11:00:00Z",
			"compactMetadata": map[string]any{"trigger": "auto", "preTokens": 180000}}),
		enc(map[string]any{"type": "user", "uuid": "c1", "parentUuid": "b1", "timestamp": "2026-11-01T11:00:00Z", "isCompactSummary": true,
			"message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation. The widget subsystem will use exponential backoff for retries."}}),
		enc(map[string]any{"type": "user", "uuid": "u2", "timestamp": "2026-11-01T11:01:00Z",
			"message": map[string]any{"role": "user", "content": "carry on"}}),
	}
	if more {
		lines = append(lines,
			enc(map[string]any{"type": "ai-title", "aiTitle": "Widget retry design and rollout"}),
			enc(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "b2", "timestamp": "2026-11-01T12:00:00Z",
				"compactMetadata": map[string]any{"trigger": "manual", "preTokens": 150000}}),
			enc(map[string]any{"type": "user", "uuid": "c2", "parentUuid": "b2", "timestamp": "2026-11-01T12:00:00Z", "isCompactSummary": true,
				"message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation. The rollout is staged over a week."}}),
			enc(map[string]any{"type": "user", "uuid": "u3", "timestamp": "2026-11-01T12:01:00Z",
				"message": map[string]any{"role": "user", "content": "finish it"}}),
		)
	}
	return strings.Join(lines, "\n") + "\n"
}

func factsHandler(t *testing.T) (*httpapi.Handler, teststore.Store) {
	t.Helper()
	return factsHandlerWith(t, &archivingStore{mockSessionStore: &mockSessionStore{}, user: 7})
}

// factsHandlerWith is factsHandler over a given session store.
func factsHandlerWith(t *testing.T, ss memstore.SessionStore) (*httpapi.Handler, teststore.Store) {
	t.Helper()
	rs, err := rawstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	embedder := &mockEmbedder{dim: 4}
	store := teststore.New(t, embedder, "test")
	sc := httpapi.NewSessionContext()
	t.Cleanup(sc.Stop)
	h := httpapi.New(store, embedder, "",
		httpapi.WithSessionStore(ss), httpapi.WithTranscriptStore(rs), httpapi.WithSessionContext(sc))
	return h, store
}

func listKind(t *testing.T, store teststore.Store, kind string, onlyActive bool) []memstore.Fact {
	t.Helper()
	facts, err := store.List(context.Background(), memstore.QueryOpts{Kind: kind, OnlyActive: onlyActive})
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func metaOf(t *testing.T, f memstore.Fact) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(f.Metadata, &m); err != nil {
		t.Fatalf("metadata of fact %d: %v", f.ID, err)
	}
	return m
}

// An upload leaves a session fact and one fact per compaction, the session
// linked to each. These are what make an archived session findable.
func TestRawUpload_RecordsTheSessionAndItsCompactionsAsFacts(t *testing.T) {
	h, store := factsHandler(t)
	if w := putRaw(h, rawSession, gzipBytes(t, compactedTranscript(t, false))); w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	sessions := listKind(t, store, "session", true)
	if len(sessions) != 1 {
		t.Fatalf("session facts = %d, want 1", len(sessions))
	}
	s := sessions[0]
	if !strings.HasPrefix(s.Content, "Widget retry design\n") {
		t.Errorf("session fact does not lead with the title: %q", s.Content)
	}
	for _, want := range []string{rawSession, "2026-11-01T10:00:00Z", "2026-11-01T11:01:00Z", "/work/repo", "1 compaction"} {
		if !strings.Contains(s.Content, want) {
			t.Errorf("session fact lacks %q:\n%s", want, s.Content)
		}
	}
	// putRaw sends cwd=/work/repo, and the client's cwd is what names the project.
	if s.Subject != "repo" {
		t.Errorf("session subject = %q, want the project name", s.Subject)
	}
	sm := metaOf(t, s)
	if sm["session_id"] != rawSession || sm["recall"] != "never" {
		t.Errorf("session metadata = %v", sm)
	}

	comps := listKind(t, store, "compaction", true)
	if len(comps) != 1 {
		t.Fatalf("compaction facts = %d, want 1", len(comps))
	}
	c := comps[0]
	if !strings.Contains(c.Content, "exponential backoff") {
		t.Errorf("compaction fact is not the summary: %q", c.Content)
	}
	cm := metaOf(t, c)
	if cm["session_id"] != rawSession || cm["summary_uuid"] != "c1" || cm["ordinal"] != float64(0) ||
		cm["trigger"] != "auto" || cm["format"] != "claude-code-marker" || cm["recall"] != "never" {
		t.Errorf("compaction metadata = %v", cm)
	}

	links, err := store.GetLinks(context.Background(), s.ID, memstore.LinkOutbound, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].TargetID != c.ID {
		t.Errorf("session links = %+v, want one to fact %d", links, c.ID)
	}
}

// A resumed session uploads again with a new title, a later end and another
// compaction. There is one current session fact, the earlier one superseded
// by it; the first compaction is not stored twice; and the current session
// fact links to both.
func TestRawUpload_AResumedSessionUpdatesItsFacts(t *testing.T) {
	h, store := factsHandler(t)
	for _, more := range []bool{false, true, true} {
		if w := putRaw(h, rawSession, gzipBytes(t, compactedTranscript(t, more))); w.Code != http.StatusAccepted {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	}

	active := listKind(t, store, "session", true)
	all := listKind(t, store, "session", false)
	if len(active) != 1 || len(all) != 2 {
		t.Fatalf("session facts: %d active of %d, want 1 of 2 (an unchanged upload adds none)", len(active), len(all))
	}
	if !strings.HasPrefix(active[0].Content, "Widget retry design and rollout\n") {
		t.Errorf("current session fact = %q", active[0].Content)
	}
	for _, f := range all {
		if f.ID != active[0].ID && (f.SupersededBy == nil || *f.SupersededBy != active[0].ID) {
			t.Errorf("earlier session fact %d is not superseded by the current one", f.ID)
		}
	}

	comps := listKind(t, store, "compaction", false)
	if len(comps) != 2 {
		t.Fatalf("compaction facts = %d, want 2", len(comps))
	}
	links, err := store.GetLinks(context.Background(), active[0].ID, memstore.LinkOutbound, "compaction")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 {
		t.Errorf("current session fact has %d compaction links, want 2", len(links))
	}
}

// A compaction summary is long and about everything the session touched. It
// would win prompt recall on almost any prompt from that project, so it is
// kept out of recall and left in search.
func TestSessionFactsStayOutOfRecallAndInSearch(t *testing.T) {
	h, store := factsHandler(t)
	seedWidgetFacts(t, store)
	if w := putRaw(h, rawSession, gzipBytes(t, compactedTranscript(t, false))); w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "how does the widget subsystem handle exponential backoff retries", "session_id": "r", "cwd": "/work/repo", "limit": 10,
	})
	var recalled struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &recalled)
	if len(recalled.Facts) == 0 {
		t.Fatal("recall returned nothing; the test needs it to return the widget facts")
	}
	archived := map[int64]bool{}
	for _, kind := range []string{"session", "compaction"} {
		for _, f := range listKind(t, store, kind, true) {
			archived[f.ID] = true
		}
	}
	for _, f := range recalled.Facts {
		if archived[f.ID] {
			t.Errorf("recall injected archive fact %d", f.ID)
		}
	}

	found, err := store.SearchFTS(context.Background(), "exponential backoff", memstore.SearchOpts{MaxResults: 20, OnlyActive: true})
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, r := range found {
		hit = hit || r.Fact.Kind == "compaction"
	}
	if !hit {
		t.Error("search did not find the compaction fact")
	}
}

// The flag is data on the fact, so it works for any fact that carries it,
// not for a list of kinds.
func TestRecallHonoursTheNeverFlagOnAnyFact(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)
	ctx := context.Background()
	id, err := store.Insert(ctx, memstore.Fact{
		Content: "Herald is a Go RSS feed aggregator with a private note", Subject: "herald", Category: "project",
		Metadata: json.RawMessage(`{"recall":"never"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{"prompt": "tell me about the herald feed aggregator", "session_id": "r", "limit": 10})
	var recalled struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &recalled)
	if len(recalled.Facts) == 0 {
		t.Fatal("recall returned nothing")
	}
	for _, f := range recalled.Facts {
		if f.ID == id {
			t.Error("recall injected a fact flagged recall=never")
		}
	}
}
