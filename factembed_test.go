package memstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// recordingEmbedder captures the exact texts sent to the backend.
type recordingEmbedder struct {
	seen  []string
	calls int
	err   error
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	r.seen = append(r.seen, texts...)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(i + 1), float32(len(t))}
	}
	return out, nil
}
func (r *recordingEmbedder) Model() string { return chunkModel }
func (r *recordingEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: chunkModel}
}

func longFact() memstore.Fact {
	return memstore.Fact{
		Subject: "deployment",
		Content: strings.Repeat("The retry budget needs careful tuning. ", 80),
	}
}

func TestEmbedFact_ShortFactSendsSubjectAndContent(t *testing.T) {
	e := &recordingEmbedder{}
	f := memstore.Fact{Subject: "matthew", Content: "Prefers ASCII punctuation everywhere."}

	vecs, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs.Chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(vecs.Chunks))
	}
	if len(e.seen) != 1 || !strings.Contains(e.seen[0], "matthew") || !strings.Contains(e.seen[0], "ASCII") {
		t.Errorf("embedded text %q does not carry both subject and content", e.seen)
	}
}

// The lesson chunking usually gets wrong: split the rendered text and chunk 0
// keeps the subject while chunks 1..N are anonymous prose. Every chunk must
// carry it.
func TestEmbedFact_EveryChunkCarriesTheSubject(t *testing.T) {
	e := &recordingEmbedder{}
	f := longFact()

	vecs, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs.Chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(vecs.Chunks))
	}
	for i, text := range e.seen {
		if !strings.Contains(text, f.Subject) {
			t.Errorf("chunk %d was embedded without its subject: %q", i, text[:min(60, len(text))])
		}
	}
}

// Offsets address Fact.Content, not the rendered text. Rendering adds a header
// per chunk, so offsets taken from the rendered string would drift further out
// with every chunk.
func TestEmbedFact_OffsetsAddressContentNotRenderedText(t *testing.T) {
	e := &recordingEmbedder{}
	f := longFact()

	vecs, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range vecs.Chunks {
		if c.ByteStart < 0 || c.ByteEnd > len(f.Content) || c.ByteStart >= c.ByteEnd {
			t.Errorf("chunk %d span [%d,%d) is not inside the %d-byte content",
				c.Ordinal, c.ByteStart, c.ByteEnd, len(f.Content))
		}
	}
	if last := vecs.Chunks[len(vecs.Chunks)-1]; last.ByteEnd < len(f.Content)-ChunkSlack {
		t.Errorf("last chunk ends at %d, leaving %d bytes of a %d-byte fact unindexed",
			last.ByteEnd, len(f.Content)-last.ByteEnd, len(f.Content))
	}
}

// A half-embedded fact would look complete to the queue and leave a permanent
// hole in the middle of it.
func TestEmbedFact_FailureIsAllOrNothing(t *testing.T) {
	e := &recordingEmbedder{err: errors.New("backend down")}

	vecs, err := memstore.EmbedFact(context.Background(), e, chunkModel, longFact(), 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(vecs.Chunks) != 0 {
		t.Errorf("got %d chunks alongside an error; a partial set must never be stored", len(vecs.Chunks))
	}
}

// The header is charged against the budget, so a ceiling must bound the
// rendered text, not just the body.
func TestEmbedFact_CeilingBoundsTheRenderedText(t *testing.T) {
	e := &recordingEmbedder{}
	const ceiling = 400

	if _, err := memstore.EmbedFact(context.Background(), e, chunkModel, longFact(), ceiling); err != nil {
		t.Fatal(err)
	}
	for i, text := range e.seen {
		if len(text) > ceiling {
			t.Errorf("chunk %d rendered to %d bytes, over the %d-byte ceiling", i, len(text), ceiling)
		}
	}
}

// ChunkSlack tolerates the trailing-sliver merge and whitespace trimming when
// asserting coverage.
const ChunkSlack = 300

// --- The dedup vector ---
//
// Auto-supersession compares a new fact's vector against an existing fact's
// stored marker, and acts destructively at 0.85. Both sides therefore have to
// mean the same thing: a whole-fact vector. Letting the marker be chunk 0 makes
// it an opening-passage vector for anything that splits, which compares low
// against a whole-fact average and silently stops deduping long facts.

func TestEmbedFact_SingleChunkReusesItsVectorAsTheWhole(t *testing.T) {
	e := &recordingEmbedder{}
	f := memstore.Fact{Subject: "matthew", Content: "Prefers ASCII punctuation everywhere."}

	v, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(v.Chunks))
	}
	if len(e.seen) != 1 {
		t.Errorf("sent %d texts for a single-chunk fact, want 1 -- "+
			"chunk 0 already is the whole fact, so no separate embed is needed", len(e.seen))
	}
	if !vecEq(v.Whole, v.Chunks[0].Vector) {
		t.Error("Whole differs from the only chunk's vector")
	}
}

func TestEmbedFact_MultiChunkPoolsAWholeFactVector(t *testing.T) {
	e := &recordingEmbedder{}

	v, err := memstore.EmbedFact(context.Background(), e, chunkModel, longFact(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(v.Chunks))
	}
	if len(v.Whole) == 0 {
		t.Fatal("no whole-fact vector; supersession would have nothing to compare")
	}
	if vecEq(v.Whole, v.Chunks[0].Vector) {
		t.Error("Whole is chunk 0's vector -- for a fact that splits, that is the " +
			"opening passage, not the fact")
	}
}

// Pooling comes from the chunk vectors already in hand, so a fact costs one
// request whether or not it splits.
func TestEmbedFact_WholeFactCostsNoExtraInput(t *testing.T) {
	e := &recordingEmbedder{}

	v, err := memstore.EmbedFact(context.Background(), e, chunkModel, longFact(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Errorf("made %d embed calls, want 1", e.calls)
	}
	if len(e.seen) != len(v.Chunks) {
		t.Errorf("sent %d texts for %d chunks; the whole-fact vector must be pooled, not embedded",
			len(e.seen), len(v.Chunks))
	}
}

func vecEq(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
