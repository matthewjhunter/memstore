package pgstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// docStore narrows a store to the document interface, failing loudly if the
// backend stops implementing it.
func docStore(t *testing.T, s memstore.Store) memstore.DocumentStore {
	t.Helper()
	ds, ok := s.(memstore.DocumentStore)
	if !ok {
		t.Fatalf("%T does not implement memstore.DocumentStore", s)
	}
	return ds
}

func sha(content string) []byte {
	h := sha256.Sum256([]byte(content))
	return h[:]
}

// testDoc builds a minimal valid document for path in repo (repo may be "").
func testDoc(repo, path, content string) memstore.Document {
	return memstore.Document{
		RepoURL:        repo,
		Commit:         "abc123",
		Path:           path,
		Lang:           "markdown",
		FileSHA256:     sha(content),
		ChunkerVersion: 1,
	}
}

// chunksOf cuts content into one chunk per line, which satisfies the span
// invariants (monotonic ordinals, non-overlapping byte ranges) without
// needing a real chunker.
func chunksOf(content string) []memstore.DocumentChunk {
	var chunks []memstore.DocumentChunk
	start := 0
	line := 1
	for _, part := range strings.SplitAfter(content, "\n") {
		if strings.TrimSpace(part) == "" {
			start += len(part)
			line++
			continue
		}
		text := strings.TrimSuffix(part, "\n")
		chunks = append(chunks, memstore.DocumentChunk{
			Ordinal:   len(chunks),
			Content:   text,
			ByteStart: start,
			ByteEnd:   start + len(text),
			LineStart: line,
			LineEnd:   line,
		})
		start += len(part)
		line++
	}
	return chunks
}

func mustUpsert(t *testing.T, ds memstore.DocumentStore, doc memstore.Document, chunks []memstore.DocumentChunk) int64 {
	t.Helper()
	id, err := ds.UpsertDocument(context.Background(), doc, chunks)
	if err != nil {
		t.Fatalf("UpsertDocument(%s): %v", doc.Path, err)
	}
	return id
}

func TestDocuments_UpsertAndGet(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))

	content := "# Design\nThe auth design delegates to webauth.\n"
	doc := testDoc("https://github.com/matthewjhunter/memstore", "docs/design.md", content)
	doc.Title = "Design"
	doc.FrontMatter = []byte(`{"title":"Design"}`)
	doc.Trusted = true
	chunks := chunksOf(content)
	chunks[1].HeadingPath = "Design"
	chunks[1].HeadingLevel = 1

	id := mustUpsert(t, ds, doc, chunks)
	if id == 0 {
		t.Fatal("expected non-zero document id")
	}

	got, err := ds.GetDocument(ctx, id)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if got == nil {
		t.Fatal("GetDocument returned nil for a stored document")
	}
	if got.Basename != "design.md" {
		t.Errorf("basename not derived from path: got %q", got.Basename)
	}
	if got.RepoURL != doc.RepoURL || got.Commit != "abc123" || got.Lang != "markdown" {
		t.Errorf("identity fields mangled: %+v", got)
	}
	if !bytes.Equal(got.FileSHA256, doc.FileSHA256) {
		t.Errorf("file_sha256 mangled")
	}
	if !got.Trusted || got.ChunkerVersion != 1 || got.Title != "Design" {
		t.Errorf("provenance fields mangled: %+v", got)
	}
	if got.IngestedAt.IsZero() {
		t.Error("ingested_at not stamped")
	}
	if got.UserID == 0 {
		t.Error("user_id not stamped from store scope")
	}

	stored, err := ds.GetDocumentChunks(ctx, id)
	if err != nil {
		t.Fatalf("GetDocumentChunks: %v", err)
	}
	if len(stored) != len(chunks) {
		t.Fatalf("chunk count: got %d want %d", len(stored), len(chunks))
	}
	for i, c := range stored {
		if c.Ordinal != i {
			t.Errorf("chunk %d: ordinal %d", i, c.Ordinal)
		}
		if c.Content != chunks[i].Content {
			t.Errorf("chunk %d content mangled: %q", i, c.Content)
		}
		if c.Content != content[c.ByteStart:c.ByteEnd] {
			t.Errorf("chunk %d violates the span invariant", i)
		}
	}
	if stored[1].HeadingPath != "Design" || stored[1].HeadingLevel != 1 {
		t.Errorf("derived context mangled: %+v", stored[1])
	}
}

func TestDocuments_GetDocumentMissing(t *testing.T) {
	ds := docStore(t, newTestStore(t))
	got, err := ds.GetDocument(context.Background(), 999999)
	if err != nil {
		t.Fatalf("GetDocument on missing id: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing document, got %+v", got)
	}
}

// Re-ingesting the same (repo, path) replaces the row and its chunk set;
// commit is deliberately outside the uniqueness key.
func TestDocuments_ReplaceOnReingest(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	const repo = "https://github.com/matthewjhunter/memstore"

	v1 := "old content line one\nold content line two\nold line three\n"
	id1 := mustUpsert(t, ds, testDoc(repo, "README.md", v1), chunksOf(v1))

	v2 := "new content\n"
	doc2 := testDoc(repo, "README.md", v2)
	doc2.Commit = "def456"
	id2 := mustUpsert(t, ds, doc2, chunksOf(v2))

	if id1 != id2 {
		t.Errorf("re-ingest allocated a new document id (%d -> %d); replace should keep it", id1, id2)
	}

	infos, err := ds.ListDocuments(ctx, repo)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 document after re-ingest, got %d", len(infos))
	}
	if !bytes.Equal(infos[0].FileSHA256, sha(v2)) {
		t.Error("manifest sha not updated on re-ingest")
	}

	got, err := ds.GetDocument(ctx, id2)
	if err != nil || got == nil {
		t.Fatalf("GetDocument after re-ingest: %v, %v", got, err)
	}
	if got.Commit != "def456" {
		t.Errorf("commit not updated: %q", got.Commit)
	}

	chunks, err := ds.GetDocumentChunks(ctx, id2)
	if err != nil {
		t.Fatalf("GetDocumentChunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Content != "new content" {
		t.Fatalf("old chunk set not replaced: %+v", chunks)
	}
}

// The partial-unique-index regression test: Postgres treats NULLs as
// distinct, so without the WHERE repo_url IS NULL index, loose-file
// re-ingest accumulates rows instead of replacing (docs/document-ingest.md).
func TestDocuments_LooseFileReplace(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))

	v1 := "loose file first version\n"
	id1 := mustUpsert(t, ds, testDoc("", "notes/todo.md", v1), chunksOf(v1))
	v2 := "loose file second version\n"
	id2 := mustUpsert(t, ds, testDoc("", "notes/todo.md", v2), chunksOf(v2))

	if id1 != id2 {
		t.Errorf("loose-file re-ingest allocated a new id (%d -> %d)", id1, id2)
	}
	infos, err := ds.ListDocuments(ctx, "")
	if err != nil {
		t.Fatalf("ListDocuments(loose): %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("loose-file re-ingest accumulated %d rows; want 1", len(infos))
	}
}

// The same path may exist both as a repo file and a loose file; they are
// distinct documents.
func TestDocuments_LooseAndRepoCoexist(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	const repo = "https://github.com/matthewjhunter/memstore"

	c := "same path, different identity\n"
	mustUpsert(t, ds, testDoc(repo, "README.md", c), chunksOf(c))
	mustUpsert(t, ds, testDoc("", "README.md", c), chunksOf(c))

	repoDocs, err := ds.ListDocuments(ctx, repo)
	if err != nil {
		t.Fatalf("ListDocuments(repo): %v", err)
	}
	looseDocs, err := ds.ListDocuments(ctx, "")
	if err != nil {
		t.Fatalf("ListDocuments(loose): %v", err)
	}
	if len(repoDocs) != 1 || len(looseDocs) != 1 {
		t.Fatalf("repo/loose identities collided: repo=%d loose=%d", len(repoDocs), len(looseDocs))
	}
}

func TestDocuments_DeleteDocuments(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	const repo = "https://github.com/matthewjhunter/memstore"

	c := "content\n"
	keep := mustUpsert(t, ds, testDoc(repo, "keep.md", c), chunksOf(c))
	gone := mustUpsert(t, ds, testDoc(repo, "gone.md", c), chunksOf(c))

	n, err := ds.DeleteDocuments(ctx, repo, []string{"gone.md", "never-existed.md"})
	if err != nil {
		t.Fatalf("DeleteDocuments: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d documents, want 1", n)
	}

	if got, err := ds.GetDocument(ctx, gone); err != nil || got != nil {
		t.Errorf("deleted document still visible: %+v, %v", got, err)
	}
	if got, err := ds.GetDocument(ctx, keep); err != nil || got == nil {
		t.Errorf("unrelated document deleted: %+v, %v", got, err)
	}
	// Chunks cascade with the document.
	chunks, err := ds.GetDocumentChunks(ctx, gone)
	if err != nil {
		t.Fatalf("GetDocumentChunks after delete: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks survived document deletion: %d", len(chunks))
	}
}

func TestDocuments_UpsertValidation(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	c := "content line\n"

	t.Run("empty path", func(t *testing.T) {
		doc := testDoc("", "", c)
		if _, err := ds.UpsertDocument(ctx, doc, chunksOf(c)); err == nil {
			t.Error("expected error for empty path")
		}
	})
	t.Run("bad sha length", func(t *testing.T) {
		doc := testDoc("", "a.md", c)
		doc.FileSHA256 = []byte("short")
		if _, err := ds.UpsertDocument(ctx, doc, chunksOf(c)); err == nil {
			t.Error("expected error for non-32-byte sha256")
		}
	})
	t.Run("non-dense ordinals", func(t *testing.T) {
		chunks := chunksOf(c)
		chunks[0].Ordinal = 5
		if _, err := ds.UpsertDocument(ctx, testDoc("", "b.md", c), chunks); err == nil {
			t.Error("expected error for non-dense ordinals")
		}
	})
	t.Run("overlapping spans", func(t *testing.T) {
		two := "first line here\nsecond line here\n"
		chunks := chunksOf(two)
		chunks[1].ByteStart = chunks[0].ByteEnd - 3
		if _, err := ds.UpsertDocument(ctx, testDoc("", "c.md", two), chunks); err == nil {
			t.Error("expected error for overlapping spans")
		}
	})
	t.Run("inverted span", func(t *testing.T) {
		chunks := chunksOf(c)
		chunks[0].ByteEnd = chunks[0].ByteStart
		if _, err := ds.UpsertDocument(ctx, testDoc("", "d.md", c), chunks); err == nil {
			t.Error("expected error for empty/inverted span")
		}
	})
}

func TestDocuments_SearchExact(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	const repo = "https://github.com/matthewjhunter/memstore"

	content := "The authentication design delegates every federation decision to webauth.\n"
	doc := testDoc(repo, "docs/auth.md", content)
	doc.Trusted = true
	doc.Dirty = true
	mustUpsert(t, ds, doc, chunksOf(content))

	results, err := ds.SearchDocumentChunks(ctx, "federation decision", memstore.DocumentSearchOpts{})
	if err != nil {
		t.Fatalf("SearchDocumentChunks: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Fallback {
		t.Error("exact match reported as fallback")
	}
	if r.Score <= 0 {
		t.Error("expected positive score")
	}
	if !r.Trusted || !r.Dirty {
		t.Errorf("trust/dirty flags did not travel: %+v", r)
	}
	if r.Path != "docs/auth.md" || r.Basename != "auth.md" || r.RepoURL != repo {
		t.Errorf("citation identity mangled: %+v", r)
	}
	if want := repo + "@abc123 docs/auth.md:L1-1"; r.Citation() != want {
		t.Errorf("Citation() = %q, want %q", r.Citation(), want)
	}
}

// The measured decomposed-with-fallback design: a query like "sqlite.go"
// finds nothing exactly (the english tsvector token for a mention of
// "memstore/sqlite.go" is the whole path), so the decomposed fallback fires
// and appends hits ranked below any exact ones.
func TestDocuments_SearchDecomposedFallback(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	const repo = "https://github.com/matthewjhunter/memstore"

	content := "The single-file backend lives in memstore/sqlite.go for local use.\n"
	mustUpsert(t, ds, testDoc(repo, "docs/backends.md", content), chunksOf(content))

	results, err := ds.SearchDocumentChunks(ctx, "sqlite.go", memstore.DocumentSearchOpts{})
	if err != nil {
		t.Fatalf("SearchDocumentChunks: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("decomposed fallback did not fire; identifier query found nothing")
	}
	if !results[0].Fallback {
		t.Error("fallback hit not marked Fallback")
	}

	// A query that hits exactly must not fire the fallback flag.
	exact, err := ds.SearchDocumentChunks(ctx, "single-file backend", memstore.DocumentSearchOpts{})
	if err != nil {
		t.Fatalf("SearchDocumentChunks(exact): %v", err)
	}
	if len(exact) == 0 || exact[0].Fallback {
		t.Errorf("exact query mis-ranked: %+v", exact)
	}
}

func TestDocuments_SearchFilters(t *testing.T) {
	ctx := context.Background()
	ds := docStore(t, newTestStore(t))
	repoA := "https://github.com/matthewjhunter/memstore"
	repoB := "https://github.com/matthewjhunter/majordomo"

	c1 := "shared keyword xylophone in memstore\n"
	c2 := "shared keyword xylophone in majordomo\n"
	mustUpsert(t, ds, testDoc(repoA, "docs/a.md", c1), chunksOf(c1))
	mustUpsert(t, ds, testDoc(repoB, "notes/b.md", c2), chunksOf(c2))

	gen := "shared keyword xylophone in generated code\n"
	genDoc := testDoc(repoA, "gen/zz_generated.md", gen)
	genDoc.Generated = true
	mustUpsert(t, ds, genDoc, chunksOf(gen))

	t.Run("unfiltered excludes generated", func(t *testing.T) {
		results, err := ds.SearchDocumentChunks(ctx, "xylophone", memstore.DocumentSearchOpts{})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("got %d results, want 2 (generated excluded by default)", len(results))
		}
	})
	t.Run("IncludeGenerated", func(t *testing.T) {
		results, err := ds.SearchDocumentChunks(ctx, "xylophone", memstore.DocumentSearchOpts{IncludeGenerated: true})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("got %d results, want 3 with IncludeGenerated", len(results))
		}
	})
	t.Run("repo filter", func(t *testing.T) {
		results, err := ds.SearchDocumentChunks(ctx, "xylophone", memstore.DocumentSearchOpts{RepoURL: repoB})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 1 || results[0].RepoURL != repoB {
			t.Fatalf("repo filter leaked: %+v", results)
		}
	})
	t.Run("basename filter", func(t *testing.T) {
		results, err := ds.SearchDocumentChunks(ctx, "xylophone", memstore.DocumentSearchOpts{Basename: "a.md"})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 1 || results[0].Basename != "a.md" {
			t.Fatalf("basename filter leaked: %+v", results)
		}
	})
	t.Run("path prefix filter", func(t *testing.T) {
		results, err := ds.SearchDocumentChunks(ctx, "xylophone", memstore.DocumentSearchOpts{PathPrefix: "notes/"})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(results) != 1 || results[0].Path != "notes/b.md" {
			t.Fatalf("path prefix filter leaked: %+v", results)
		}
	})
}

// Documents are user-scoped: every read and write path carries the owner
// predicate, matching the fact-side isolation battery.
func TestDocuments_UserIsolation(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	uidA, err := pgstore.EnsureUser(ctx, pool, "test", "doc-iso-a")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	uidB, err := pgstore.EnsureUser(ctx, pool, "test", "doc-iso-b")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	sa, err := base.ForUser(uidA)
	if err != nil {
		t.Fatalf("ForUser(A): %v", err)
	}
	sb, err := base.ForUser(uidB)
	if err != nil {
		t.Fatalf("ForUser(B): %v", err)
	}
	a, b := docStore(t, sa), docStore(t, sb)
	const repo = "https://github.com/matthewjhunter/memstore"

	content := "user A's private corpus content mentioning quixotic\n"
	docID := mustUpsert(t, a, testDoc(repo, "private.md", content), chunksOf(content))

	t.Run("GetDocument invisible cross-user", func(t *testing.T) {
		got, err := b.GetDocument(ctx, docID)
		if err != nil {
			t.Fatalf("GetDocument: %v", err)
		}
		if got != nil {
			t.Fatalf("user B can read user A's document: %+v", got)
		}
	})
	t.Run("GetDocumentChunks invisible cross-user", func(t *testing.T) {
		chunks, err := b.GetDocumentChunks(ctx, docID)
		if err != nil {
			t.Fatalf("GetDocumentChunks: %v", err)
		}
		if len(chunks) != 0 {
			t.Fatalf("user B can read user A's chunks: %d", len(chunks))
		}
	})
	t.Run("ListDocuments invisible cross-user", func(t *testing.T) {
		infos, err := b.ListDocuments(ctx, repo)
		if err != nil {
			t.Fatalf("ListDocuments: %v", err)
		}
		if len(infos) != 0 {
			t.Fatalf("user B sees user A's manifest: %+v", infos)
		}
	})
	t.Run("Search invisible cross-user", func(t *testing.T) {
		results, err := b.SearchDocumentChunks(ctx, "quixotic", memstore.DocumentSearchOpts{})
		if err != nil {
			t.Fatalf("SearchDocumentChunks: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("user B can search user A's corpus: %+v", results)
		}
	})
	t.Run("Delete cannot cross users", func(t *testing.T) {
		n, err := b.DeleteDocuments(ctx, repo, []string{"private.md"})
		if err != nil {
			t.Fatalf("DeleteDocuments: %v", err)
		}
		if n != 0 {
			t.Fatalf("user B deleted %d of user A's documents", n)
		}
		if got, err := a.GetDocument(ctx, docID); err != nil || got == nil {
			t.Fatalf("user A's document damaged by cross-user delete: %+v, %v", got, err)
		}
	})
	t.Run("same path is a distinct document per user", func(t *testing.T) {
		other := "user B's own file at the same path\n"
		mustUpsert(t, b, testDoc(repo, "private.md", other), chunksOf(other))
		aDocs, err := a.ListDocuments(ctx, repo)
		if err != nil {
			t.Fatalf("ListDocuments(A): %v", err)
		}
		if len(aDocs) != 1 || !bytes.Equal(aDocs[0].FileSHA256, sha(content)) {
			t.Fatalf("user B's upsert replaced user A's document: %+v", aDocs)
		}
	})
	t.Run("scoped store cannot write another user's document", func(t *testing.T) {
		doc := testDoc(repo, "forged.md", content)
		doc.UserID = uidA
		if _, err := b.UpsertDocument(ctx, doc, chunksOf(content)); err == nil {
			t.Fatal("scoped store accepted a document claiming another user's id")
		}
	})
}

// Service scope (userID 0) sees all users but must name a real owner on write,
// mirroring ownerFor.
func TestDocuments_ServiceScope(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	uidA, err := pgstore.EnsureUser(ctx, pool, "test", "doc-svc-a")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	svc := base.ServiceScope()
	const repo = "https://github.com/matthewjhunter/memstore"
	content := "service scope content\n"

	t.Run("write without an owner is rejected", func(t *testing.T) {
		if _, err := svc.UpsertDocument(ctx, testDoc(repo, "svc.md", content), chunksOf(content)); err == nil {
			t.Fatal("service-scope upsert without an explicit UserID succeeded")
		}
	})
	t.Run("write with an explicit owner lands under that owner", func(t *testing.T) {
		doc := testDoc(repo, "svc.md", content)
		doc.UserID = uidA
		id, err := svc.UpsertDocument(ctx, doc, chunksOf(content))
		if err != nil {
			t.Fatalf("service-scope upsert: %v", err)
		}
		var owner int64
		if err := pool.QueryRow(ctx,
			`SELECT user_id FROM memstore_documents WHERE id = $1`, id,
		).Scan(&owner); err != nil {
			t.Fatalf("reading document owner: %v", err)
		}
		if owner != uidA {
			t.Fatalf("document owner = %d, want %d", owner, uidA)
		}
		var chunkOwner int64
		if err := pool.QueryRow(ctx,
			`SELECT DISTINCT user_id FROM memstore_document_chunks WHERE document_id = $1`, id,
		).Scan(&chunkOwner); err != nil {
			t.Fatalf("reading chunk owner: %v", err)
		}
		if chunkOwner != uidA {
			t.Fatalf("chunk owner = %d, want %d (denormalized owner must match)", chunkOwner, uidA)
		}
	})
}
