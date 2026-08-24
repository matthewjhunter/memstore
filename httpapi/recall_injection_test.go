package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// Recall is the highest-volume read path in the system and, before this, the
// only one that left no trace: a fact injected into hundreds of prompts read as
// never used. The recording lives in the daemon rather than in the hook or in a
// model-initiated tool call, because both of those have already been tried and
// both went silent (context_injections stopped 2026-05-27, context_feedback
// 2026-06-07, confirmed_count is zero corpus-wide).
func TestRecallRecordsInjection(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)
	ctx := context.Background()

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "tell me about the herald feed aggregator",
	})

	var out struct {
		Facts []struct {
			ID int64 `json:"id"`
		} `json:"facts"`
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recall status = %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode recall response: %v", err)
	}
	if len(out.Facts) == 0 {
		t.Fatal("recall returned no facts; cannot assert injection recording")
	}

	for _, rf := range out.Facts {
		f, err := store.Get(ctx, rf.ID)
		if err != nil {
			t.Fatalf("get fact %d: %v", rf.ID, err)
		}
		if f == nil {
			t.Fatalf("fact %d vanished", rf.ID)
		}
		if f.InjectCount != 1 {
			t.Errorf("fact %d: inject_count = %d, want 1", rf.ID, f.InjectCount)
		}
		if f.LastInjectedAt == nil {
			t.Errorf("fact %d: last_injected_at not set", rf.ID)
		}
		// Injection must not masquerade as an explicit search hit; #157's
		// prune predicate has to be able to tell the two apart.
		if f.UseCount != 0 {
			t.Errorf("fact %d: use_count = %d, want 0 -- recall must not bump the search counter", rf.ID, f.UseCount)
		}
	}
}

// Counting has to accumulate, or "how often is this surfaced" is unanswerable.
func TestRecallInjectionAccumulates(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)
	ctx := context.Background()

	const rounds = 3
	var ids []int64
	for range rounds {
		resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
			"prompt": "tell me about the herald feed aggregator",
		})
		var out struct {
			Facts []struct {
				ID int64 `json:"id"`
			} `json:"facts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Facts) == 0 {
			t.Fatal("recall returned no facts")
		}
		if ids == nil {
			for _, rf := range out.Facts {
				ids = append(ids, rf.ID)
			}
		}
	}

	for _, id := range ids {
		f, err := store.Get(ctx, id)
		if err != nil || f == nil {
			t.Fatalf("get fact %d: %v", id, err)
		}
		if f.InjectCount != rounds {
			t.Errorf("fact %d: inject_count = %d after %d recalls, want %d", id, f.InjectCount, rounds, rounds)
		}
	}
}

// The two counters are independent: a search bumps use_count and leaves
// inject_count alone.
func TestSearchDoesNotBumpInjectCount(t *testing.T) {
	_, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)
	ctx := context.Background()

	facts, err := store.List(ctx, memstore.QueryOpts{Subject: "herald", OnlyActive: true})
	if err != nil || len(facts) == 0 {
		t.Fatalf("list herald facts: %v", err)
	}
	id := facts[0].ID

	if err := store.Touch(ctx, []int64{id}); err != nil {
		t.Fatalf("touch: %v", err)
	}

	f, err := store.Get(ctx, id)
	if err != nil || f == nil {
		t.Fatalf("get: %v", err)
	}
	if f.UseCount != 1 {
		t.Errorf("use_count = %d, want 1", f.UseCount)
	}
	if f.InjectCount != 0 {
		t.Errorf("inject_count = %d, want 0 -- Touch must not bump the injection counter", f.InjectCount)
	}
}
