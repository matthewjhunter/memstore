package memstore_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// A fact's embedding is a daemon-side detail. Marshalling it sends 768
// float32s per fact to a caller that has no use for them -- the client's
// embedding methods are all no-ops and transfer re-embeds after import --
// which made `memstore tasks --format json` 688 KB for 42 tasks, roughly
// 95% vector.
func TestFact_JSONOmitsEmbedding(t *testing.T) {
	b, err := json.Marshal(memstore.Fact{
		ID:        7,
		Content:   "the fact",
		Embedding: []float32{1, 2, 3, 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("Embedding")) {
		t.Errorf("marshalled fact carries its embedding: %s", b)
	}

	// Round-tripping still works for everything a caller does need.
	var got memstore.Fact
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Content != "the fact" {
		t.Errorf("round trip = %+v", got)
	}
	if got.Embedding != nil {
		t.Errorf("embedding survived the round trip: %v", got.Embedding)
	}
}
