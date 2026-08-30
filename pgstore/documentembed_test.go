package pgstore_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
