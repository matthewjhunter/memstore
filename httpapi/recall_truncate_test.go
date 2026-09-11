package httpapi_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// TestRecallMarksTruncatedFacts pins #215: recall cuts each fact to a per-fact
// budget, and a cut fact must say so. Before this, the content ended in "..."
// inside an intact fence, so a reader following the documented truncation rule
// (a missing closing tag) took a partial fact for a complete one -- and the
// elided remainder can be the qualification that changes its meaning.
//
// The marker has to sit on the header line, outside the fence: that is memstore
// speaking. A marker inside the content would be indistinguishable from stored
// text, and a fact could forge or suppress it.
func TestRecallMarksTruncatedFacts(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)
	ctx := context.Background()

	long := "Herald polls each feed aggregator source hourly. " +
		strings.Repeat("It backs off exponentially when a feed errors, capped at a day. ", 10) +
		"EXCEPT feeds marked priority, which are never backed off."
	longID, err := store.Insert(ctx, memstore.Fact{Content: long, Subject: "herald", Category: "project"})
	if err != nil {
		t.Fatal(err)
	}
	shortID, err := store.Insert(ctx, memstore.Fact{
		Content: "Herald feed aggregator stores entries in Postgres.", Subject: "herald", Category: "project",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{
		"prompt": "tell me about the herald feed aggregator",
		"limit":  10,
	})

	var result struct {
		Context string `json:"context"`
		Facts   []struct {
			ID        int64  `json:"id"`
			Content   string `json:"content"`
			Truncated bool   `json:"truncated"`
		} `json:"facts"`
	}
	decodeJSON(t, resp, &result)

	seen := map[int64]bool{}
	for _, f := range result.Facts {
		seen[f.ID] = true
		switch f.ID {
		case longID:
			if !f.Truncated {
				t.Errorf("fact %d was cut to %d of %d bytes but is not flagged truncated", f.ID, len(f.Content), len(long))
			}
			if strings.Contains(f.Content, "EXCEPT") {
				t.Fatalf("fact %d was not cut; the test is not exercising truncation", f.ID)
			}
		case shortID:
			if f.Truncated {
				t.Errorf("fact %d is complete but flagged truncated", f.ID)
			}
		}
	}
	if !seen[longID] || !seen[shortID] {
		t.Fatalf("recall did not return both facts (long=%d short=%d); got %+v", longID, shortID, result.Facts)
	}

	longHeader := headerLine(t, result.Context, longID)
	if !strings.Contains(longHeader, "truncated") {
		t.Errorf("header of a cut fact does not say it was truncated: %q", longHeader)
	}
	if want := fmt.Sprintf("memory_history id=%d", longID); !strings.Contains(longHeader, want) {
		t.Errorf("header of a cut fact does not say where the full text is (%q): %q", want, longHeader)
	}
	if strings.Contains(longHeader, "<untrusted-") {
		t.Errorf("header line carries a fence tag; the marker must be outside the fence: %q", longHeader)
	}

	if shortHeader := headerLine(t, result.Context, shortID); strings.Contains(shortHeader, "truncated") {
		t.Errorf("header of a complete fact claims truncation: %q", shortHeader)
	}
}

// headerLine returns the line recall writes before fact id's fenced content.
func headerLine(t *testing.T, out string, id int64) string {
	t.Helper()
	prefix := fmt.Sprintf("[id=%d] ", id)
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no header line for fact %d in recall context:\n%s", id, out)
	return ""
}
