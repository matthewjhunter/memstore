package pgstore_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// chunkVectorCount reports how many of a document's chunks carry a vector.
func chunkVectorCount(t *testing.T, s *pgstore.PostgresStore, docID int64) int {
	t.Helper()
	es, ok := any(s).(memstore.DocumentEmbedStore)
	if !ok {
		t.Skip("no document embed store")
	}
	pending, err := es.ChunksNeedingEmbedding(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	ds, _ := any(s).(memstore.DocumentStore)
	all, err := ds.GetDocumentChunks(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	return len(all) - len(pending)
}

// embedAll gives every pending chunk a vector, standing in for the queue.
func embedAll(t *testing.T, s *pgstore.PostgresStore) {
	t.Helper()
	es, _ := any(s).(memstore.DocumentEmbedStore)
	pending, err := es.ChunksNeedingEmbedding(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if err := es.SetChunkVector(context.Background(), p.Chunk.ID, testVector()); err != nil {
			t.Fatal(err)
		}
	}
}

// Re-ingest replaces a document's whole chunk set, so every chunk of a
// changed file was re-embedded even when its own bytes never moved. On a repo
// sync that is most of the embedding work in the run, and it is pure waste:
// same text and same header render the same input to the same model.
func TestUpsertDocumentReusesVectorsForUnchangedChunks(t *testing.T) {
	s := newTestStore(t)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("no document corpus")
	}
	id := seedDoc(t, ds, "paper.md", "First section, unchanged.", "Second section, will change.")
	embedAll(t, s)
	if n := chunkVectorCount(t, s, id); n != 2 {
		t.Fatalf("setup: %d chunks embedded, want 2", n)
	}

	// Re-ingest with the second chunk edited.
	id2 := seedDoc(t, ds, "paper.md", "First section, unchanged.", "Second section, now different.")
	if id2 != id {
		t.Fatalf("re-ingest created a new document (%d -> %d)", id, id2)
	}

	es, _ := any(s).(memstore.DocumentEmbedStore)
	pending, err := es.ChunksNeedingEmbedding(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d chunks need embedding after re-ingest, want 1 (only the edited one)", len(pending))
	}
	if pending[0].Chunk.Content != "Second section, now different." {
		t.Errorf("wrong chunk requeued: %q", pending[0].Chunk.Content)
	}
}

// Reuse is keyed on what actually reaches the embedder, not on content alone.
// ChunkEmbedText puts the section heading and the symbol into the header, so a
// chunk whose text is unchanged but whose heading moved renders different
// input and must be embedded again. Matching on content alone would silently
// keep a vector for text the model never saw in that form.
func TestUpsertDocumentDoesNotReuseWhenTheHeaderChanges(t *testing.T) {
	s := newTestStore(t)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("no document corpus")
	}
	body := "The same words in both versions."
	mk := func(heading string) []memstore.DocumentChunk {
		return []memstore.DocumentChunk{{
			Ordinal: 0, Content: body, ByteStart: 0, ByteEnd: len(body),
			LineStart: 1, LineEnd: 1, HeadingPath: heading,
		}}
	}
	doc := memstore.Document{
		Path: "moved.md", Basename: "moved.md", Lang: "markdown",
		FileSHA256: make([]byte, 32), ChunkerVersion: 1, ChunkStrategy: "markdown",
	}
	for i := range doc.FileSHA256 {
		doc.FileSHA256[i] = 0x22
	}
	if _, err := ds.UpsertDocument(context.Background(), doc, mk("Introduction")); err != nil {
		t.Fatal(err)
	}
	embedAll(t, s)

	if _, err := ds.UpsertDocument(context.Background(), doc, mk("Conclusion")); err != nil {
		t.Fatal(err)
	}
	es, _ := any(s).(memstore.DocumentEmbedStore)
	pending, err := es.ChunksNeedingEmbedding(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("heading changed but the vector was reused; %d chunks requeued, want 1", len(pending))
	}
}

// reset-embeddings promises "every stored vector". Document chunks were added
// after it was written, so it cleared the fact side and left document vectors
// behind -- in the old model's space, which is precisely the silently
// degraded ranking the fingerprint check exists to prevent.
func TestResetEmbeddingsClearsDocumentChunks(t *testing.T) {
	s := newTestStore(t)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("no document corpus")
	}
	id := seedDoc(t, ds, "doc.md", "alpha", "beta")
	embedAll(t, s)
	if n := chunkVectorCount(t, s, id); n != 2 {
		t.Fatalf("setup: %d embedded, want 2", n)
	}

	pool, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cleared, err := pgstore.ResetEmbeddings(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if cleared < 2 {
		t.Errorf("cleared = %d, want at least the 2 document chunk vectors counted", cleared)
	}
	if n := chunkVectorCount(t, s, id); n != 0 {
		t.Errorf("%d document chunk vectors survived reset-embeddings", n)
	}
}

// Reuse must never cross documents. Two files can hold identical text under
// identical headings, but ChunkEmbedText puts the document path in the header,
// so their embedder inputs differ and their vectors are not interchangeable.
// The query is document-scoped and the key carries the path, and this pins
// both.
func TestUpsertDocumentDoesNotReuseAcrossDocuments(t *testing.T) {
	s := newTestStore(t)
	ds, ok := any(s).(memstore.DocumentStore)
	if !ok {
		t.Skip("no document corpus")
	}
	shared := "Identical prose in two different files."
	seedDoc(t, ds, "first.md", shared)
	embedAll(t, s)

	seedDoc(t, ds, "second.md", shared)
	es, _ := any(s).(memstore.DocumentEmbedStore)
	pending, err := es.ChunksNeedingEmbedding(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].DocPath != "second.md" {
		t.Errorf("second document's chunk should need its own vector; pending = %d", len(pending))
	}
}
