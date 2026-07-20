package httpapi_test

// Endpoint battery for the document ingest routes. Runs against a private
// ephemeral Postgres database (gate: MEMSTORE_TEST_PG), reusing the
// isolation fixture's pool bootstrap.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/pgstore"
)

type docFixture struct {
	handler     http.Handler
	pool        *pgxpool.Pool
	tokens      *pgstore.TokenStore
	ingestToken string
	readToken   string
	writeToken  string // write scope only: must NOT reach ingest routes
}

func newDocFixture(t *testing.T) *docFixture {
	t.Helper()
	ctx := context.Background()
	pool := newIsolationPool(t)
	emb := &mockEmbedder{dim: 4}

	if _, err := pgstore.New(ctx, pool, emb, "docs", 4, 0); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("pgstore.New (bootstrap): %v", err)
	}
	if err := pgstore.InitIdentity(ctx, pool, "docs", "doc-default"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
	store, err := pgstore.New(ctx, pool, emb, "docs", 4, 0)
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}

	uid, err := pgstore.EnsureUser(ctx, pool, "docs", "doc-user")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	ts, err := pgstore.NewTokenStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	ingestTok, err := ts.Issue(ctx, "doc-user@ingest", pgstore.IssueOpts{UserID: uid, Scopes: []string{"ingest"}})
	if err != nil {
		t.Fatalf("Issue ingest token: %v", err)
	}
	readTok, err := ts.Issue(ctx, "doc-user@read", pgstore.IssueOpts{UserID: uid, Scopes: []string{"read"}})
	if err != nil {
		t.Fatalf("Issue read token: %v", err)
	}
	writeTok, err := ts.Issue(ctx, "doc-user@write", pgstore.IssueOpts{UserID: uid, Scopes: []string{"write"}})
	if err != nil {
		t.Fatalf("Issue write token: %v", err)
	}

	h := httpapi.New(store, emb, "", httpapi.WithTokenVerifier(isoTokenVerifier{ts: ts}))
	return &docFixture{handler: h, pool: pool, tokens: ts, ingestToken: ingestTok, readToken: readTok, writeToken: writeTok}
}

func (f *docFixture) do(t *testing.T, token, method, path string, body any) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)

	var out map[string]json.RawMessage
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, w.Body.String())
		}
	}
	return w, out
}

func uploadBody(repo, filePath, content string) map[string]any {
	sum := sha256.Sum256([]byte(content))
	return map[string]any{
		"repo_url":    repo,
		"commit":      "abc123",
		"path":        filePath,
		"content_b64": base64.StdEncoding.EncodeToString([]byte(content)),
		"sha256":      hex.EncodeToString(sum[:]),
	}
}

func syncEntry(filePath, content string) map[string]any {
	sum := sha256.Sum256([]byte(content))
	return map[string]any{
		"path":   filePath,
		"sha256": hex.EncodeToString(sum[:]),
		"size":   len(content),
	}
}

func TestDocumentEndpoints_SyncUploadRoundTrip(t *testing.T) {
	f := newDocFixture(t)
	const repo = "https://github.com/matthewjhunter/example"

	readme := "# Example\n\nThis project demonstrates the ingest wire protocol end to end.\n"
	binaryName := "cmd/tool.bin"

	// First sync: everything new; the binary is refused before any bytes move.
	w, out := f.do(t, f.ingestToken, "POST", "/v1/documents/sync", map[string]any{
		"repo_url": repo,
		"entries": []map[string]any{
			syncEntry("README.md", readme),
			syncEntry(binaryName, "\x00\x01"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", w.Code, w.Body.String())
	}
	var need []string
	if err := json.Unmarshal(out["need"], &need); err != nil || len(need) != 1 || need[0] != "README.md" {
		t.Fatalf("need = %s (err %v), want [README.md]", out["need"], err)
	}
	var skip []struct{ Path, Reason string }
	if err := json.Unmarshal(out["skip"], &skip); err != nil || len(skip) != 1 || skip[0].Path != binaryName {
		t.Fatalf("skip = %s, want the .bin entry", out["skip"])
	}

	// Upload the needed file.
	w, out = f.do(t, f.ingestToken, "POST", "/v1/documents", uploadBody(repo, "README.md", readme))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var strategy string
	json.Unmarshal(out["strategy"], &strategy)
	if strategy != "markdown" {
		t.Errorf("strategy = %q, want markdown", strategy)
	}

	// Second sync with the same manifest: nothing needed, nothing orphaned.
	w, out = f.do(t, f.ingestToken, "POST", "/v1/documents/sync", map[string]any{
		"repo_url": repo,
		"entries": []map[string]any{
			syncEntry("README.md", readme),
			syncEntry(binaryName, "\x00\x01"),
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("re-sync: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(out["need"], &need)
	if len(need) != 0 {
		t.Errorf("re-sync still needs %v", need)
	}
	var unchanged int
	json.Unmarshal(out["unchanged"], &unchanged)
	if unchanged != 1 {
		t.Errorf("unchanged = %d, want 1", unchanged)
	}

	// Search finds the ingested chunk with a citation, under the read token.
	w, out = f.do(t, f.readToken, "POST", "/v1/documents/search", map[string]any{
		"query": "ingest wire protocol",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	var results []struct {
		Citation string `json:"citation"`
		Path     string `json:"path"`
		Trusted  bool   `json:"trusted"`
	}
	if err := json.Unmarshal(out["results"], &results); err != nil || len(results) != 1 {
		t.Fatalf("results = %s (err %v), want exactly 1", out["results"], err)
	}
	if results[0].Path != "README.md" || !strings.Contains(results[0].Citation, repo+"@abc123 README.md:L") {
		t.Errorf("citation wrong: %+v", results[0])
	}
	if results[0].Trusted {
		t.Error("document trusted by default; must be untrusted until a repo policy exists")
	}

	// Third sync with README.md gone from the manifest: orphan deleted.
	w, out = f.do(t, f.ingestToken, "POST", "/v1/documents/sync", map[string]any{
		"repo_url": repo,
		"entries":  []map[string]any{syncEntry(binaryName, "\x00\x01")},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("orphan sync: %d %s", w.Code, w.Body.String())
	}
	var orphaned []string
	json.Unmarshal(out["orphaned"], &orphaned)
	if len(orphaned) != 1 || orphaned[0] != "README.md" {
		t.Fatalf("orphaned = %v, want [README.md]", orphaned)
	}
	_, out = f.do(t, f.readToken, "POST", "/v1/documents/search", map[string]any{"query": "ingest wire protocol"})
	results = nil
	json.Unmarshal(out["results"], &results)
	if len(results) != 0 {
		t.Errorf("deleted document still searchable: %+v", results)
	}
}

func TestDocumentEndpoints_GoRoutingAndFallback(t *testing.T) {
	f := newDocFixture(t)
	const repo = "https://github.com/matthewjhunter/example"

	goodGo := "// Package demo is a demonstration.\npackage demo\n\n// Answer returns the canonical answer to everything.\nfunc Answer() int { return 42 }\n"
	w, out := f.do(t, f.ingestToken, "POST", "/v1/documents", uploadBody(repo, "demo.go", goodGo))
	if w.Code != http.StatusCreated {
		t.Fatalf("go upload: %d %s", w.Code, w.Body.String())
	}
	var strategy string
	json.Unmarshal(out["strategy"], &strategy)
	if strategy != "go" {
		t.Errorf("strategy = %q, want go", strategy)
	}

	brokenGo := "package broken\n\nfunc unclosed( {\n"
	w, out = f.do(t, f.ingestToken, "POST", "/v1/documents", uploadBody(repo, "broken.go", brokenGo))
	if w.Code != http.StatusCreated {
		t.Fatalf("broken go upload: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(out["strategy"], &strategy)
	if strategy != "line-window" {
		t.Errorf("unparseable file strategy = %q, want line-window fallback", strategy)
	}

	testGo := "package demo\n\nfunc TestAnswer(t *T) { _ = Answer() }\n"
	w, _ = f.do(t, f.ingestToken, "POST", "/v1/documents", uploadBody(repo, "demo_test.go", testGo))
	if w.Code != http.StatusCreated {
		t.Fatalf("test file upload: %d %s", w.Code, w.Body.String())
	}
	// is_test is excluded from nothing by default, but must be marked.
	w, out = f.do(t, f.readToken, "POST", "/v1/documents/search", map[string]any{"query": "TestAnswer", "basename": "demo_test.go"})
	var results []struct {
		IsTest bool `json:"is_test"`
	}
	if err := json.Unmarshal(out["results"], &results); err != nil || len(results) == 0 {
		t.Fatalf("test-file chunk not searchable: %s", w.Body.String())
	}
	if !results[0].IsTest {
		t.Error("_test.go document not marked is_test")
	}
}

func TestDocumentEndpoints_UploadRejections(t *testing.T) {
	f := newDocFixture(t)
	const repo = "https://github.com/matthewjhunter/example"

	cases := []struct {
		name string
		body func() map[string]any
		want int
	}{
		{"hash mismatch", func() map[string]any {
			b := uploadBody(repo, "a.md", "content one\n")
			b["sha256"] = strings.Repeat("00", 32)
			return b
		}, http.StatusUnprocessableEntity},
		{"bad hash encoding", func() map[string]any {
			b := uploadBody(repo, "a.md", "content one\n")
			b["sha256"] = "zz"
			return b
		}, http.StatusBadRequest},
		{"non-UTF-8 content", func() map[string]any {
			raw := []byte{0xff, 0xfe, 'h', 'i'}
			sum := sha256.Sum256(raw)
			b := uploadBody(repo, "a.md", "")
			b["content_b64"] = base64.StdEncoding.EncodeToString(raw)
			b["sha256"] = hex.EncodeToString(sum[:])
			return b
		}, http.StatusUnprocessableEntity},
		{"unroutable extension", func() map[string]any {
			return uploadBody(repo, "tool.bin", "not really binary\n")
		}, http.StatusUnprocessableEntity},
		{"vendored path", func() map[string]any {
			return uploadBody(repo, "vendor/dep/dep.go", "package dep\n")
		}, http.StatusUnprocessableEntity},
		{"path traversal", func() map[string]any {
			return uploadBody(repo, "../escape.md", "content\n")
		}, http.StatusBadRequest},
		{"absolute path", func() map[string]any {
			return uploadBody(repo, "/etc/passwd.md", "content\n")
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := f.do(t, f.ingestToken, "POST", "/v1/documents", tc.body())
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestDocumentEndpoints_ScopeEnforcement(t *testing.T) {
	f := newDocFixture(t)
	const repo = "https://github.com/matthewjhunter/example"

	sync := map[string]any{"repo_url": repo, "entries": []map[string]any{}}
	upload := uploadBody(repo, "x.md", "content\n")

	// write scope does not include ingest.
	if w, _ := f.do(t, f.writeToken, "POST", "/v1/documents/sync", sync); w.Code != http.StatusForbidden {
		t.Errorf("write token reached sync: %d", w.Code)
	}
	if w, _ := f.do(t, f.writeToken, "POST", "/v1/documents", upload); w.Code != http.StatusForbidden {
		t.Errorf("write token reached upload: %d", w.Code)
	}
	// ingest scope does not include read.
	if w, _ := f.do(t, f.ingestToken, "POST", "/v1/documents/search", map[string]any{"query": "x"}); w.Code != http.StatusForbidden {
		t.Errorf("ingest token reached search: %d", w.Code)
	}
	// read scope does not include ingest.
	if w, _ := f.do(t, f.readToken, "POST", "/v1/documents", upload); w.Code != http.StatusForbidden {
		t.Errorf("read token reached upload: %d", w.Code)
	}
}

// User A ingests; user B's read token must see nothing through the HTTP
// wiring -- the pgstore battery proves the store predicates, this pins the
// per-request store scoping in ServeHTTP.
func TestDocumentEndpoints_UserIsolation(t *testing.T) {
	f := newDocFixture(t)
	ctx := context.Background()

	uidB, err := pgstore.EnsureUser(ctx, f.pool, "docs", "doc-user-b")
	if err != nil {
		t.Fatalf("EnsureUser B: %v", err)
	}
	readTokB, err := f.tokens.Issue(ctx, "doc-user-b@read", pgstore.IssueOpts{UserID: uidB, Scopes: []string{"read"}})
	if err != nil {
		t.Fatalf("Issue B token: %v", err)
	}

	const repo = "https://github.com/matthewjhunter/example"
	content := "isolated corpus content zephyr\n"
	if w, _ := f.do(t, f.ingestToken, "POST", "/v1/documents", uploadBody(repo, "iso.md", content)); w.Code != http.StatusCreated {
		t.Fatalf("upload: %d", w.Code)
	}

	w, out := f.do(t, f.readToken, "POST", "/v1/documents/search", map[string]any{"query": "zephyr"})
	var results []json.RawMessage
	json.Unmarshal(out["results"], &results)
	if w.Code != http.StatusOK || len(results) != 1 {
		t.Fatalf("same-user search: %d, %d results", w.Code, len(results))
	}

	w, out = f.do(t, readTokB, "POST", "/v1/documents/search", map[string]any{"query": "zephyr"})
	results = nil
	json.Unmarshal(out["results"], &results)
	if w.Code != http.StatusOK {
		t.Fatalf("cross-user search status: %d", w.Code)
	}
	if len(results) != 0 {
		t.Fatalf("user B can search user A's corpus over HTTP: %d results", len(results))
	}
}

func TestDocumentEndpoints_SQLiteBackendIs501(t *testing.T) {
	h := newTestHandlerWith(t) // SQLite-backed, no document corpus
	body, _ := json.Marshal(map[string]any{"repo_url": "", "entries": []map[string]any{}})
	req := httptest.NewRequest("POST", "/v1/documents/sync", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("SQLite backend: %d, want 501", w.Code)
	}
}

// Loose files: repo_url "" all the way through sync and upload.
func TestDocumentEndpoints_LooseFiles(t *testing.T) {
	f := newDocFixture(t)
	note := "a loose note about the quokka migration\n"

	if w, _ := f.do(t, f.ingestToken, "POST", "/v1/documents", uploadBody("", "notes/quokka.md", note)); w.Code != http.StatusCreated {
		t.Fatalf("loose upload: %d", w.Code)
	}
	// Re-upload replaces, does not accumulate: sync with the same entry
	// reports unchanged=1 and no orphans.
	w, out := f.do(t, f.ingestToken, "POST", "/v1/documents/sync", map[string]any{
		"repo_url": "",
		"entries":  []map[string]any{syncEntry("notes/quokka.md", note)},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("loose sync: %d %s", w.Code, w.Body.String())
	}
	var unchanged int
	json.Unmarshal(out["unchanged"], &unchanged)
	if unchanged != 1 {
		t.Errorf("loose sync unchanged = %d, want 1: %s", unchanged, w.Body.String())
	}
}
