package memstore_test

import (
	"context"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// ReadOnly withholds writes; it must not withhold reads. Document search is a
// read, and a read server that cannot reach the corpus is most of production
// -- so the narrowing has to let the search capability through while the
// write half stays unreachable.
func TestDocumentSearcherOfSeesThroughReadOnly(t *testing.T) {
	full := &fakeDocStore{}
	ro := memstore.ReadOnly(full)

	ds, ok := memstore.DocumentSearcherOf(ro)
	if !ok {
		t.Fatal("read-only handle hides document search; a read server could not use the corpus")
	}
	if _, err := ds.SearchDocumentChunks(context.Background(), "q", memstore.DocumentSearchOpts{}); err != nil {
		t.Fatal(err)
	}
	if !full.searched {
		t.Error("search did not reach the backing store")
	}

	// The escape hatch must stay shut: what comes back has no write methods
	// to assert to, so it cannot be a route to UpsertDocument.
	if _, isFull := any(ds).(memstore.DocumentStore); isFull {
		t.Error("DocumentSearcherOf handed back the full DocumentStore, restoring the writes ReadOnly removed")
	}
}

func TestDocumentSearcherOfReportsNoCorpus(t *testing.T) {
	if _, ok := memstore.DocumentSearcherOf(struct{}{}); ok {
		t.Error("a backend with no corpus reported one")
	}
}

// fakeDocStore is a DocumentStore that records only whether search ran.
type fakeDocStore struct {
	memstore.ReadableStore
	searched bool
}

func (f *fakeDocStore) UpsertDocument(context.Context, memstore.Document, []memstore.DocumentChunk) (int64, error) {
	return 0, nil
}
func (f *fakeDocStore) ListDocuments(context.Context, string) ([]memstore.DocumentInfo, error) {
	return nil, nil
}
func (f *fakeDocStore) DeleteDocuments(context.Context, string, []string) (int64, error) {
	return 0, nil
}
func (f *fakeDocStore) GetDocument(context.Context, int64) (*memstore.Document, error) {
	return nil, nil
}
func (f *fakeDocStore) GetDocumentChunks(context.Context, int64) ([]memstore.DocumentChunk, error) {
	return nil, nil
}
func (f *fakeDocStore) SearchDocumentChunks(context.Context, string, memstore.DocumentSearchOpts) ([]memstore.DocumentSearchResult, error) {
	f.searched = true
	return nil, nil
}
