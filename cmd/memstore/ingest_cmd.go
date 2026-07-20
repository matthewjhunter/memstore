package main

// memstore ingest <path>: the client side of docs/document-ingest.md. A stat
// on the path decides file versus tree. The client enumerates, hashes, and
// ships bytes plus asserted git metadata; the daemon owns routing, chunking,
// and every verification. Authenticates with the dedicated ingest token
// (ingest_token / MEMSTORE_INGEST_TOKEN) -- never the shared api_key.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
)

// uploadParallelism bounds concurrent uploads.
const uploadParallelism = 4

// repoInfo is the client-asserted identity of a working tree.
type repoInfo struct {
	root    string // absolute path of the repo root; "" when not a repo
	url     string // canonical remote; "" for loose files
	commit  string
	dirty   map[string]bool // repo-relative paths modified or untracked
	isNoGit bool            // plain directory, no git
}

func runIngest(args []string) {
	fset := flag.NewFlagSet("ingest", flag.ExitOnError)
	remote := fset.String("remote", cliConfig.Remote, "memstored base URL")
	fset.Parse(args)

	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: memstore ingest [--remote URL] <path>")
		os.Exit(2)
	}
	if *remote == "" {
		fmt.Fprintln(os.Stderr, "ingest needs a daemon: set remote in the config or pass --remote")
		os.Exit(2)
	}
	token := memstore.LoadIngestToken()
	if token == "" {
		fmt.Fprintln(os.Stderr, `no ingest credential: set ingest_token in the config file or MEMSTORE_INGEST_TOKEN.
Issue one with: memstore admin issue-token --user <name> --scopes ingest <user>@<host>-ingest`)
		os.Exit(2)
	}

	client, err := httpclient.NewWithOptions(*remote, token, httpclient.ClientOptionsFromConfig(cliConfig))
	if err != nil {
		fmt.Fprintf(os.Stderr, "building client: %v\n", err)
		os.Exit(1)
	}

	target, err := filepath.Abs(fset.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving %s: %v\n", fset.Arg(0), err)
		os.Exit(1)
	}
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat %s: %v\n", target, err)
		os.Exit(1)
	}

	ctx := context.Background()
	var failed int
	if info.IsDir() {
		failed = ingestTree(ctx, client, target)
	} else {
		failed = ingestFile(ctx, client, target)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// gitOutput runs git in dir and returns trimmed stdout, or an error.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// discoverRepo derives the asserted repo identity for dir: root, canonical
// remote, HEAD commit, and the per-file dirty set from `git status
// --porcelain` (modified or untracked, matching docs/document-ingest.md).
// A directory outside any repo comes back with isNoGit set.
func discoverRepo(dir string) repoInfo {
	root, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return repoInfo{isNoGit: true}
	}
	info := repoInfo{root: root, dirty: map[string]bool{}}
	// The raw configured URL, not `git remote get-url`: get-url applies
	// insteadOf rewrites from the machine's git config, so the asserted
	// identity would vary by workstation. The value in .git/config is what
	// the clone actually is. A repo with no remote gets "" and stores as
	// loose files.
	info.url, _ = gitOutput(dir, "config", "--get", "remote.origin.url")
	info.commit, _ = gitOutput(dir, "rev-parse", "HEAD")

	// Porcelain lines are XY<space>path; the leading X may itself be a
	// space, so the output must not be whitespace-trimmed before slicing.
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = dir
	if out, err := statusCmd.Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if len(line) < 4 {
				continue
			}
			p := line[3:]
			// Renames report "old -> new"; the new path is the live one.
			if i := strings.Index(p, " -> "); i >= 0 {
				p = p[i+4:]
			}
			p = strings.Trim(p, `"`)
			info.dirty[p] = true
		}
	}
	return info
}

// enumerateRepo lists ingestable candidates: tracked files plus untracked-
// but-not-ignored, exactly the set git's own ignore semantics selects.
func enumerateRepo(root string) ([]string, error) {
	tracked, err := gitOutput(root, "ls-files")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	untracked, err := gitOutput(root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git ls-files --others: %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, group := range []string{tracked, untracked} {
		for _, p := range strings.Split(group, "\n") {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// enumerateDir is the non-repo fallback: a plain walk, paths relative to the
// ingest root.
func enumerateDir(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// hashFile returns the sha256 (hex), size, and mtime of a file.
func hashFile(path string) (string, int64, time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), int64(len(b)), st.ModTime(), nil
}

// ingestTree runs the manifest-sync protocol for a directory. Returns the
// number of failed uploads.
func ingestTree(ctx context.Context, client *httpclient.Client, dir string) int {
	repo := discoverRepo(dir)
	root := repo.root
	var paths []string
	var err error
	if repo.isNoGit {
		root = dir
		paths, err = enumerateDir(dir)
	} else {
		paths, err = enumerateRepo(root)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "enumerating %s: %v\n", dir, err)
		return 1
	}

	entries := make([]httpclient.DocSyncEntry, 0, len(paths))
	for _, p := range paths {
		sha, size, _, err := hashFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			// A file deleted or unreadable mid-walk: leave it out; the next
			// run reconciles.
			fmt.Fprintf(os.Stderr, "  skipping %s: %v\n", p, err)
			continue
		}
		entries = append(entries, httpclient.DocSyncEntry{Path: p, SHA256: sha, Size: size})
	}

	res, err := client.SyncDocuments(ctx, repo.url, entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "manifest sync: %v\n", err)
		return 1
	}

	identity := repo.url
	switch {
	case repo.isNoGit:
		identity = "(loose files, no repo)"
	case identity == "":
		identity = "(repo with no remote; stored as loose files)"
	}
	fmt.Printf("repo: %s\n", identity)
	if repo.commit != "" {
		fmt.Printf("commit: %s (asserted)\n", repo.commit)
	}
	fmt.Printf("manifest: %d files, %d to upload, %d unchanged, %d skipped, %d orphans deleted\n",
		len(entries), len(res.Need), res.Unchanged, len(res.Skip), len(res.Orphaned))

	// Skips grouped by reason.
	byReason := map[string]int{}
	for _, s := range res.Skip {
		byReason[s.Reason]++
	}
	reasons := make([]string, 0, len(byReason))
	for r := range byReason {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Printf("  skipped %d: %s\n", byReason[r], r)
	}

	// Upload the delta with bounded parallelism.
	type outcome struct {
		path     string
		chunks   int
		strategy string
		err      error
	}
	sem := make(chan struct{}, uploadParallelism)
	results := make([]outcome, len(res.Need))
	var wg sync.WaitGroup
	for i, p := range res.Need {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			up, err := uploadOne(ctx, client, repo, root, p)
			if err != nil {
				results[i] = outcome{path: p, err: err}
				return
			}
			results[i] = outcome{path: p, chunks: up.Chunks, strategy: up.Strategy}
		}(i, p)
	}
	wg.Wait()

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  FAILED %s: %v\n", r.path, r.err)
			continue
		}
		fmt.Printf("  ingested %s (%d chunks, %s)\n", r.path, r.chunks, r.strategy)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d of %d uploads failed\n", failed, len(res.Need))
	}
	return failed
}

// uploadOne ships a single repo-relative path.
func uploadOne(ctx context.Context, client *httpclient.Client, repo repoInfo, root, relPath string) (*httpclient.DocUploadResult, error) {
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	mtime := st.ModTime()
	return client.UploadDocument(ctx, httpclient.DocUpload{
		RepoURL: repo.url,
		Commit:  repo.commit,
		Path:    relPath,
		Content: content,
		SHA256:  hex.EncodeToString(sum[:]),
		Mtime:   &mtime,
		Dirty:   repo.dirty[relPath],
	})
}

// ingestFile ships a single file: step 4 of the protocol alone, with the
// repo identity derived by walking up for .git. Returns 1 on failure.
func ingestFile(ctx context.Context, client *httpclient.Client, file string) int {
	dir := filepath.Dir(file)
	repo := discoverRepo(dir)

	var relPath string
	root := dir
	if repo.isNoGit {
		// Loose file: path relative to its own directory is just the name.
		relPath = filepath.Base(file)
		repo.url = ""
	} else {
		root = repo.root
		rel, err := filepath.Rel(repo.root, file)
		if err != nil || strings.HasPrefix(rel, "..") {
			fmt.Fprintf(os.Stderr, "%s is outside its repo root %s\n", file, repo.root)
			return 1
		}
		relPath = filepath.ToSlash(rel)
	}

	up, err := uploadOne(ctx, client, repo, root, relPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ingest %s: %v\n", relPath, err)
		return 1
	}
	fmt.Printf("ingested %s (%d chunks, %s)\n", relPath, up.Chunks, up.Strategy)
	return 0
}
