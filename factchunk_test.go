package memstore_test

import (
	"strings"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

const chunkModel = "nomic-embed-text"

// Most facts are short. They must stay one vector, or chunking would multiply
// the corpus for no retrieval gain.
func TestChunkFact_ShortFactStaysWhole(t *testing.T) {
	content := "Matthew prefers ASCII punctuation in everything Claude writes."

	chunks := memstore.ChunkFact(chunkModel, content, 0)

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks for a %d-byte fact, want 1", len(chunks), len(content))
	}
	if chunks[0].Text != content {
		t.Errorf("chunk text %q != content", chunks[0].Text)
	}
}

// The point of the change: chunks are sized for retrieval, not to fill the
// model's context window. nomic-embed-text is registered at 6000 bytes, so a
// 3000-byte fact would be a single vector if the budget drove sizing.
func TestChunkFact_TargetIsWellUnderTheModelBudget(t *testing.T) {
	budget := embedding.LookupLimits(chunkModel).MaxBytes
	if budget < 3000 {
		t.Fatalf("test assumes a model budget over 3000 bytes, got %d", budget)
	}
	content := strings.Repeat("The retry budget needs careful tuning. ", 80) // ~3000 bytes

	chunks := memstore.ChunkFact(chunkModel, content, 0)

	if len(chunks) < 2 {
		t.Fatalf("a %d-byte fact produced %d chunk(s); it fits the %d-byte model budget, "+
			"which is exactly the sizing this is meant not to use", len(content), len(chunks), budget)
	}
	for i, c := range chunks {
		if len(c.Text) > memstore.ChunkTargetBytes {
			t.Errorf("chunk %d is %d bytes, over the %d-byte target", i, len(c.Text), memstore.ChunkTargetBytes)
		}
	}
}

// The target is a retrieval choice; the ceiling is a backend constraint. When
// the backend is stricter than the target, the backend has to win or requests
// get rejected.
func TestChunkFact_CeilingClampsTheTarget(t *testing.T) {
	content := strings.Repeat("The retry budget needs careful tuning. ", 80)
	const ceiling = 400

	chunks := memstore.ChunkFact(chunkModel, content, ceiling)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	for i, c := range chunks {
		if len(c.Text) > ceiling {
			t.Errorf("chunk %d is %d bytes, over the %d-byte ceiling", i, len(c.Text), ceiling)
		}
	}
}

// A ceiling above the target must not inflate chunks: that is the herald
// failure mode (raising the backend limit silently widens chunks).
func TestChunkFact_GenerousCeilingDoesNotInflateChunks(t *testing.T) {
	content := strings.Repeat("The retry budget needs careful tuning. ", 80)

	tight := memstore.ChunkFact(chunkModel, content, memstore.ChunkTargetBytes*4)
	none := memstore.ChunkFact(chunkModel, content, 0)

	if len(tight) != len(none) {
		t.Errorf("a ceiling of %d produced %d chunks but no ceiling produced %d; "+
			"the ceiling is raising the target", memstore.ChunkTargetBytes*4, len(tight), len(none))
	}
}

// Offsets are provenance: a chunk hit has to resolve back to a span of the
// fact. Overlap makes them impossible to recompute, so they must be exact.
func TestChunkFact_OffsetsAddressTheSourceExactly(t *testing.T) {
	content := strings.Repeat("The retry budget needs careful tuning. ", 80)

	for i, c := range memstore.ChunkFact(chunkModel, content, 0) {
		if c.Start < 0 || c.End > len(content) || c.Start > c.End {
			t.Fatalf("chunk %d has out-of-range span [%d,%d) for %d bytes", i, c.Start, c.End, len(content))
		}
		if got := content[c.Start:c.End]; got != c.Text {
			t.Errorf("chunk %d: content[%d:%d] != Text", i, c.Start, c.End)
		}
		if c.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, c.Ordinal)
		}
	}
}

func TestChunkFact_EmptyContentYieldsNothing(t *testing.T) {
	if got := memstore.ChunkFact(chunkModel, "   \n ", 0); len(got) != 0 {
		t.Errorf("got %d chunks for whitespace-only content", len(got))
	}
}
