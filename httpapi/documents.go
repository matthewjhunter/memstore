package httpapi

// Document ingest endpoints (docs/document-ingest.md). Two write routes
// under the ingest scope -- the manifest sync and the per-file upload -- plus
// a read-scoped search. The daemon owns type routing and the chunkers; the
// client ships bytes and assertions. Nothing here may invoke a model: the
// chunkers are import-graph-tested against that, and these handlers only
// hash, route, chunk, verify, and store.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/chunk"
	"github.com/matthewjhunter/memstore/chunk/gosrc"
	"github.com/matthewjhunter/memstore/chunk/markdown"
)

// maxDocumentBytes is the per-file size cap, applied at manifest time (so
// oversized files never cross the wire) and re-checked at upload.
const maxDocumentBytes = 1 << 20 // 1 MiB

// The daemon's chunkers. Stateless and safe for concurrent use.
var (
	markdownChunker   = markdown.New()
	goChunker         = gosrc.New()
	lineWindowChunker = chunk.NewLineWindow()
)

// textExtensions route to the line-window fallback: languages without a
// structural chunker yet, and plain text. lang is recorded as the extension.
var textExtensions = map[string]bool{
	"txt": true, "text": true, "rst": true, "adoc": true, "org": true,
	"yaml": true, "yml": true, "toml": true, "ini": true, "cfg": true, "conf": true,
	"sh": true, "bash": true, "sql": true, "proto": true,
	"py": true, "js": true, "ts": true, "rb": true, "rs": true, "java": true,
	"c": true, "h": true, "cpp": true, "hpp": true, "css": true, "html": true,
	"xml": true, "json": true,
}

// excludedDirs are path segments whose subtrees are never ingested. Applied
// daemon-side at manifest time: an exclusion the daemon never saw would be
// one it could not verify (docs/document-ingest.md).
var excludedDirs = map[string]bool{
	"vendor":       true,
	"node_modules": true,
	".git":         true,
}

// routeForPath maps an extension to its chunker and lang. The table lives
// here, on the daemon, because the daemon owns the chunkers and
// chunker_version; a client-side copy would split that authority.
func routeForPath(p string) (chunk.Chunker, string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	switch ext {
	case "md", "markdown":
		return markdownChunker, "markdown", true
	case "go":
		return goChunker, "go", true
	}
	if textExtensions[ext] {
		return lineWindowChunker, ext, true
	}
	return nil, "", false
}

// validDocPath accepts clean, relative, forward-slash paths -- what
// `git ls-files` and a rooted WalkDir emit. Everything else is rejected
// outright; paths are storage keys, and a key like "../x" is a lie about
// where the file lives.
func validDocPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	if clean := path.Clean(p); clean != p {
		return false
	}
	return p != ".." && !strings.HasPrefix(p, "../")
}

// skipReason reports why a manifest entry is not ingestable, or "" when it
// is. This is the server-side filter: it runs before any bytes move.
func skipReason(p string, size int64) string {
	for _, seg := range strings.Split(p, "/") {
		if excludedDirs[seg] {
			return "excluded directory: " + seg
		}
	}
	if _, _, ok := routeForPath(p); !ok {
		ext := path.Ext(p)
		if ext == "" {
			return "no extension"
		}
		return "unroutable extension: " + ext
	}
	if size > maxDocumentBytes {
		return fmt.Sprintf("size %d exceeds cap %d", size, maxDocumentBytes)
	}
	return ""
}

// docStore narrows the per-request scoped store to the document corpus
// interface. The SQLite backend does not carry the corpus.
func (h *Handler) docStore(w http.ResponseWriter, r *http.Request) (memstore.DocumentStore, bool) {
	ds, ok := storeFromCtx(r.Context(), h.store).(memstore.DocumentStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this backend does not carry the document corpus (Postgres required)")
		return nil, false
	}
	return ds, true
}

// handleDocumentSync implements POST /v1/documents/sync: the client sends
// the repo identity and one {path, sha256, size} entry per enumerated file;
// the daemon answers with what it needs, what it refuses (with reasons), and
// which stored documents were orphaned by the manifest -- those are deleted
// here, since a path absent from the manifest is a file that no longer
// exists. Replace-on-(repo,path) can never notice deletions on its own; the
// manifest's complement is what closes that hole.
func (h *Handler) handleDocumentSync(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RepoURL string `json:"repo_url"`
		Entries []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"entries"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	ds, ok := h.docStore(w, r)
	if !ok {
		return
	}

	type skipEntry struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}

	stored, err := ds.ListDocuments(r.Context(), input.RepoURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	storedSHA := make(map[string][]byte, len(stored))
	for _, info := range stored {
		storedSHA[info.Path] = info.FileSHA256
	}

	need := []string{}
	skip := []skipEntry{}
	unchanged := 0
	seen := make(map[string]bool, len(input.Entries))
	for _, e := range input.Entries {
		if !validDocPath(e.Path) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid path %q: paths must be clean, relative, forward-slash", e.Path))
			return
		}
		if seen[e.Path] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("duplicate manifest entry %q", e.Path))
			return
		}
		seen[e.Path] = true

		if reason := skipReason(e.Path, e.Size); reason != "" {
			skip = append(skip, skipEntry{Path: e.Path, Reason: reason})
			continue
		}
		sha, err := hex.DecodeString(e.SHA256)
		if err != nil || len(sha) != sha256.Size {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("entry %q: sha256 must be 64 hex characters", e.Path))
			return
		}
		if have, ok := storedSHA[e.Path]; ok && bytes.Equal(have, sha) {
			unchanged++
			continue
		}
		need = append(need, e.Path)
	}

	// Orphans: stored paths the manifest no longer mentions. Skipped entries
	// count as mentioned -- the file still exists, it is just not ingestable.
	orphaned := []string{}
	for _, info := range stored {
		if !seen[info.Path] {
			orphaned = append(orphaned, info.Path)
		}
	}
	if len(orphaned) > 0 {
		if _, err := ds.DeleteDocuments(r.Context(), input.RepoURL, orphaned); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"need":      need,
		"skip":      skip,
		"orphaned":  orphaned,
		"unchanged": unchanged,
	})
}

// handleDocumentUpload implements POST /v1/documents: one file's bytes plus
// the client's asserted git metadata. The daemon hashes and verifies,
// routes, chunks, re-verifies every span against the bytes, and stores.
// Content travels base64-encoded so a non-UTF-8 file fails loudly here
// rather than being silently mangled by JSON string decoding.
func (h *Handler) handleDocumentUpload(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RepoURL    string     `json:"repo_url"`
		Commit     string     `json:"commit"`
		Path       string     `json:"path"`
		ContentB64 string     `json:"content_b64"`
		SHA256     string     `json:"sha256"`
		Mtime      *time.Time `json:"mtime"`
		Dirty      bool       `json:"dirty"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	ds, ok := h.docStore(w, r)
	if !ok {
		return
	}
	if !validDocPath(input.Path) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid path %q: paths must be clean, relative, forward-slash", input.Path))
		return
	}

	content, err := base64.StdEncoding.DecodeString(input.ContentB64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "content_b64 is not valid base64")
		return
	}
	if reason := skipReason(input.Path, int64(len(content))); reason != "" {
		writeError(w, http.StatusUnprocessableEntity, "not ingestable: "+reason)
		return
	}

	// Verify the asserted hash against the actual bytes. The stored hash is
	// what makes the span invariant checkable later; storing an unverified
	// assertion would poison that.
	asserted, err := hex.DecodeString(input.SHA256)
	if err != nil || len(asserted) != sha256.Size {
		writeError(w, http.StatusBadRequest, "sha256 must be 64 hex characters")
		return
	}
	actual := sha256.Sum256(content)
	if !bytes.Equal(asserted, actual[:]) {
		writeError(w, http.StatusUnprocessableEntity, "sha256 does not match the uploaded bytes")
		return
	}

	// The corpus is text: content lands in a TEXT column and comes back into
	// context windows. Binary that slipped past extension routing stops here.
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		writeError(w, http.StatusUnprocessableEntity, "content is not valid UTF-8 text")
		return
	}

	chunker, lang, _ := routeForPath(input.Path)
	res, err := chunker.Chunk(input.Path, content)
	if errors.Is(err, gosrc.ErrUnparseable) {
		// Work in progress and repos pinned to newer toolchains still
		// contain searchable text; refusing them would leave silent holes.
		// The recorded strategy is what makes the fallback visible and
		// retryable (docs/code-chunking.md).
		chunker = lineWindowChunker
		res, err = chunker.Chunk(input.Path, content)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chunking failed: "+err.Error())
		return
	}

	chunks := make([]memstore.DocumentChunk, 0, len(res.Chunks))
	for _, c := range res.Chunks {
		// Re-verify the verbatim invariant against the actual bytes before
		// anything is stored. The chunkers' own tests hold this too; this
		// check is what makes it a property of the corpus rather than of
		// the test suite.
		if c.ByteStart < 0 || c.ByteEnd > len(content) || string(content[c.ByteStart:c.ByteEnd]) != c.Content {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("chunk %d failed span verification", c.Ordinal))
			return
		}
		chunks = append(chunks, memstore.DocumentChunk{
			Ordinal:      c.Ordinal,
			Content:      c.Content,
			ByteStart:    c.ByteStart,
			ByteEnd:      c.ByteEnd,
			LineStart:    c.LineStart,
			LineEnd:      c.LineEnd,
			HeadingPath:  c.HeadingPath,
			HeadingLevel: c.HeadingLevel,
			Lang:         c.Lang,
			Package:      c.Package,
			ImportPath:   c.ImportPath,
			Symbol:       c.Symbol,
			Receiver:     c.Receiver,
			DeclKind:     c.DeclKind,
			Exported:     c.Exported,
			Signature:    c.Signature,
			ScopePath:    c.ScopePath,
			ImportsUsed:  c.ImportsUsed,
		})
	}

	doc := memstore.Document{
		RepoURL:    input.RepoURL,
		Commit:     input.Commit,
		Path:       input.Path,
		Lang:       lang,
		FileSHA256: actual[:],
		Mtime:      input.Mtime,
		Dirty:      input.Dirty,
		// Trusted stays false until the per-user repo policy table exists;
		// untrusted is the safe default and untrusted chunks come back
		// fenced (docs/document-corpus.md).
		Trusted:        false,
		Generated:      res.Generated,
		IsTest:         strings.HasSuffix(path.Base(input.Path), "_test.go"),
		ChunkerVersion: chunker.Version(),
		ChunkStrategy:  chunker.Strategy(),
		Title:          res.Title,
		FrontMatter:    res.FrontMatter,
	}

	id, err := ds.UpsertDocument(r.Context(), doc, chunks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       id,
		"chunks":   len(chunks),
		"strategy": chunker.Strategy(),
	})
}

// handleDocumentSearch implements POST /v1/documents/search: the read side
// of the corpus. Results are never merged with fact results -- separate
// index, separate tool -- and every hit carries its citation.
func (h *Handler) handleDocumentSearch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Query            string `json:"query"`
		MaxResults       int    `json:"max_results"`
		RepoURL          string `json:"repo_url"`
		PathPrefix       string `json:"path_prefix"`
		Basename         string `json:"basename"`
		Lang             string `json:"lang"`
		IncludeGenerated bool   `json:"include_generated"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	ds, ok := h.docStore(w, r)
	if !ok {
		return
	}

	results, err := ds.SearchDocumentChunks(r.Context(), input.Query, memstore.DocumentSearchOpts{
		MaxResults:       input.MaxResults,
		RepoURL:          input.RepoURL,
		PathPrefix:       input.PathPrefix,
		Basename:         input.Basename,
		Lang:             input.Lang,
		IncludeGenerated: input.IncludeGenerated,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type hit struct {
		memstore.DocumentSearchResult
		Citation string `json:"citation"`
	}
	hits := make([]hit, 0, len(results))
	for _, res := range results {
		hits = append(hits, hit{DocumentSearchResult: res, Citation: res.Citation()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   input.Query,
		"results": hits,
	})
}
