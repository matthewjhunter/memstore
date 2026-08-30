package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	embedding "github.com/matthewjhunter/go-embedding"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/teststore"
)

// recordingEmbedder captures what was sent to the model, which is the only way
// to assert the structural header actually reaches it.
type recordingEmbedder struct {
	dim  int
	seen []string
	err  error
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.seen = append(r.seen, texts...)
	if r.err != nil {
		return nil, r.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, r.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}
func (r *recordingEmbedder) Model() string { return "mock" }
func (r *recordingEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "mock", Dim: r.dim}
}

func upsertChunk(t *testing.T, store teststore.Store, path, body string) {
	t.Helper()
	ds, ok := any(store).(memstore.DocumentStore)
	if !ok {
		t.Skip("backend carries no document corpus")
	}
	_, err := ds.UpsertDocument(context.Background(), memstore.Document{
		Path: path, Basename: path, Lang: "markdown",
		FileSHA256: bytes.Repeat([]byte{0x11}, 32), ChunkerVersion: 1, ChunkStrategy: "markdown",
	}, []memstore.DocumentChunk{
		{Ordinal: 0, Content: body, ByteEnd: len(body), LineStart: 1, LineEnd: 1, HeadingPath: "Results"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The queue that already embeds facts must also drain document chunks, or the
// corpus stays keyword-only however good the schema is.
func TestEmbedQueue_EmbedsDocumentChunks(t *testing.T) {
	emb := &recordingEmbedder{dim: teststore.VecDim}
	store := teststore.New(t, emb, "test")
	upsertChunk(t, store, "papers/chunking.md", "Recall is flat from 800 tokens down to 200.")

	eq := httpapi.NewEmbedQueue(store, emb, time.Hour, 10)
	eq.ProcessOnce()

	es, ok := any(store).(memstore.DocumentEmbedStore)
	if !ok {
		t.Fatal("store does not implement DocumentEmbedStore")
	}
	left, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d chunks still unembedded after a queue pass", len(left))
	}

	// The structural header has to reach the model, not just exist.
	var sent string
	for _, s := range emb.seen {
		if len(s) > len(sent) {
			sent = s
		}
	}
	for _, want := range []string{"papers/chunking.md", "Results", "Recall is flat"} {
		if !contains(sent, want) {
			t.Errorf("embed text missing %q:\n%s", want, sent)
		}
	}
}

// An empty fact queue must not stop documents being embedded. The fact pass
// returned early on no facts, and folding chunks into that body would have
// made document embedding depend on there being unembedded facts.
func TestEmbedQueue_EmbedsChunksWhenNoFactsPending(t *testing.T) {
	emb := &recordingEmbedder{dim: teststore.VecDim}
	store := teststore.New(t, emb, "test")
	upsertChunk(t, store, "only-docs.md", "no facts here at all")

	eq := httpapi.NewEmbedQueue(store, emb, time.Hour, 10)
	eq.ProcessOnce()

	es, _ := any(store).(memstore.DocumentEmbedStore)
	left, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("chunks left unembedded because no facts were pending: %d", len(left))
	}
}

// A permanently unembeddable chunk must leave the queue, or it is handed back
// every tick and everything behind it starves.
func TestEmbedQueue_QuarantinesPermanentChunkFailure(t *testing.T) {
	emb := &recordingEmbedder{dim: teststore.VecDim, err: &embedding.PermanentError{Err: errors.New("input too long")}}
	store := teststore.New(t, emb, "test")
	upsertChunk(t, store, "bad.md", "unembeddable")

	eq := httpapi.NewEmbedQueue(store, emb, time.Hour, 10)
	eq.ProcessOnce()

	es, _ := any(store).(memstore.DocumentEmbedStore)
	left, err := es.ChunksNeedingEmbedding(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("permanently failing chunk still queued: %d", len(left))
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
