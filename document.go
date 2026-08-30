package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Document is the file-level identity and rigid provenance for one ingested
// file, per docs/document-corpus.md. Every field is written by the ingest
// path, never by a model: this is the typed-column provenance system, distinct
// from the flexible model-written metadata facts carry.
//
// The uniqueness key is (namespace, user_id, repo_url, path); Commit is
// deliberately outside it -- re-ingesting at a new commit replaces the row,
// because git already stores history.
type Document struct {
	ID        int64  `json:"id"`
	Namespace string `json:"namespace,omitempty"` // set automatically by the store on upsert
	UserID    int64  `json:"user_id,omitempty"`   // owning user; resolved by the store, see ownerFor
	RepoURL   string `json:"repo_url,omitempty"`  // canonical remote; "" = loose file (stored as NULL)
	Commit    string `json:"commit,omitempty"`    // asserted by the ingesting client
	Path      string `json:"path"`                // repo-relative (or ingest-root-relative) path
	Basename  string `json:"basename"`            // derived from Path by the store
	Lang      string `json:"lang,omitempty"`      // set by the daemon's extension routing

	FileSHA256 []byte     `json:"file_sha256"` // hash of the exact bytes the chunks were cut from
	Mtime      *time.Time `json:"mtime,omitempty"`
	Dirty      bool       `json:"dirty"`               // file was modified/untracked in the working tree at ingest
	Trusted    bool       `json:"trusted"`             // resolved from repo policy at ingest; not consulted live
	Generated  bool       `json:"generated,omitempty"` // file carries a "Code generated" header
	IsTest     bool       `json:"is_test,omitempty"`   // _test.go and equivalents

	ChunkerVersion int             `json:"chunker_version"`
	ChunkStrategy  string          `json:"chunk_strategy,omitempty"` // which chunker cut this file (e.g. "markdown", "go", "line-window" for parse fallback)
	Title          string          `json:"title,omitempty"`          // from front matter when present
	FrontMatter    json.RawMessage `json:"front_matter,omitempty"`   // parsed front matter; rigid provenance, not model-writable

	IngestedAt time.Time `json:"ingested_at"`
}

// DocumentChunk is one verbatim span of a document. The checkable invariant:
// Content equals file[ByteStart:ByteEnd] at the document's recorded
// FileSHA256. Spans are monotonically increasing in Ordinal and do not cover
// the file (front matter and inter-block whitespace are skipped by design).
//
// Spans may overlap, and whether they do is the chunker's choice. The
// structural chunkers cut at boundaries that already mean something -- a
// markdown heading, a Go declaration -- and do not overlap. LineWindow has no
// such boundary to cut at, so it deliberately shares roughly a tenth of each
// window with the next, which is what keeps a match near a window edge
// retrievable (chunk/linewindow.go). Overlap costs the verbatim invariant
// nothing: each span is still byte-identical to the file at the recorded
// hash, whether or not another span covers some of the same bytes.
//
// The heading/scope fields are derived context assembled into embed text at
// embed time; they are never prepended to Content.
type DocumentChunk struct {
	ID         int64 `json:"id"`
	DocumentID int64 `json:"document_id"`
	Ordinal    int   `json:"ordinal"`

	Content   string `json:"content"`
	ByteStart int    `json:"byte_start"`
	ByteEnd   int    `json:"byte_end"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`

	// Markdown-derived context (docs/document-chunking.md).
	HeadingPath  string `json:"heading_path,omitempty"` // ancestors only, never the chunk's own heading
	HeadingLevel int    `json:"heading_level,omitempty"`
	Lang         string `json:"lang,omitempty"` // set on split-out fences

	// Code-derived context (docs/code-chunking.md).
	Package     string   `json:"package,omitempty"`
	ImportPath  string   `json:"import_path,omitempty"`
	Symbol      string   `json:"symbol,omitempty"`
	Receiver    string   `json:"receiver,omitempty"`
	DeclKind    string   `json:"decl_kind,omitempty"` // func | method | type | const | var | import | package_doc
	Exported    bool     `json:"exported,omitempty"`
	Signature   string   `json:"signature,omitempty"`
	ScopePath   string   `json:"scope_path,omitempty"` // package > receiver > symbol
	ImportsUsed []string `json:"imports_used,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// DocumentInfo is the manifest-sync view of a stored document: enough to
// compute need/skip/orphaned against a client manifest without shipping
// content.
type DocumentInfo struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	FileSHA256 []byte `json:"file_sha256"`
	Dirty      bool   `json:"dirty"`
}

// DocumentSearchOpts filters and sizes a document-chunk search.
//
// Basename exists because "show me sqlite.go" is a metadata lookup, not a
// full-text problem (docs/document-corpus.md). Generated documents are
// excluded unless IncludeGenerated is set.
type DocumentSearchOpts struct {
	MaxResults       int
	RepoURL          string // exact match; "" = all repos including loose files
	PathPrefix       string // prefix match on document path
	Basename         string // exact match on document basename
	Lang             string // exact match on document lang
	IncludeGenerated bool
}

// DocumentSearchResult is one chunk hit with the document identity needed for
// its citation. Trusted travels with every result so the caller can fence
// untrusted content before it reaches a context window.
type DocumentSearchResult struct {
	Chunk     DocumentChunk `json:"chunk"`
	RepoURL   string        `json:"repo_url,omitempty"`
	Commit    string        `json:"commit,omitempty"`
	Path      string        `json:"path"`
	Basename  string        `json:"basename"`
	DocLang   string        `json:"doc_lang,omitempty"`
	Trusted   bool          `json:"trusted"`
	Dirty     bool          `json:"dirty"`
	Generated bool          `json:"generated,omitempty"`
	IsTest    bool          `json:"is_test,omitempty"`
	Score     float64       `json:"score"`               // fused rank: FTSWeight*FTSScore + VecWeight*VecScore
	FTSScore  float64       `json:"fts_score,omitempty"` // normalized against the best FTS hit in this result set
	VecScore  float64       `json:"vec_score,omitempty"` // cosine similarity to the query, [0,1]
	Fallback  bool          `json:"fallback,omitempty"`  // matched via decomposed-identifier fallback; ranked below exact hits
}

// Citation renders the mandatory traceability string for a chunk result:
// repo@commit path:L120-160, degrading gracefully for loose files.
func (r DocumentSearchResult) Citation() string {
	loc := fmt.Sprintf("%s:L%d-%d", r.Path, r.Chunk.LineStart, r.Chunk.LineEnd)
	if r.RepoURL == "" {
		return loc
	}
	if r.Commit == "" {
		return r.RepoURL + " " + loc
	}
	return r.RepoURL + "@" + r.Commit + " " + loc
}

// DocumentStore is the document-corpus storage interface. It is separate from
// Store on purpose: documents are written only by the ingest path (never by
// model-facing tools), do not participate in Export/Import, and have no
// supersession -- re-ingest replaces on (repo, path). Postgres implements it;
// the SQLite backend does not carry the corpus.
type DocumentStore interface {
	// UpsertDocument stores or replaces a document and its chunks atomically.
	// Replacement is keyed on (namespace, user, repo_url, path); the previous
	// chunk set is deleted, not merged. Basename is derived from doc.Path.
	// Returns the document id.
	UpsertDocument(ctx context.Context, doc Document, chunks []DocumentChunk) (int64, error)

	// ListDocuments returns the manifest view of every stored document for a
	// repo identity. repoURL "" selects loose files (repo_url IS NULL).
	ListDocuments(ctx context.Context, repoURL string) ([]DocumentInfo, error)

	// DeleteDocuments removes the named documents (chunks cascade) for a repo
	// identity and reports how many documents were deleted. Used for orphan
	// cleanup after a manifest sync. repoURL "" selects loose files.
	DeleteDocuments(ctx context.Context, repoURL string, paths []string) (int64, error)

	// GetDocument returns a document by id, or (nil, nil) when no document
	// with that id is visible in the caller's scope -- matching Get's
	// not-found contract.
	GetDocument(ctx context.Context, id int64) (*Document, error)

	// GetDocumentChunks returns a document's chunks ordered by ordinal.
	GetDocumentChunks(ctx context.Context, documentID int64) ([]DocumentChunk, error)

	// SearchDocumentChunks is FTS over chunk content: an exact pass first,
	// then -- only if the exact pass leaves room -- a decomposed-identifier
	// fallback appended below it (docs/embedding-model-routing.md, measured:
	// fall back, do not blend).
	SearchDocumentChunks(ctx context.Context, query string, opts DocumentSearchOpts) ([]DocumentSearchResult, error)
}

// PendingChunk is one chunk awaiting embedding, with the document path its
// embed header needs. The path travels with the chunk because ChunkEmbedText
// requires it and a per-chunk lookup for a value the queue already joins to
// would be one query per row for nothing.
//
// Path rather than basename, though the store derives a basename and it would
// be shorter. Basenames collide -- index.md, README.md, main.go -- and a
// header that says "index.md" locates a chunk in no particular document. The
// path carries the host for an ingested URL and the directory for a repo file,
// which is the specificity the header exists to add.
type PendingChunk struct {
	Chunk   DocumentChunk
	DocPath string
}

// DocumentEmbedStore is the embedding lifecycle for document chunks. It
// mirrors the fact side's NeedingEmbedding / SetFactVectors / MarkEmbedFailed
// so one queue can drain both corpora rather than each growing its own rules.
//
// Separate from DocumentStore because it is daemon-internal: nothing
// model-facing embeds, and the read-only document capability
// (DocumentSearcher) must not reach these.
type DocumentEmbedStore interface {
	// ChunksNeedingEmbedding returns unembedded, unquarantined chunks
	// oldest first.
	ChunksNeedingEmbedding(ctx context.Context, limit int) ([]PendingChunk, error)

	// SetChunkVector stores a chunk's embedding and clears any prior
	// failure, so a chunk that failed under one embedder can be retried
	// under another.
	SetChunkVector(ctx context.Context, id int64, vec []float32) error

	// MarkChunkEmbedFailed quarantines a chunk the embedder cannot handle,
	// so it stops being returned by every poll and blocking the queue
	// behind it.
	MarkChunkEmbedFailed(ctx context.Context, id int64, reason string) error
}
