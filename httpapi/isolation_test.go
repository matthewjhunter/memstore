package httpapi_test

// Two-token isolation battery: proves that user A's facts, sessions, and
// context data are invisible to user B when the handler resolves per-request
// scoped stores via Identity.UserID.
//
// Requires a live PostgreSQL instance. Gate: MEMSTORE_TEST_PG must be set.
// The test uses a throw-away schema on a fresh pool and cleans up via
// t.Cleanup. It does NOT test the extraction path because ExtractQueue has
// no exported synchronous drain seam; adding a sleep-based assertion would
// be flaky. That surface requires a ProcessOnce equivalent or test seam to
// be added to ExtractQueue in a follow-up.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/pgstore"
	_ "modernc.org/sqlite"
)

// isolationFixture holds the handler and per-user bearer tokens for
// the two-token isolation battery.
type isolationFixture struct {
	handler http.Handler
	tokenA  string
	tokenB  string
	queue   *httpapi.ExtractQueue // for synchronous extraction draining
}

// isoExtractGenerator is a deterministic generator for the extraction path.
// It returns a single-fact extraction array for the extraction prompt and a
// "trivial" summary envelope for the summary prompt (so no summary fact is
// written), keeping the extraction assertion focused on the extracted fact.
type isoExtractGenerator struct {
	factContent string
	factSubject string
}

func (g *isoExtractGenerator) Generate(_ context.Context, prompt string) (string, error) {
	return g.respond(prompt), nil
}

func (g *isoExtractGenerator) GenerateJSON(_ context.Context, prompt string) (string, error) {
	return g.respond(prompt), nil
}

func (g *isoExtractGenerator) Model() string { return "iso-extract-mock" }

func (g *isoExtractGenerator) respond(prompt string) string {
	switch {
	// Extraction prompt: return one fact as a JSON array. Checked first because
	// it is the only prompt that opens with "Extract factual claims".
	case strings.Contains(prompt, "Extract factual claims"):
		return fmt.Sprintf(`[{"content":%q,"subject":%q,"category":"project"}]`, g.factContent, g.factSubject)

	// Summary prompt: return a trivial outcome envelope so no summary fact is
	// written (keeps the extraction assertion focused on the extracted fact).
	// Matched on "session summarizer", which is unique to summaryPrompt.
	case strings.Contains(prompt, "session summarizer"):
		return `{"outcome":"trivial","scope":"","lead":"nothing of note","decisions":[],"outcomes":[],"error":{"kind":"","detail":""}}`

	// Rating / scoring prompts (rateFact, rateHint, scoreDesirability) all ask
	// for a {"score", "reason"} object. This is the branch backfill's per-fact
	// rating step lands in -- returning a valid object lets a fact be rated.
	default:
		return `{"score": 1, "reason": "relevant to the session"}`
	}
}

// newIsolationFixture sets up a fresh postgres store, two users, two tokens,
// and a fully-wired Handler. It skips if MEMSTORE_TEST_PG is not set.
func newIsolationFixture(t *testing.T) *isolationFixture {
	t.Helper()
	dsn := os.Getenv("MEMSTORE_TEST_PG")
	if dsn == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping isolation battery")
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Tear down in reverse dependency order so we start fresh.
	for _, tbl := range []string{
		"context_feedback", "context_injections", "context_hints",
		"session_turns", "session_hooks",
		"api_tokens",
		"memstore_links", "memstore_facts",
		"memstore_users", "memstore_meta", "memstore_version",
	} {
		pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", tbl))
	}

	emb := &mockEmbedder{dim: 4}

	// Bootstrap ordering on a virgin DB (mirrors plan 010's pg fixtures):
	// 1. The first New() runs and commits the migration (creating
	//    memstore_users / memstore_meta), then fails at the default-user gate
	//    because no default_user is recorded yet. Tolerate that specific error;
	//    fatal on anything else.
	if _, err := pgstore.New(ctx, pool, emb, "iso", 4, 0); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("pgstore.New (bootstrap): %v", err)
	}
	// 2. Now the identity tables exist, so seed the default user.
	if err := pgstore.InitIdentity(ctx, pool, "iso", "iso-default"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
	// 3. New() again -- the default user resolves and we get the real store.
	pgStore, err := pgstore.New(ctx, pool, emb, "iso", 4, 0)
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}

	// Provision two named users.
	uidA, err := pgstore.EnsureUser(ctx, pool, "iso", "iso-user-a")
	if err != nil {
		t.Fatalf("EnsureUser iso-user-a: %v", err)
	}
	uidB, err := pgstore.EnsureUser(ctx, pool, "iso", "iso-user-b")
	if err != nil {
		t.Fatalf("EnsureUser iso-user-b: %v", err)
	}

	// Set up token store and issue one token per user.
	ts, err := pgstore.NewTokenStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	tokA, err := ts.Issue(ctx, "user-a@test", pgstore.IssueOpts{UserID: uidA, Scopes: []string{"read", "write"}})
	if err != nil {
		t.Fatalf("Issue tokenA: %v", err)
	}
	tokB, err := ts.Issue(ctx, "user-b@test", pgstore.IssueOpts{UserID: uidB, Scopes: []string{"read", "write"}})
	if err != nil {
		t.Fatalf("Issue tokenB: %v", err)
	}

	// Set up session store.
	ss, err := pgstore.NewSessionStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	// Set up the extract queue with a deterministic generator. It is NOT started
	// (no background worker), so the extraction test drives it synchronously via
	// ProcessOnce -- no sleep, no race with a worker goroutine.
	gen := &isoExtractGenerator{
		factContent: "user B extracted secret fact",
		factSubject: "iso-extract",
	}
	xq := httpapi.NewExtractQueue(pgStore, emb, gen, ss)

	// Wire up the handler with the token verifier, session store, and extract queue.
	tv := isoTokenVerifier{ts: ts}
	h := httpapi.New(
		pgStore,
		emb,
		"",
		httpapi.WithTokenVerifier(tv),
		httpapi.WithSessionStore(ss),
		httpapi.WithExtractQueue(xq),
	)

	return &isolationFixture{
		handler: h,
		tokenA:  tokA,
		tokenB:  tokB,
		queue:   xq,
	}
}

// isoTokenVerifier adapts pgstore.TokenStore to httpapi.TokenVerifier for
// the isolation test. Same adapter as cmd/memstored/main.go tokenVerifier.
type isoTokenVerifier struct{ ts *pgstore.TokenStore }

func (v isoTokenVerifier) VerifyToken(ctx context.Context, token string) (httpapi.Identity, error) {
	r, err := v.ts.Verify(ctx, token)
	if err != nil {
		return httpapi.Identity{}, err
	}
	return httpapi.Identity{Name: r.Name, Scopes: r.Scopes, Source: "bearer", UserID: r.UserID}, nil
}

// isoRequest fires a JSON request to the handler, authorised with the given token.
func isoRequest(t *testing.T, h http.Handler, token, method, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

// mustDecodeJSON panics-on-fail helper for test assertions.
func mustDecodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// --- Facts isolation ---

func TestIsolation_Facts(t *testing.T) {
	f := newIsolationFixture(t)

	// User A inserts a fact.
	resp := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
		"content":  "user A secret",
		"subject":  "iso-test",
		"category": "test",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("A insert: want 201, got %d", resp.StatusCode)
	}
	var created map[string]any
	mustDecodeJSON(t, resp, &created)
	aID := int64(created["id"].(float64))

	// User B cannot GET user A's fact by ID.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", fmt.Sprintf("/v1/facts/%d", aID), nil)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("B GET A's fact: want 404, got %d body=%s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}

	// User B's fact list does not contain user A's fact.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", "/v1/facts?subject=iso-test", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B list: want 200, got %d", resp.StatusCode)
	}
	var facts []memstore.Fact
	mustDecodeJSON(t, resp, &facts)
	for _, ff := range facts {
		if ff.ID == aID {
			t.Errorf("B's list contains A's fact id=%d", aID)
		}
	}

	// User B's search does not return user A's fact.
	resp = isoRequest(t, f.handler, f.tokenB, "POST", "/v1/search", map[string]any{
		"query": "user A secret",
		"limit": 20,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B search: want 200, got %d", resp.StatusCode)
	}
	var results []memstore.SearchResult
	mustDecodeJSON(t, resp, &results)
	for _, r := range results {
		if r.Fact.ID == aID {
			t.Errorf("B's search contains A's fact id=%d", aID)
		}
	}

	// User B's FTS search does not return user A's fact.
	resp = isoRequest(t, f.handler, f.tokenB, "POST", "/v1/search/fts", map[string]any{
		"query": "user A secret",
		"limit": 20,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B fts search: want 200, got %d", resp.StatusCode)
	}
	mustDecodeJSON(t, resp, &results)
	for _, r := range results {
		if r.Fact.ID == aID {
			t.Errorf("B's FTS search contains A's fact id=%d", aID)
		}
	}

	// User B's history does not contain user A's fact.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", "/v1/history/iso-test", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B history: want 200, got %d", resp.StatusCode)
	}
	var hist []memstore.HistoryEntry
	mustDecodeJSON(t, resp, &hist)
	for _, e := range hist {
		if e.Fact.ID == aID {
			t.Errorf("B's history contains A's fact id=%d", aID)
		}
	}

	// User B cannot delete user A's fact.
	resp = isoRequest(t, f.handler, f.tokenB, "DELETE", fmt.Sprintf("/v1/facts/%d", aID), nil)
	// Expect 500 (delete of missing row in scoped store) or 404 -- not 200.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("B deleted A's fact: got 200")
	}
	resp.Body.Close()

	// User A's fact is still reachable by A after B's attempted delete.
	resp = isoRequest(t, f.handler, f.tokenA, "GET", fmt.Sprintf("/v1/facts/%d", aID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("A GET own fact after B's delete attempt: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Subsystems isolation ---

func TestIsolation_Subsystems(t *testing.T) {
	f := newIsolationFixture(t)

	// A inserts a fact with a subsystem.
	resp := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
		"content":   "A has a secret subsystem fact",
		"subject":   "iso-sub",
		"category":  "test",
		"subsystem": "private-sub",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("A insert: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// B's subsystem list does not contain A's subsystem.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", "/v1/subsystems?subject=iso-sub", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B subsystems: want 200, got %d", resp.StatusCode)
	}
	var subs []string
	mustDecodeJSON(t, resp, &subs)
	for _, s := range subs {
		if s == "private-sub" {
			t.Errorf("B's subsystems list contains A's subsystem %q", s)
		}
	}
}

// --- Links isolation ---

func TestIsolation_Links(t *testing.T) {
	f := newIsolationFixture(t)

	// A inserts two facts and links them.
	r1 := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
		"content": "A fact 1", "subject": "iso-link", "category": "test",
	})
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("A insert fact1: %d", r1.StatusCode)
	}
	var c1 map[string]any
	mustDecodeJSON(t, r1, &c1)
	id1 := int64(c1["id"].(float64))

	r2 := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
		"content": "A fact 2", "subject": "iso-link", "category": "test",
	})
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("A insert fact2: %d", r2.StatusCode)
	}
	var c2 map[string]any
	mustDecodeJSON(t, r2, &c2)
	id2 := int64(c2["id"].(float64))

	rl := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/links", map[string]any{
		"source_id": id1, "target_id": id2, "link_type": "related",
	})
	if rl.StatusCode != http.StatusCreated {
		t.Fatalf("A link: want 201, got %d", rl.StatusCode)
	}
	var lc map[string]any
	mustDecodeJSON(t, rl, &lc)
	linkID := int64(lc["id"].(float64))

	// B cannot GET A's link.
	resp := isoRequest(t, f.handler, f.tokenB, "GET", fmt.Sprintf("/v1/links/%d", linkID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B GET A's link: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// B sees no links on A's fact IDs.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", fmt.Sprintf("/v1/facts/%d/links", id1), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B get links for A's fact: want 200, got %d", resp.StatusCode)
	}
	var links []memstore.Link
	mustDecodeJSON(t, resp, &links)
	if len(links) != 0 {
		t.Errorf("B sees %d links on A's fact, want 0", len(links))
	}
}

// --- Recall isolation ---

func TestIsolation_Recall(t *testing.T) {
	f := newIsolationFixture(t)

	// A inserts a distinctive fact.
	resp := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
		"content":  "xyzzy-unique-recall-isolation-secret",
		"subject":  "iso-recall",
		"category": "test",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("A insert: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// B's recall for a prompt containing A's keyword returns nothing from A.
	resp = isoRequest(t, f.handler, f.tokenB, "POST", "/v1/recall", map[string]any{
		"prompt": "xyzzy-unique-recall-isolation-secret",
		"limit":  20,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B recall: want 200, got %d", resp.StatusCode)
	}
	var rec struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	mustDecodeJSON(t, resp, &rec)
	for _, rf := range rec.Facts {
		resp2 := isoRequest(t, f.handler, f.tokenA, "GET", fmt.Sprintf("/v1/facts/%d", rf.ID), nil)
		if resp2.StatusCode == http.StatusOK {
			// A could retrieve it -- check it's not A's "secret" fact.
			var ff memstore.Fact
			mustDecodeJSON(t, resp2, &ff)
			if ff.Content == "xyzzy-unique-recall-isolation-secret" {
				t.Errorf("B's recall returned A's secret fact id=%d", rf.ID)
			}
		} else {
			resp2.Body.Close()
		}
	}
}

// --- Session isolation ---

func TestIsolation_Sessions(t *testing.T) {
	f := newIsolationFixture(t)

	sessionID := "iso-test-session-a-001"

	// A posts a session hook.
	resp := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/sessions/hook", map[string]any{
		"session_id": sessionID,
		"type":       "stop",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("A hook: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A stores a context hint.
	resp = isoRequest(t, f.handler, f.tokenA, "POST", "/v1/context/hints", map[string]any{
		"session_id": sessionID,
		"hint_text":  "A private hint",
		"turn_index": 0,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("A store hint: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// B cannot see A's hints when querying by A's session_id.
	resp = isoRequest(t, f.handler, f.tokenB, "GET",
		fmt.Sprintf("/v1/context/hints?session_id=%s", sessionID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B get hints: want 200, got %d", resp.StatusCode)
	}
	var hints []memstore.ContextHint
	mustDecodeJSON(t, resp, &hints)
	for _, h := range hints {
		if h.SessionID == sessionID {
			t.Errorf("B sees A's hint for session %s", sessionID)
		}
	}
}

// --- Feedback isolation ---

func TestIsolation_Feedback(t *testing.T) {
	f := newIsolationFixture(t)

	// A inserts a fact.
	resp := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
		"content": "A feedback isolation fact", "subject": "iso-fb", "category": "test",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("A insert: want 201, got %d", resp.StatusCode)
	}
	var created map[string]any
	mustDecodeJSON(t, resp, &created)
	aID := int64(created["id"].(float64))

	// A records feedback on A's own fact.
	resp = isoRequest(t, f.handler, f.tokenA, "POST", "/v1/context/injections", map[string]any{
		"session_id": "iso-fb-session-a",
		"ref_id":     fmt.Sprintf("%d", aID),
		"ref_type":   memstore.RefTypeFact,
		"rank":       0,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A record injection: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = isoRequest(t, f.handler, f.tokenA, "POST", "/v1/context/feedback", map[string]any{
		"ref_id":     fmt.Sprintf("%d", aID),
		"ref_type":   memstore.RefTypeFact,
		"session_id": "iso-fb-session-a",
		"score":      1,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A record feedback: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// B's recall for the same prompt should not be influenced by A's fact or feedback.
	// B sees no facts with A's id in any response.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", fmt.Sprintf("/v1/facts/%d", aID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B GET A's fact after feedback: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Count isolation ---

func TestIsolation_ActiveCount(t *testing.T) {
	f := newIsolationFixture(t)

	// A's initial count.
	resp := isoRequest(t, f.handler, f.tokenA, "GET", "/v1/facts/count", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A count: want 200, got %d", resp.StatusCode)
	}
	var countBefore map[string]int64
	mustDecodeJSON(t, resp, &countBefore)
	aBefore := countBefore["count"]

	// A inserts 2 facts.
	for i := 0; i < 2; i++ {
		r := isoRequest(t, f.handler, f.tokenA, "POST", "/v1/facts", map[string]any{
			"content": fmt.Sprintf("count test fact %d", i), "subject": "iso-count", "category": "test",
		})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("A insert %d: want 201, got %d", i, r.StatusCode)
		}
		r.Body.Close()
	}

	// A's count increased by 2.
	resp = isoRequest(t, f.handler, f.tokenA, "GET", "/v1/facts/count", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A count after insert: want 200, got %d", resp.StatusCode)
	}
	var countAfterA map[string]int64
	mustDecodeJSON(t, resp, &countAfterA)
	if got := countAfterA["count"] - aBefore; got != 2 {
		t.Errorf("A count delta: want 2, got %d", got)
	}

	// B's count did not change.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", "/v1/facts/count", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B count: want 200, got %d", resp.StatusCode)
	}
	var countB map[string]int64
	mustDecodeJSON(t, resp, &countB)
	// B's count should not include A's 2 new facts.
	if countB["count"] >= countAfterA["count"] {
		t.Errorf("B's count (%d) >= A's count (%d), expected isolation", countB["count"], countAfterA["count"])
	}
}

// --- Backfill caller-scoping ---

// TestIsolation_BackfillScopedToCaller proves the /v1/context/backfill-feedback
// endpoint rates only the CALLER's sessions and facts. A and B each set up one
// session with one injected fact. B's backfill must report exactly B's one
// session, never A's -- and must not touch A's feedback.
func TestIsolation_BackfillScopedToCaller(t *testing.T) {
	f := newIsolationFixture(t)

	// Helper: for a given token, insert a fact, post a transcript (creates
	// session turns), and record an injection of that fact into the session.
	setup := func(token, sessionID, content string) int64 {
		t.Helper()
		r := isoRequest(t, f.handler, token, "POST", "/v1/facts", map[string]any{
			"content": content, "subject": "iso-backfill", "category": "test",
		})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("insert fact: want 201, got %d", r.StatusCode)
		}
		var created map[string]any
		mustDecodeJSON(t, r, &created)
		id := int64(created["id"].(float64))

		// Post a minimal JSONL transcript so the session has turns.
		transcript := fmt.Sprintf(
			`{"type":"user","uuid":"u1","timestamp":"2026-06-13T00:00:00Z","message":{"role":"user","content":"working on %s"}}`,
			sessionID)
		r = isoRequest(t, f.handler, token, "POST", "/v1/sessions/transcript", map[string]any{
			"session_id": sessionID,
			"cwd":        "/tmp/iso",
			"content":    transcript,
		})
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("post transcript: want 202, got %d", r.StatusCode)
		}
		r.Body.Close()
		// Drain the extraction job this transcript enqueued so the queue is clean
		// for the dedicated extraction test (and so background state is settled).
		f.queue.ProcessOnce()

		// Record an injection of the fact into the session (unrated -> backfill target).
		r = isoRequest(t, f.handler, token, "POST", "/v1/context/injections", map[string]any{
			"session_id": sessionID,
			"ref_id":     fmt.Sprintf("%d", id),
			"ref_type":   memstore.RefTypeFact,
			"rank":       0,
		})
		if r.StatusCode != http.StatusOK {
			t.Fatalf("record injection: want 200, got %d", r.StatusCode)
		}
		r.Body.Close()
		return id
	}

	setup(f.tokenA, "iso-bf-session-a", "A backfill fact")
	setup(f.tokenB, "iso-bf-session-b", "B backfill fact")

	// B runs backfill. It must see exactly one session (B's), never A's.
	resp := isoRequest(t, f.handler, f.tokenB, "POST", "/v1/context/backfill-feedback", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("B backfill: want 200, got %d body=%s", resp.StatusCode, body)
	}
	var bResult httpapi.BackfillResult
	mustDecodeJSON(t, resp, &bResult)
	if bResult.Sessions != 1 {
		t.Errorf("B backfill Sessions = %d, want 1 (only B's session)", bResult.Sessions)
	}
	if bResult.Rated != 1 {
		t.Errorf("B backfill Rated = %d, want 1 (only B's fact)", bResult.Rated)
	}

	// A runs backfill afterwards. A still has exactly one unrated session --
	// proving B's run did NOT consume or rate A's data.
	resp = isoRequest(t, f.handler, f.tokenA, "POST", "/v1/context/backfill-feedback", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A backfill: want 200, got %d", resp.StatusCode)
	}
	var aResult httpapi.BackfillResult
	mustDecodeJSON(t, resp, &aResult)
	if aResult.Sessions != 1 {
		t.Errorf("A backfill Sessions = %d, want 1 (A's session still unrated after B's run)", aResult.Sessions)
	}
	if aResult.Rated != 1 {
		t.Errorf("A backfill Rated = %d, want 1", aResult.Rated)
	}
}

// --- Extraction ownership ---

// TestIsolation_ExtractionOwnedByB proves the per-job ForUser routing in
// processJob writes extracted facts to the posting user's partition. B posts a
// transcript; the queue is drained synchronously via ProcessOnce; then B sees
// the extracted fact and A sees none of it. This exercises the highest-leak
// write path -- extraction WRITES facts, and an unscoped job would write them
// as the default user.
func TestIsolation_ExtractionOwnedByB(t *testing.T) {
	f := newIsolationFixture(t)

	const extractSubject = "iso-extract"
	const extractContent = "user B extracted secret fact"

	// B posts a transcript with enough content for extraction to run.
	transcript := `{"type":"user","uuid":"e1","timestamp":"2026-06-13T00:00:00Z","message":{"role":"user","content":"we decided to use postgres for the iso-extract project because it has good vector support"}}
{"type":"assistant","uuid":"e2","timestamp":"2026-06-13T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Agreed, postgres with pgvector is the right call for iso-extract."}]}}`
	resp := isoRequest(t, f.handler, f.tokenB, "POST", "/v1/sessions/transcript", map[string]any{
		"session_id": "iso-extract-session-b",
		"cwd":        "/tmp/iso-extract",
		"content":    transcript,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("B post transcript: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Drain the enqueued extraction job synchronously -- no sleep, no worker race.
	if !f.queue.ProcessOnce() {
		t.Fatal("ProcessOnce: expected a queued extraction job, got none")
	}

	// B sees the extracted fact in its own scope.
	resp = isoRequest(t, f.handler, f.tokenB, "GET", "/v1/facts?subject="+extractSubject, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B list extracted: want 200, got %d", resp.StatusCode)
	}
	var bFacts []memstore.Fact
	mustDecodeJSON(t, resp, &bFacts)
	var found *memstore.Fact
	for i := range bFacts {
		if bFacts[i].Content == extractContent {
			found = &bFacts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("B does not see its own extracted fact %q (got %d facts)", extractContent, len(bFacts))
	}

	// A sees NONE of B's extracted facts.
	resp = isoRequest(t, f.handler, f.tokenA, "GET", "/v1/facts?subject="+extractSubject, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A list extracted: want 200, got %d", resp.StatusCode)
	}
	var aFacts []memstore.Fact
	mustDecodeJSON(t, resp, &aFacts)
	for _, af := range aFacts {
		if af.ID == found.ID || af.Content == extractContent {
			t.Errorf("A sees B's extracted fact id=%d content=%q -- per-job scoping leaked", af.ID, af.Content)
		}
	}

	// A's direct GET of B's extracted fact id is a not-found.
	resp = isoRequest(t, f.handler, f.tokenA, "GET", fmt.Sprintf("/v1/facts/%d", found.ID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A GET B's extracted fact: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
