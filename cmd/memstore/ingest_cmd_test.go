package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/matthewjhunter/memstore/httpclient"
)

// gitRepo builds a throwaway repo: two committed files, one modified after
// commit, one untracked, one ignored.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q")
	run("remote", "add", "origin", "https://github.com/example/fixture")
	write(".gitignore", "ignored.md\n")
	write("README.md", "# Fixture\n\ncommitted content\n")
	write("docs/design.md", "## Design\n\nmore committed content\n")
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	write("README.md", "# Fixture\n\nmodified after commit\n")
	write("untracked.md", "untracked note\n")
	write("ignored.md", "must never be enumerated\n")
	return dir
}

func TestDiscoverAndEnumerateRepo(t *testing.T) {
	dir := gitRepo(t)
	repo := discoverRepo(dir)

	if repo.isNoGit {
		t.Fatal("repo not detected")
	}
	if repo.url != "https://github.com/example/fixture" {
		t.Errorf("url = %q", repo.url)
	}
	if len(repo.commit) != 40 {
		t.Errorf("commit = %q", repo.commit)
	}
	if !repo.dirty["README.md"] || !repo.dirty["untracked.md"] {
		t.Errorf("dirty set wrong: %v", repo.dirty)
	}
	if repo.dirty["docs/design.md"] {
		t.Error("clean file marked dirty")
	}

	paths, err := enumerateRepo(repo.root)
	if err != nil {
		t.Fatalf("enumerateRepo: %v", err)
	}
	want := []string{".gitignore", "README.md", "docs/design.md", "untracked.md"}
	sort.Strings(want)
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
}

func TestEnumerateDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.md"), []byte("b"), 0o644)

	paths, err := enumerateDir(dir)
	if err != nil {
		t.Fatalf("enumerateDir: %v", err)
	}
	if len(paths) != 2 || paths[0] != "a.md" || paths[1] != "sub/b.md" {
		t.Errorf("paths = %v (.git must be excluded)", paths)
	}
}

// stubDaemon implements the two ingest routes with canned sync behavior and
// records what the client sent.
type stubDaemon struct {
	mu       sync.Mutex
	syncReq  map[string]any
	uploads  []map[string]any
	needAll  bool
	skipPath string
}

func (s *stubDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/documents/sync", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.syncReq = req
		s.mu.Unlock()

		need := []string{}
		skip := []map[string]any{}
		for _, e := range req["entries"].([]any) {
			entry := e.(map[string]any)
			p := entry["path"].(string)
			if p == s.skipPath {
				skip = append(skip, map[string]any{"path": p, "reason": "unroutable extension: .bin"})
				continue
			}
			if s.needAll {
				need = append(need, p)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"need": need, "skip": skip, "orphaned": []string{}, "unchanged": 0,
		})
	})
	mux.HandleFunc("POST /v1/documents", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.uploads = append(s.uploads, req)
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 1, "chunks": 3, "strategy": "markdown"})
	})
	return mux
}

func TestIngestTree_Protocol(t *testing.T) {
	dir := gitRepo(t)
	stub := &stubDaemon{needAll: true, skipPath: ".gitignore"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	client := httpclient.New(srv.URL, "test-token")
	if failed := ingestTree(context.Background(), client, dir); failed != 0 {
		t.Fatalf("ingestTree reported %d failures", failed)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()

	if stub.syncReq["repo_url"] != "https://github.com/example/fixture" {
		t.Errorf("sync repo_url = %v", stub.syncReq["repo_url"])
	}
	entries := stub.syncReq["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("manifest has %d entries, want 4: %v", len(entries), entries)
	}

	if len(stub.uploads) != 3 { // .gitignore skipped by the stub
		t.Fatalf("%d uploads, want 3", len(stub.uploads))
	}
	byPath := map[string]map[string]any{}
	for _, u := range stub.uploads {
		byPath[u["path"].(string)] = u
	}
	readme, ok := byPath["README.md"]
	if !ok {
		t.Fatalf("README.md not uploaded: %v", byPath)
	}
	if readme["dirty"] != true {
		t.Error("modified file not asserted dirty")
	}
	if clean := byPath["docs/design.md"]; clean == nil || clean["dirty"] == true {
		t.Errorf("clean file mis-asserted: %v", clean)
	}
	if untracked := byPath["untracked.md"]; untracked == nil || untracked["dirty"] != true {
		t.Errorf("untracked file not asserted dirty: %v", untracked)
	}
	if len(readme["commit"].(string)) != 40 {
		t.Errorf("commit not asserted: %v", readme["commit"])
	}
	// The manifest carries real hashes: verify one against the working tree.
	content, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	sum := sha256.Sum256(content)
	found := false
	for _, e := range entries {
		entry := e.(map[string]any)
		if entry["path"] == "README.md" && entry["sha256"] == hex.EncodeToString(sum[:]) {
			found = true
		}
	}
	if !found {
		t.Error("manifest sha for README.md does not match working-tree bytes")
	}
}

func TestIngestFile_SingleAndLoose(t *testing.T) {
	dir := gitRepo(t)
	stub := &stubDaemon{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	client := httpclient.New(srv.URL, "test-token")

	// Repo file: repo-relative path plus asserted identity.
	if failed := ingestFile(context.Background(), client, filepath.Join(dir, "docs", "design.md")); failed != 0 {
		t.Fatal("repo-file ingest failed")
	}
	// Loose file: no repo anywhere above the temp dir.
	loose := t.TempDir()
	loosePath := filepath.Join(loose, "note.md")
	os.WriteFile(loosePath, []byte("loose note\n"), 0o644)
	if failed := ingestFile(context.Background(), client, loosePath); failed != 0 {
		t.Fatal("loose-file ingest failed")
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.uploads) != 2 {
		t.Fatalf("%d uploads, want 2", len(stub.uploads))
	}
	repoUp, looseUp := stub.uploads[0], stub.uploads[1]
	if repoUp["path"] != "docs/design.md" || repoUp["repo_url"] != "https://github.com/example/fixture" {
		t.Errorf("repo-file upload wrong: %v", repoUp)
	}
	if looseUp["path"] != "note.md" || looseUp["repo_url"] != "" {
		t.Errorf("loose-file upload wrong: %v", looseUp)
	}
}
