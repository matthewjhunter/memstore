package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/timing"
)

// recallOutcome runs one recall the way the daemon does, with a recorder on
// the request context, and returns what the access log line would carry.
func recallOutcome(t *testing.T, h *httpapi.Handler, body map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx := timing.NewContext(context.Background())
	req := httptest.NewRequest("POST", "/v1/recall", bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("recall status %d: %s", w.Code, w.Body)
	}
	out := map[string]any{}
	for _, a := range timing.Attrs(ctx) {
		out[a.Key] = a.Value.Any()
	}
	return out
}

// Four recalls in five came back empty in production and the access log could
// not say why: an empty answer for a prompt that matched nothing and one for a
// prompt whose every match had already been shown look identical from outside.
func TestRecall_LogsWhatItFoundAndWhyItReturnedNothing(t *testing.T) {
	t.Run("returned", func(t *testing.T) {
		h, store, _ := newTestHandlerWithRecall(t)
		seedFacts(t, store)
		got := recallOutcome(t, h, map[string]any{"prompt": "tell me about the herald feed aggregator", "session_id": "s"})
		if n, _ := got["recall_returned"].(int64); n == 0 {
			t.Fatalf("recall_returned = %v, want > 0 (%v)", got["recall_returned"], got)
		}
		if n, _ := got["recall_pool"].(int64); n == 0 {
			t.Errorf("recall_pool = %v, want > 0", got["recall_pool"])
		}
		if reason, ok := got["recall_empty"]; ok {
			t.Errorf("recall_empty = %v on a recall that returned facts", reason)
		}
	})

	t.Run("nothing matched", func(t *testing.T) {
		h, _, _ := newTestHandlerWithRecall(t)
		got := recallOutcome(t, h, map[string]any{"prompt": "tell me about the herald feed aggregator", "session_id": "s"})
		if got["recall_empty"] != "no_hits" {
			t.Errorf("recall_empty = %v, want no_hits (%v)", got["recall_empty"], got)
		}
		if got["recall_returned"] != int64(0) {
			t.Errorf("recall_returned = %v, want 0", got["recall_returned"])
		}
	})

	t.Run("no usable words", func(t *testing.T) {
		h, store, _ := newTestHandlerWithRecall(t)
		seedFacts(t, store)
		got := recallOutcome(t, h, map[string]any{"prompt": "the and of it", "session_id": "s"})
		if got["recall_empty"] != "no_words" {
			t.Errorf("recall_empty = %v, want no_words (%v)", got["recall_empty"], got)
		}
	})

	t.Run("already shown", func(t *testing.T) {
		h, store, _ := newTestHandlerWithRecall(t)
		seedFacts(t, store)
		body := map[string]any{"prompt": "tell me about the herald feed aggregator", "session_id": "s"}
		if first := recallOutcome(t, h, body); first["recall_returned"] == int64(0) {
			t.Fatalf("first recall returned nothing (%v)", first)
		}
		// The same prompt again: the same facts match and all of them have
		// been shown.
		got := recallOutcome(t, h, body)
		if got["recall_empty"] != "seen" {
			t.Errorf("recall_empty = %v, want seen (%v)", got["recall_empty"], got)
		}
		if n, _ := got["recall_candidates"].(int64); n == 0 {
			t.Errorf("recall_candidates = 0: the reason should be seen because matches existed (%v)", got)
		}
	})
}

// The cross-encoder pass is the slowest stage of a recall and was the one
// stage the line did not time: the timed phases summed to a quarter of the
// request.
func TestRecall_TimesTheRerankPass(t *testing.T) {
	rr := fakeRecallReranker{score: func(string) float64 { return 0.9 }}
	h, store := recallHandlerWithReranker(t, rr, memstore.RerankDominant, 0)
	seedWidgetFacts(t, store)
	got := recallOutcome(t, h, map[string]any{"prompt": "how does the widget subsystem handle retries", "session_id": "s"})
	if got["rerank_calls"] != int64(1) {
		t.Errorf("rerank_calls = %v, want 1 (%v)", got["rerank_calls"], got)
	}
}
