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
	seen []string
	err  error
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.seen = append(r.seen, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{float32(i), 1}
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

	chunks, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
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

	chunks, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
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

	chunks, err := memstore.EmbedFact(context.Background(), e, chunkModel, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if c.ByteStart < 0 || c.ByteEnd > len(f.Content) || c.ByteStart >= c.ByteEnd {
			t.Errorf("chunk %d span [%d,%d) is not inside the %d-byte content",
				c.Ordinal, c.ByteStart, c.ByteEnd, len(f.Content))
		}
	}
	if last := chunks[len(chunks)-1]; last.ByteEnd < len(f.Content)-ChunkSlack {
		t.Errorf("last chunk ends at %d, leaving %d bytes of a %d-byte fact unindexed",
			last.ByteEnd, len(f.Content)-last.ByteEnd, len(f.Content))
	}
}

// A half-embedded fact would look complete to the queue and leave a permanent
// hole in the middle of it.
func TestEmbedFact_FailureIsAllOrNothing(t *testing.T) {
	e := &recordingEmbedder{err: errors.New("backend down")}

	chunks, err := memstore.EmbedFact(context.Background(), e, chunkModel, longFact(), 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(chunks) != 0 {
		t.Errorf("got %d chunks alongside an error; a partial set must never be stored", len(chunks))
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
