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

	// Open the pgstore; this runs migrations and creates the identity schema.
	pgStore, err := pgstore.New(ctx, pool, emb, "iso", 4, 0)
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}

	// Seed the identity layer with the default user so tier3-init is satisfied.
	if err := pgstore.InitIdentity(ctx, pool, "iso", "iso-default"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
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

	// Wire up the handler with the token verifier and session store.
	// No extract queue -- extraction isolation requires a sync drain seam (TODO).
	tv := isoTokenVerifier{ts: ts}
	h := httpapi.New(
		pgStore,
		emb,
		"",
		httpapi.WithTokenVerifier(tv),
		httpapi.WithSessionStore(ss),
	)

	return &isolationFixture{
		handler: h,
		tokenA:  tokA,
		tokenB:  tokB,
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
