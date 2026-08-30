package pgstore_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"

	"github.com/matthewjhunter/memstore"
)

func seedDoc(t *testing.T, s memstore.DocumentStore, path string, bodies ...string) int64 {
	t.Helper()
	chunks := make([]memstore.DocumentChunk, len(bodies))
	off := 0
	for i, b := range bodies {
		chunks[i] = memstore.DocumentChunk{
			Ordinal: i, Content: b, ByteStart: off, ByteEnd: off + len(b),
			LineStart: i + 1, LineEnd: i + 1, HeadingPath: "Results",
		}
		off += len(b)
	}
	id, err := s.UpsertDocument(context.Background(), memstore.Document{
		Path: path, Basename: path, Lang: "markdown",
		FileSHA256: bytes.Repeat([]byte{0xcd}, 32), ChunkerVersion: 1, ChunkStrategy: "markdown",
	}, chunks)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Document search was keyword-only because chunks carried no vector at all.
// This is the queue side of fixing that: the same unembedded-then-embedded
// lifecycle the fact side has had since V1.
func TestChunksNeedingEmbedding(t *testing.T) {
	s := newTestStore(t)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("backend carries no document corpus")
	}
	es, ok := any(s).(memstore.DocumentEmbedStore)
	if !ok {
		t.Fatal("PostgresStore must implement DocumentEmbedStore")
	}
	seedDoc(t, ds, "papers/chunking.md", "Recall is flat.", "Precision rises.")

	pending, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d pending chunks, want 2", len(pending))
	}
	// The basename travels with the chunk because ChunkEmbedText needs it for
	// the header, and a second query per chunk to fetch it would be silly.
	// Path, not basename: basenames collide, and "chunking.md" locates a
	// chunk in no particular document.
	if pending[0].DocPath != "papers/chunking.md" {
		t.Errorf("DocPath = %q; the embed header cannot be built without it", pending[0].DocPath)
	}
	if pending[0].Chunk.HeadingPath != "Results" {
		t.Errorf("structural fields did not travel: %+v", pending[0].Chunk)
	}

	// Embedding one removes it from the queue and leaves the other.
	if err := es.SetChunkVector(context.Background(), pending[0].Chunk.ID, testVector()); err != nil {
		t.Fatal(err)
	}
	again, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Chunk.ID != pending[1].Chunk.ID {
		t.Errorf("after embedding one, queue = %d chunks, want just the other", len(again))
	}
}

// A chunk the embedder cannot handle must leave the queue, or it is handed
// back every poll forever and nothing behind it is ever embedded -- the same
// failure the fact side's embed_failed_at exists to prevent.
func TestMarkChunkEmbedFailedQuarantines(t *testing.T) {
	s := newTestStore(t)
	ds, _ := any(s).(memstore.DocumentStore)
	es, ok := any(s).(memstore.DocumentEmbedStore)
	if !ok {
		t.Skip("no document embed store")
	}
	seedDoc(t, ds, "bad.md", "unembeddable")

	pending, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	if err := es.MarkChunkEmbedFailed(context.Background(), pending[0].Chunk.ID, "input too long"); err != nil {
		t.Fatal(err)
	}
	again, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("quarantined chunk still queued: %d", len(again))
	}
}

// Re-ingest replaces the chunk set, so the new chunks arrive unembedded and
// the old vectors go with the rows they belonged to. Nothing should carry a
// stale vector across a content change.
func TestReingestRequeuesChunks(t *testing.T) {
	s := newTestStore(t)
	ds, _ := any(s).(memstore.DocumentStore)
	es, ok := any(s).(memstore.DocumentEmbedStore)
	if !ok {
		t.Skip("no document embed store")
	}
	seedDoc(t, ds, "moving.md", "first version")
	p, _ := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err := es.SetChunkVector(context.Background(), p[0].Chunk.ID, testVector()); err != nil {
		t.Fatal(err)
	}
	if q, _ := es.ChunksNeedingEmbedding(context.Background(), 10); len(q) != 0 {
		t.Fatalf("queue not drained: %d", len(q))
	}

	seedDoc(t, ds, "moving.md", "second version")
	q, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || !strings.Contains(q[0].Chunk.Content, "second") {
		t.Errorf("re-ingest did not requeue the new chunk: %+v", q)
	}
}

// testVector is a valid vector at the width the test store is opened with.
func testVector() []float32 { return []float32{0.1, 0.2, 0.3, 0.4} }

// orthEmbedder maps text to fixed orthogonal vectors so a test can assert
// semantic ranking rather than keyword overlap. Deterministic by switch rather
// than by map, because map iteration order made which rule won a coin flip.
//
// Anything that is not the decoy embeds to the answer's direction, including
// the query -- whose rendered form depends on FactQueryText and the model
// name, and is not worth pinning here.
type orthEmbedder struct{}

func (o *orthEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if strings.Contains(t, "chunk size chunk size") {
			out[i] = []float32{0, 1, 0, 0}
			continue
		}
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}
func (o *orthEmbedder) Model() string { return "orth" }
func (o *orthEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "orth", Dim: 4}
}

// The defect this whole task exists for: keyword-only ranking puts prose that
// repeats the query terms above prose that answers the query. Here the decoy
// says "chunk size chunk size chunk size" and the answer says it once, in a
// sentence that means it. FTS alone prefers the decoy; with a vector pass
// fused in, the answer wins.
func TestSearchDocumentChunks_SemanticBeatsKeywordStuffing(t *testing.T) {
	// Both chunks match the query on FTS -- plainto_tsquery ANDs its terms,
	// so the query has to appear in both or the ranking question never
	// arises. The decoy simply repeats it five times.
	answer := "Precision rises fourfold as chunk size falls."
	decoy := "chunk size chunk size chunk size chunk size chunk size"
	emb := &orthEmbedder{}

	s := newTestStoreWithEmbedder(t, emb, 4, 0)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("no document corpus")
	}
	seedDoc(t, ds, "paper.md", answer, decoy)

	es, _ := any(s).(memstore.DocumentEmbedStore)
	pending, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		vecs, err := emb.Embed(context.Background(), []string{p.Chunk.Content})
		if err != nil {
			t.Fatal(err)
		}
		if err := es.SetChunkVector(context.Background(), p.Chunk.ID, vecs[0]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ds.SearchDocumentChunks(context.Background(), "chunk size",
		memstore.DocumentSearchOpts{MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no results")
	}
	if !strings.Contains(got[0].Chunk.Content, "Precision rises") {
		t.Errorf("keyword stuffing still outranks the answer; top hit was:\n%s", got[0].Chunk.Content)
	}
}

// A corpus with no vectors yet must still search. Embedding is a background
// pass, so every chunk is unembedded for some window after ingest, and a
// hybrid path that returned nothing until the queue caught up would be a
// regression on the keyword-only behaviour it replaces.
func TestSearchDocumentChunks_WorksBeforeEmbeddingCatchesUp(t *testing.T) {
	s := newTestStore(t)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("no document corpus")
	}
	seedDoc(t, ds, "fresh.md", "Precision rises as chunks shrink.")

	got, err := ds.SearchDocumentChunks(context.Background(), "precision chunks shrink",
		memstore.DocumentSearchOpts{MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("unembedded corpus returned %d results, want 1 from the FTS pass", len(got))
	}
}
