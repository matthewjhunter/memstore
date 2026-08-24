package hookcapture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/matthewjhunter/memstore/httpclient"
)

// --- helpers ---

// deadPID returns a pid that is guaranteed not to be running: a child process
// is started, run to completion, and reaped, so its pid is free (modulo the
// vanishingly small chance the OS recycles it before the assertion).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		// "true" missing is unusual; fall back to a high, unlikely pid.
		return 0x3FFFFFFF
	}
	return cmd.Process.Pid
}

// writeJSON marshals v and writes it to path, creating parent dirs.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readSessionState(t *testing.T, path string) sessionState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state %s: %v", path, err)
	}
	var s sessionState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return s
}

// recordingServer captures the path and body of every request it receives and
// answers with the given status. With status 0 it answers 200.
type recordingServer struct {
	mu     sync.Mutex
	hits   map[string]int
	bodies map[string][]byte
}

func newRecordingServer(t *testing.T, status int) (*httptest.Server, *recordingServer) {
	t.Helper()
	rec := &recordingServer{hits: map[string]int{}, bodies: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		rec.mu.Lock()
		rec.hits[r.URL.Path]++
		rec.bodies[r.URL.Path] = body
		rec.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (rec *recordingServer) count(path string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.hits[path]
}

func (rec *recordingServer) body(path string) []byte {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.bodies[path]
}

// --- sessionsDir / currentPersona ---

func TestSessionsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".cache", "memstore", "sessions")
	if got := sessionsDir(); got != want {
		t.Errorf("sessionsDir() = %q, want %q", got, want)
	}
}

func TestCurrentPersona(t *testing.T) {
	if got := currentPersona(); got == "" {
		t.Error("currentPersona() returned empty string; should fall back to a non-empty value")
	}
}

// --- isProcessAlive ---

func TestIsProcessAlive(t *testing.T) {
	if !isProcessAlive(os.Getpid()) {
		t.Error("isProcessAlive(self) = false, want true")
	}
	if isProcessAlive(deadPID(t)) {
		t.Error("isProcessAlive(reaped child) = true, want false")
	}
	if isProcessAlive(-1) {
		t.Error("isProcessAlive(-1) = true, want false for an invalid pid")
	}
}

// --- updateSessionState ---

func TestUpdateSessionState_CreatesAndIncrements(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ev := hookEvent{SessionID: "sess-1", CWD: "/work", TranscriptPath: "/t/a.jsonl"}
	got := Options{}.updateSessionState(ev)
	if got.MessageCount != 1 {
		t.Errorf("first MessageCount = %d, want 1", got.MessageCount)
	}

	statePath := filepath.Join(sessionsDir(), "sess-1.json")
	on := readSessionState(t, statePath)
	if on.SessionID != "sess-1" || on.CWD != "/work" || on.TranscriptPath != "/t/a.jsonl" {
		t.Errorf("persisted state = %+v, missing fields", on)
	}

	// A second event with empty CWD/TranscriptPath must not clobber prior values.
	got2 := Options{}.updateSessionState(hookEvent{SessionID: "sess-1"})
	if got2.MessageCount != 2 {
		t.Errorf("second MessageCount = %d, want 2", got2.MessageCount)
	}
	on2 := readSessionState(t, statePath)
	if on2.CWD != "/work" || on2.TranscriptPath != "/t/a.jsonl" {
		t.Errorf("empty fields clobbered prior state: %+v", on2)
	}

	// A non-empty TranscriptPath updates it.
	Options{}.updateSessionState(hookEvent{SessionID: "sess-1", TranscriptPath: "/t/b.jsonl"})
	if on3 := readSessionState(t, statePath); on3.TranscriptPath != "/t/b.jsonl" {
		t.Errorf("TranscriptPath = %q, want updated /t/b.jsonl", on3.TranscriptPath)
	}
}

// --- aliveClaudeSessions ---

func TestAliveClaudeSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "sessions")

	writeJSON(t, filepath.Join(dir, "live.json"),
		map[string]any{"pid": os.Getpid(), "sessionId": "alive-session"})
	writeJSON(t, filepath.Join(dir, "dead.json"),
		map[string]any{"pid": deadPID(t), "sessionId": "dead-session"})
	// Malformed and zero-value entries are ignored, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "nopid.json"),
		map[string]any{"pid": 0, "sessionId": "no-pid"})

	alive := aliveClaudeSessions()
	if !alive["alive-session"] {
		t.Error("alive-session not reported alive")
	}
	if alive["dead-session"] {
		t.Error("dead-session reported alive")
	}
	if alive["no-pid"] {
		t.Error("no-pid entry reported alive")
	}
}

func TestAliveClaudeSessions_NoDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := aliveClaudeSessions(); len(got) != 0 {
		t.Errorf("aliveClaudeSessions() with no dir = %v, want empty", got)
	}
}

// --- maybeEmitNudge ---

func TestMaybeEmitNudge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, rec := newRecordingServer(t, 0)
	c := httpclient.New(srv.URL, "")

	ev := hookEvent{SessionID: "s", CWD: "/w"}

	// Below threshold: no nudge.
	Options{}.maybeEmitNudge(c, ev, sessionState{SessionID: "s", MessageCount: hookNudgeThreshold - 1})
	if n := rec.count("/v1/context/hints"); n != 0 {
		t.Errorf("below-threshold posted %d hints, want 0", n)
	}

	// Already nudged: no nudge.
	Options{}.maybeEmitNudge(c, ev, sessionState{SessionID: "s", MessageCount: hookNudgeThreshold + 5, Nudged: true})
	if n := rec.count("/v1/context/hints"); n != 0 {
		t.Errorf("already-nudged posted %d hints, want 0", n)
	}

	// At threshold, not yet nudged: one nudge, and the state file flips Nudged.
	statePath := filepath.Join(sessionsDir(), "s.json")
	writeJSON(t, statePath, sessionState{SessionID: "s", MessageCount: hookNudgeThreshold})
	Options{}.maybeEmitNudge(c, ev, sessionState{SessionID: "s", MessageCount: hookNudgeThreshold})
	if n := rec.count("/v1/context/hints"); n != 1 {
		t.Fatalf("at-threshold posted %d hints, want 1", n)
	}
	var hint map[string]any
	if err := json.Unmarshal(rec.body("/v1/context/hints"), &hint); err != nil {
		t.Fatalf("nudge body not JSON: %v", err)
	}
	if hint["hint_text"] != hookNudgeText {
		t.Errorf("hint_text = %q, want the nudge text", hint["hint_text"])
	}
	if on := readSessionState(t, statePath); !on.Nudged {
		t.Error("state file Nudged flag not set after nudge")
	}
}

// --- drainOnePendingUpload ---

// drainFixture sets HOME to a temp dir and returns the sessions dir path.
func drainFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := sessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDrainOnePendingUpload_Success(t *testing.T) {
	dir := drainFixture(t)
	srv, rec := newRecordingServer(t, 0)
	c := httpclient.New(srv.URL, "")

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"role":"user"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "dead.json")
	writeJSON(t, statePath, sessionState{SessionID: "dead", CWD: "/w", TranscriptPath: transcript})

	Options{}.drainOnePendingUpload(c)

	if n := rec.count("/v1/sessions/transcript"); n != 1 {
		t.Fatalf("posted %d transcripts, want 1", n)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.body("/v1/sessions/transcript"), &payload); err != nil {
		t.Fatalf("transcript body not JSON: %v", err)
	}
	if payload["session_id"] != "dead" {
		t.Errorf("session_id = %v, want dead", payload["session_id"])
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error(".json not removed after successful upload")
	}
	if _, err := os.Stat(filepath.Join(dir, "dead.done")); err != nil {
		t.Errorf(".done not created after successful upload: %v", err)
	}
}

func TestDrainOnePendingUpload_AliveSessionSkipped(t *testing.T) {
	dir := drainFixture(t)
	srv, rec := newRecordingServer(t, 0)
	c := httpclient.New(srv.URL, "")

	// Mark the session's Claude process alive.
	writeJSON(t, filepath.Join(os.Getenv("HOME"), ".claude", "sessions", "p.json"),
		map[string]any{"pid": os.Getpid(), "sessionId": "S1"})

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "S1.json")
	writeJSON(t, statePath, sessionState{SessionID: "S1", TranscriptPath: transcript})

	Options{}.drainOnePendingUpload(c)

	if n := rec.count("/v1/sessions/transcript"); n != 0 {
		t.Errorf("alive session: posted %d transcripts, want 0", n)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("alive session: .json should remain, got %v", err)
	}
}

func TestDrainOnePendingUpload_MissingTranscript(t *testing.T) {
	dir := drainFixture(t)
	srv, rec := newRecordingServer(t, 0)
	c := httpclient.New(srv.URL, "")

	statePath := filepath.Join(dir, "gone.json")
	writeJSON(t, statePath, sessionState{SessionID: "gone", TranscriptPath: "/no/such/file.jsonl"})

	Options{}.drainOnePendingUpload(c)

	if n := rec.count("/v1/sessions/transcript"); n != 0 {
		t.Errorf("missing transcript: posted %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.done")); err != nil {
		t.Errorf("missing transcript should be marked .done: %v", err)
	}
}

func TestDrainOnePendingUpload_UploadFailureRestores(t *testing.T) {
	dir := drainFixture(t)
	srv, _ := newRecordingServer(t, http.StatusInternalServerError)
	c := httpclient.New(srv.URL, "")

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "fail.json")
	writeJSON(t, statePath, sessionState{SessionID: "fail", TranscriptPath: transcript})

	Options{}.drainOnePendingUpload(c)

	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("failed upload: .json should be restored for retry, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fail.done")); !os.IsNotExist(err) {
		t.Error("failed upload must not produce a .done")
	}
}

func TestDrainOnePendingUpload_NoSessionsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, rec := newRecordingServer(t, 0)
	c := httpclient.New(srv.URL, "")
	Options{}.drainOnePendingUpload(c) // must not panic when the dir is absent
	if n := rec.count("/v1/sessions/transcript"); n != 0 {
		t.Errorf("no sessions dir: posted %d, want 0", n)
	}
}

// --- capture entry points: empty remote is a safe no-op ---

func TestRunHookCapture_NoRemote(t *testing.T) {
	Options{}.Run() // returns before touching stdin
}

func TestRunTranscriptCapture_NoRemote(t *testing.T) {
	Options{}.RunTranscript("/does/not/matter")
}

// feedStdin replaces os.Stdin with a pipe carrying payload for the duration of
// the test, restoring it on cleanup.
func feedStdin(t *testing.T, payload string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
	go func() {
		_, _ = w.WriteString(payload)
		_ = w.Close()
	}()
}

func TestRunHookCapture_EndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, rec := newRecordingServer(t, 0)

	// The hook fires while Claude Code is still running, so the session's own
	// process is alive and the final drain step must skip (not archive) it.
	writeJSON(t, filepath.Join(home, ".claude", "sessions", "p.json"),
		map[string]any{"pid": os.Getpid(), "sessionId": "e2e"})

	ev := hookEvent{SessionID: "e2e", CWD: "/w", TranscriptPath: "/t/x.jsonl"}
	payload, _ := json.Marshal(ev)
	feedStdin(t, string(payload))

	Options{Remote: srv.URL}.Run()

	if n := rec.count("/v1/sessions/hook"); n != 1 {
		t.Errorf("posted %d hook payloads, want 1", n)
	}
	// The per-session state file was created and counted, and left in place
	// (.json, not archived to .done) because the session is still alive.
	st := readSessionState(t, filepath.Join(sessionsDir(), "e2e.json"))
	if st.MessageCount != 1 || st.CWD != "/w" {
		t.Errorf("state after hook = %+v, want count 1 and cwd /w", st)
	}
}

func TestRunHookCapture_InvalidJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, rec := newRecordingServer(t, 0)
	feedStdin(t, "this is not json")

	Options{Remote: srv.URL}.Run() // logs and returns, no panic

	if n := rec.count("/v1/sessions/hook"); n != 0 {
		t.Errorf("invalid JSON should post nothing, posted %d", n)
	}
}

func TestRunTranscriptCapture_EndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, rec := newRecordingServer(t, 0)

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"role":"user","content":"hi"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A state file ties the transcript path back to a session id + cwd.
	writeJSON(t, filepath.Join(sessionsDir(), "sess.json"),
		map[string]any{"session_id": "sess", "transcript_path": transcript, "cwd": "/proj"})

	Options{Remote: srv.URL}.RunTranscript(transcript)

	if n := rec.count("/v1/sessions/transcript"); n != 1 {
		t.Fatalf("posted %d transcripts, want 1", n)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.body("/v1/sessions/transcript"), &payload); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if payload["session_id"] != "sess" || payload["cwd"] != "/proj" {
		t.Errorf("payload = %v, want session_id sess / cwd /proj", payload)
	}
}

// A transcript the daemon will never accept -- the real case was a NUL byte
// that Postgres rejects -- must not hold the queue. The drain picks the first
// eligible entry in directory order and returns after one attempt, so before
// this was bounded, the poison entry was retried first on every Stop event and
// everything behind it waited forever.
func TestDrainOnePendingUpload_PoisonEntryStopsBlockingTheQueue(t *testing.T) {
	dir := drainFixture(t)
	// 500 on every request: the permanent-failure case.
	srv, rec := newRecordingServer(t, http.StatusInternalServerError)
	c := httpclient.New(srv.URL, "")

	transcripts := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(transcripts, name)
		if err := os.WriteFile(p, []byte(`{"role":"user"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// "1-poison" sorts before "2-good", so the poison entry is always tried
	// first -- which is exactly the production ordering that caused the block.
	poison := filepath.Join(dir, "1-poison.json")
	writeJSON(t, poison, sessionState{SessionID: "poison", TranscriptPath: write("p.jsonl")})
	good := filepath.Join(dir, "2-good.json")
	writeJSON(t, good, sessionState{SessionID: "good", TranscriptPath: write("g.jsonl")})

	for i := 1; i <= hookMaxUploadFailures; i++ {
		Options{}.drainOnePendingUpload(c)
	}

	if _, err := os.Stat(poison); !os.IsNotExist(err) {
		t.Errorf("poison entry still queued after %d failures", hookMaxUploadFailures)
	}
	if _, err := os.Stat(filepath.Join(dir, "1-poison.failed")); err != nil {
		t.Errorf("poison entry not marked .failed: %v", err)
	}
	if rec.count("/v1/sessions/transcript") != hookMaxUploadFailures {
		t.Errorf("attempted %d uploads, want %d", rec.count("/v1/sessions/transcript"), hookMaxUploadFailures)
	}

	// The entry behind it is now reachable. It still fails against this server,
	// but it was tried, which is the whole point.
	before := rec.count("/v1/sessions/transcript")
	Options{}.drainOnePendingUpload(c)
	if rec.count("/v1/sessions/transcript") != before+1 {
		t.Error("the queued entry behind the poison was never attempted")
	}
	var st sessionState
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.UploadFailures != 1 {
		t.Errorf("failure count = %d, want 1", st.UploadFailures)
	}
}

// A transient failure must stay retryable: one failure does not condemn an
// entry, and the count it carries forward is what bounds it later.
func TestDrainOnePendingUpload_TransientFailureIsRetried(t *testing.T) {
	dir := drainFixture(t)
	srv, _ := newRecordingServer(t, http.StatusServiceUnavailable)
	c := httpclient.New(srv.URL, "")

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"role":"user"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "dead.json")
	writeJSON(t, statePath, sessionState{SessionID: "dead", TranscriptPath: transcript})

	Options{}.drainOnePendingUpload(c)

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("entry not restored for retry: %v", err)
	}
	var st sessionState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.UploadFailures != 1 {
		t.Errorf("failure count = %d, want 1", st.UploadFailures)
	}
	if st.TranscriptPath != transcript || st.SessionID != "dead" {
		t.Error("restored entry lost its fields")
	}
}
