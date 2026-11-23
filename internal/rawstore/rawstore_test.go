package rawstore_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/internal/rawstore"
)

const session = "0a219f89-c532-4ee6-b324-54e6dd7b6a97"

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPutWritesTheBytesItWasGiven(t *testing.T) {
	dir := t.TempDir()
	st, err := rawstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := gz(t, "{\"type\":\"user\"}\n")
	rel, err := st.Put(7, session, body)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("7", session+".jsonl.gz"); rel != want {
		t.Errorf("path = %q, want %q", rel, want)
	}
	got, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("stored bytes differ from what was sent")
	}
}

// A transcript holds tool output: file contents, and whatever credentials
// passed through a session. Nobody but the daemon's own user reads it.
func TestFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	st, _ := rawstore.Open(dir)
	rel, err := st.Put(7, session, gz(t, "x\n"))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(dir, "7"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %o, want 700", di.Mode().Perm())
	}
}

// A resumed session uploads again; the file is replaced whole, and a reader
// never sees a partial one.
func TestPutReplacesAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	st, _ := rawstore.Open(dir)
	if _, err := st.Put(7, session, gz(t, "first\n")); err != nil {
		t.Fatal(err)
	}
	second := gz(t, "first\nsecond\n")
	rel, err := st.Put(7, session, second)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, rel))
	if !bytes.Equal(got, second) {
		t.Error("second upload did not replace the first")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "7"))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the transcript", len(entries))
	}
}

// The session id is the only part of the path a client chooses. It must be a
// UUID and nothing else, or it is a way to write outside the directory.
func TestPutRefusesAnythingButAUUID(t *testing.T) {
	st, _ := rawstore.Open(t.TempDir())
	for _, id := range []string{
		"", ".", "..", "../" + session, session + "/../x", "/etc/passwd",
		session + ".jsonl", strings.ToUpper(session) + "\x00", "not-a-uuid",
	} {
		if _, err := st.Put(7, id, gz(t, "x\n")); !errors.Is(err, rawstore.ErrInvalidSessionID) {
			t.Errorf("Put(%q) error = %v, want ErrInvalidSessionID", id, err)
		}
	}
	if _, err := st.Put(0, session, gz(t, "x\n")); err == nil {
		t.Error("Put accepted user id 0")
	}
}

func TestOpenReadsBackWhatWasPut(t *testing.T) {
	st, _ := rawstore.Open(t.TempDir())
	if _, err := st.Put(7, session, gz(t, "line one\nline two\n")); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Get(7, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != "line one\nline two\n" {
		t.Errorf("read back %q", got)
	}
	// One user's id does not open another's file.
	if _, err := st.Get(8, session); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Get as another user: %v, want not exist", err)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	st, _ := rawstore.Open(dir)
	rel, _ := st.Put(7, session, gz(t, "x\n"))
	if err := st.Remove(7, session); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, rel)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still present: %v", err)
	}
	if err := st.Remove(7, session); err != nil {
		t.Errorf("removing a file already gone: %v", err)
	}
}
