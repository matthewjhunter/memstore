package httpapi

import (
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// TestExtractCitations pins what counts as a citation: the form in the model's
// own prose, outside code.
func TestExtractCitations(t *testing.T) {
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	turns := []memstore.SessionTurn{
		{SessionID: "s", UUID: "u1", Role: "user", Content: "is [fact 11] still true?", CreatedAt: at},
		{SessionID: "s", UUID: "a1", Role: "assistant", Content: "Per [fact 12] and [fact 13], no.", CreatedAt: at},
		{SessionID: "s", UUID: "a2", Role: "assistant", CreatedAt: at.Add(time.Minute),
			Content: "Again [fact 12]. The test quotes `[fact 907]` inline and\n```go\ngood := \"[fact 908]\"\n```\nin a block."},
		{SessionID: "s", UUID: "a3", Role: "assistant", Content: "The form is [fact N].", CreatedAt: at},
	}

	got := extractCitations(turns)
	if len(got) != 2 {
		t.Fatalf("got %d citations, want 2 (12 and 13 from a1): %+v", len(got), got)
	}
	for i, want := range []int64{12, 13} {
		c := got[i]
		if c.FactID != want || c.TurnUUID != "a1" || c.SessionID != "s" || !c.CitedAt.Equal(at) {
			t.Errorf("citation %d = %+v, want fact %d from turn a1 at %v", i, c, want, at)
		}
	}
}

func TestExtractCitationsEmpty(t *testing.T) {
	if got := extractCitations(nil); len(got) != 0 {
		t.Errorf("no turns: %+v", got)
	}
	turns := []memstore.SessionTurn{{Role: "assistant", Content: "Nothing cited here."}}
	if got := extractCitations(turns); len(got) != 0 {
		t.Errorf("no citations: %+v", got)
	}
}
