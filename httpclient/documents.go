package httpclient

// Document ingest client: the wire side of docs/document-ingest.md. Used by
// `memstore ingest` with the dedicated ingest token -- construct a separate
// Client with that credential; the fact-store client's api_key does not carry
// the ingest scope.

import (
	"context"
	"encoding/base64"
	"time"
)

// DocSyncEntry is one manifest line: a file the client can see.
type DocSyncEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"` // hex
	Size   int64  `json:"size"`
}

// DocSkip is one refused manifest entry with the daemon's reason.
type DocSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DocSyncResult is the daemon's manifest delta.
type DocSyncResult struct {
	Need      []string  `json:"need"`
	Skip      []DocSkip `json:"skip"`
	Orphaned  []string  `json:"orphaned"`
	Unchanged int       `json:"unchanged"`
}

// SyncDocuments posts the manifest for one repo identity (repoURL "" = loose
// files) and returns the delta. Orphaned documents are deleted server-side.
func (c *Client) SyncDocuments(ctx context.Context, repoURL string, entries []DocSyncEntry) (*DocSyncResult, error) {
	if entries == nil {
		entries = []DocSyncEntry{}
	}
	var res DocSyncResult
	err := c.post(ctx, "/v1/documents/sync", map[string]any{
		"repo_url": repoURL,
		"entries":  entries,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// DocUpload is one file's bytes plus the client-asserted provenance.
type DocUpload struct {
	RepoURL string
	Commit  string
	Path    string
	Content []byte
	SHA256  string // hex of Content
	Mtime   *time.Time
	Dirty   bool
}

// DocUploadResult reports what the daemon stored.
type DocUploadResult struct {
	ID       int64  `json:"id"`
	Chunks   int    `json:"chunks"`
	Strategy string `json:"strategy"`
}

// UploadDocument ships one file. The daemon hashes, verifies, chunks, and
// stores; a hash mismatch or unroutable file comes back as an error.
func (c *Client) UploadDocument(ctx context.Context, up DocUpload) (*DocUploadResult, error) {
	var res DocUploadResult
	err := c.post(ctx, "/v1/documents", map[string]any{
		"repo_url":    up.RepoURL,
		"commit":      up.Commit,
		"path":        up.Path,
		"content_b64": base64.StdEncoding.EncodeToString(up.Content),
		"sha256":      up.SHA256,
		"mtime":       up.Mtime,
		"dirty":       up.Dirty,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
