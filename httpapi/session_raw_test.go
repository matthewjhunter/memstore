package httpapi_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/rawstore"
	"github.com/matthewjhunter/memstore/internal/teststore"
	"github.com/matthewjhunter/memstore/internal/transcript"
)

const rawSession = "0a219f89-c532-4ee6-b324-54e6dd7b6a97"

// archivingStore is a session store that records what an upload asked it to
// keep, standing in for the Postgres one.
type archivingStore struct {
	*mockSessionStore
	user     int64
	archives []memstore.SessionArchive
	parsed   []*transcript.Session
	turns    []memstore.SessionTurn
}

func (a *archivingStore) UserID() int64 { return a.user }

func (a *archivingStore) SaveArchive(_ context.Context, arch memstore.SessionArchive, p *transcript.Session) (int64, error) {
	a.archives = append(a.archives, arch)
	a.parsed = append(a.parsed, p)
	return int64(len(a.archives)), nil
}

func (a *archivingStore) SaveTurns(_ context.Context, _ string, turns []memstore.SessionTurn) error {
	a.turns = append(a.turns, turns...)
	return nil
}

func rawHandler(t *testing.T, opts ...httpapi.HandlerOpt) (*httpapi.Handler, *archivingStore, string) {
	t.Helper()
	dir := t.TempDir()
	rs, err := rawstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	ss := &archivingStore{mockSessionStore: &mockSessionStore{}, user: 7}
	embedder := &mockEmbedder{dim: 4}
	store := teststore.New(t, embedder, "test")
	all := append([]httpapi.HandlerOpt{httpapi.WithSessionStore(ss), httpapi.WithTranscriptStore(rs)}, opts...)
	return httpapi.New(store, embedder, "", all...), ss, dir
}

func rawTranscript(t *testing.T) string {
	t.Helper()
	enc := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return strings.Join([]string{
		enc(map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-11-01T10:00:00Z", "sessionId": rawSession, "cwd": "/work/repo",
			"message": map[string]any{"role": "user", "content": "check the tree"}}),
		enc(map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-11-01T10:00:05Z",
			"message": map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "checking"},
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "git status"}}}}}),
		enc(map[string]any{"type": "user", "uuid": "u2", "timestamp": "2026-11-01T10:00:07Z",
			"message": map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "On branch main"}}}}),
	}, "\n") + "\n"
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func putRaw(h http.Handler, id string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/v1/sessions/"+id+"/raw?cwd=/work/repo&persona=matthew", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRawUpload_StoresTheFileAndRecordsWhatItHolds(t *testing.T) {
	h, ss, dir := rawHandler(t)
	raw := rawTranscript(t)
	body := gzipBytes(t, raw)

	w := putRaw(h, rawSession, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	// The file on disk is the bytes that were sent, under the user's directory.
	stored, err := os.ReadFile(filepath.Join(dir, "7", rawSession+".jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, body) {
		t.Error("stored file differs from the upload")
	}

	if len(ss.archives) != 1 {
		t.Fatalf("archives recorded = %d, want 1", len(ss.archives))
	}
	a := ss.archives[0]
	sum := sha256.Sum256([]byte(raw))
	if a.SessionID != rawSession || a.Path != filepath.Join("7", rawSession+".jsonl.gz") {
		t.Errorf("archive = %+v", a)
	}
	if a.RawBytes != int64(len(raw)) || a.StoredBytes != int64(len(body)) || a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("archive sizes/hash = %+v", a)
	}
	if p := ss.parsed[0]; len(p.Tools) != 1 || p.Tools[0].Name != "Bash" || len(p.Segments) != 1 {
		t.Errorf("parsed = %d tools, %d segments", len(p.Tools), len(p.Segments))
	}

	// Everything the text upload did still happens, from the same bytes.
	if len(ss.turns) != 2 || ss.turns[0].Content != "check the tree" || ss.turns[1].Content != "checking" || ss.turns[0].CWD != "/work/repo" {
		t.Errorf("turns = %+v", ss.turns)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["turns"] != float64(2) || resp["tool_calls"] != float64(1) || resp["segments"] != float64(1) {
		t.Errorf("response = %v", resp)
	}
}

// The session id is the one part of the file's path the client chooses.
func TestRawUpload_RefusesASessionIDThatIsNotAUUID(t *testing.T) {
	h, ss, dir := rawHandler(t)
	for _, id := range []string{"not-a-uuid", "..%2F..%2Fetc", strings.ToUpper(rawSession)} {
		w := putRaw(h, id, gzipBytes(t, rawTranscript(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("id %q: status %d, want 400", id, w.Code)
		}
	}
	if len(ss.archives) != 0 {
		t.Error("an invalid id was recorded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("an invalid id wrote %d entries", len(entries))
	}
}

func TestRawUpload_RefusesABodyThatIsNotGzip(t *testing.T) {
	h, ss, _ := rawHandler(t)
	if w := putRaw(h, rawSession, []byte(rawTranscript(t))); w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
	if len(ss.archives) != 0 {
		t.Error("a body that was not gzip was recorded")
	}
}

// A small body can decompress to an enormous one. The limit is on what comes
// out, and nothing is kept when it is passed.
func TestRawUpload_BoundsTheDecompressedSize(t *testing.T) {
	h, ss, dir := rawHandler(t, httpapi.WithMaxTranscriptBytes(1024))
	bomb := gzipBytes(t, strings.Repeat("a", 1<<20))
	if len(bomb) > 4096 {
		t.Fatalf("test body is %d bytes compressed; expected it to be tiny", len(bomb))
	}
	if w := putRaw(h, rawSession, bomb); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", w.Code)
	}
	if len(ss.archives) != 0 {
		t.Error("an oversized transcript was recorded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("an oversized transcript was written")
	}
}

// A daemon with no transcript directory says so with a status of its own, so
// a client can tell "not configured" from "no such route" and fall back to
// the text upload either way.
func TestRawUpload_WithoutAStoreIsNotImplemented(t *testing.T) {
	embedder := &mockEmbedder{dim: 4}
	store := teststore.New(t, embedder, "test")
	ss := &archivingStore{mockSessionStore: &mockSessionStore{}, user: 7}
	h := httpapi.New(store, embedder, "", httpapi.WithSessionStore(ss))
	if w := putRaw(h, rawSession, gzipBytes(t, rawTranscript(t))); w.Code != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", w.Code)
	}
}
