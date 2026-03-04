package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// mockEmbedder implements Embedder for testing.
type mockEmbedder struct {
	dim       int
	model     string
	callCount int
	err       error
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
	result := make([][]float32, len(texts))
	for i := range texts {
		emb := make([]float32, m.dim)
		for j := range emb {
			emb[j] = float32(i+1) * 0.1 * float32(j+1)
		}
		result[i] = emb
	}
	return result, nil
}

func (m *mockEmbedder) Model() string {
	if m.model != "" {
		return m.model
	}
	return "mock"
}

// mockGenerator implements Generator for testing.
type mockGenerator struct {
	responses []string // returned in order; cycles if more calls than responses
	calls     []string // recorded prompts
	callIdx   int
}

func (m *mockGenerator) Generate(_ context.Context, prompt string) (string, error) {
	m.calls = append(m.calls, prompt)
	if len(m.responses) == 0 {
		return "mock summary", nil
	}
	resp := m.responses[m.callIdx%len(m.responses)]
	m.callIdx++
	return resp, nil
}

// newTestStoreForLearn creates an in-memory SQLite store with a mock embedder.
func newTestStoreForLearn(t *testing.T) (Store, *mockEmbedder) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	emb := &mockEmbedder{dim: 8, model: "test-model"}
	store, err := NewSQLiteStore(db, emb, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, emb
}

// createTestRepo creates a temporary directory with a go.mod and Go source files.
func createTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// go.mod
	gomod := "module example.com/testproject\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	// Root package file.
	mainGo := `package testproject

// Store is the main storage interface.
type Store interface {
	Get(id int64) (string, error)
	Put(key string, value string) error
}

// DefaultStore is the default implementation.
type DefaultStore struct {
	data map[string]string
}

// NewDefaultStore creates a new DefaultStore.
func NewDefaultStore() *DefaultStore {
	return &DefaultStore{data: make(map[string]string)}
}

// Get retrieves a value by ID.
func (s *DefaultStore) Get(id int64) (string, error) {
	return "", nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "store.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Sub-package.
	subDir := filepath.Join(dir, "util")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	utilGo := `package util

import "fmt"

// FormatID formats an integer ID as a string.
func FormatID(id int64) string {
	return fmt.Sprintf("%d", id)
}

// MaxRetries is the default retry count.
const MaxRetries = 3
`
	if err := os.WriteFile(filepath.Join(subDir, "format.go"), []byte(utilGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Test file (should be skipped).
	testGo := `package testproject

import "testing"

func TestNothing(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "store_test.go"), []byte(testGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Vendor dir (should be skipped).
	vendorDir := filepath.Join(dir, "vendor")
	if err := os.MkdirAll(vendorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorDir, "dep.go"), []byte("package dep\n"), 0644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestDiscover(t *testing.T) {
	dir := createTestRepo(t)

	modulePath, packages, err := discover(LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	if modulePath != "example.com/testproject" {
		t.Errorf("module path = %q, want %q", modulePath, "example.com/testproject")
	}

	if len(packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(packages))
	}

	// Root package (dir = ".").
	rootPkg := packages[0]
	if rootPkg.RelDir != "." {
		t.Errorf("root pkg dir = %q, want %q", rootPkg.RelDir, ".")
	}
	if rootPkg.PackageName != "testproject" {
		t.Errorf("root pkg name = %q, want %q", rootPkg.PackageName, "testproject")
	}
	if len(rootPkg.Files) != 1 {
		t.Fatalf("root pkg has %d files, want 1 (test files should be skipped)", len(rootPkg.Files))
	}
	if rootPkg.Files[0].RelPath != "store.go" {
		t.Errorf("root file = %q, want %q", rootPkg.Files[0].RelPath, "store.go")
	}

	// Check symbols in root package.
	symbols := rootPkg.Files[0].Symbols
	symbolNames := make(map[string]string)
	for _, s := range symbols {
		symbolNames[s.Name] = s.Kind
	}
	if symbolNames["Store"] != "interface" {
		t.Errorf("expected Store interface, got %q", symbolNames["Store"])
	}
	if symbolNames["DefaultStore"] != "type" {
		t.Errorf("expected DefaultStore type, got %q", symbolNames["DefaultStore"])
	}
	if symbolNames["NewDefaultStore"] != "func" {
		t.Errorf("expected NewDefaultStore func, got %q", symbolNames["NewDefaultStore"])
	}
	if symbolNames["(*DefaultStore).Get"] != "method" {
		t.Errorf("expected Get method, got %q", symbolNames["(*DefaultStore).Get"])
	}

	// Sub-package.
	utilPkg := packages[1]
	if utilPkg.RelDir != "util" {
		t.Errorf("util pkg dir = %q, want %q", utilPkg.RelDir, "util")
	}
	if utilPkg.ImportPath != "example.com/testproject/util" {
		t.Errorf("util import path = %q", utilPkg.ImportPath)
	}

	utilSymbols := make(map[string]string)
	for _, f := range utilPkg.Files {
		for _, s := range f.Symbols {
			utilSymbols[s.Name] = s.Kind
		}
	}
	if utilSymbols["FormatID"] != "func" {
		t.Errorf("expected FormatID func, got %q", utilSymbols["FormatID"])
	}
	if utilSymbols["MaxRetries"] != "const" {
		t.Errorf("expected MaxRetries const, got %q", utilSymbols["MaxRetries"])
	}
}

func TestDiscover_SkipsVendorAndTests(t *testing.T) {
	dir := createTestRepo(t)

	_, packages, err := discover(LearnOpts{RepoPath: dir, Subject: "test"})
	if err != nil {
		t.Fatal(err)
	}

	for _, pkg := range packages {
		for _, f := range pkg.Files {
			if filepath.Base(f.RelPath) == "store_test.go" {
				t.Error("test file was not skipped")
			}
			if filepath.Dir(f.RelPath) == "vendor" {
				t.Error("vendor file was not skipped")
			}
		}
	}
}

func TestDiscover_ContentHash(t *testing.T) {
	dir := createTestRepo(t)

	_, packages, err := discover(LearnOpts{RepoPath: dir, Subject: "test"})
	if err != nil {
		t.Fatal(err)
	}

	for _, pkg := range packages {
		for _, f := range pkg.Files {
			if f.ContentHash == "" {
				t.Errorf("file %s has empty content hash", f.RelPath)
			}
			if len(f.ContentHash) != 64 { // SHA-256 hex
				t.Errorf("file %s hash length = %d, want 64", f.RelPath, len(f.ContentHash))
			}
		}
	}
}

func TestParseGoMod(t *testing.T) {
	dir := t.TempDir()
	gomod := "module github.com/example/project\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	mod, err := parseGoMod(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mod != "github.com/example/project" {
		t.Errorf("got %q", mod)
	}
}

func TestParseGoMod_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := parseGoMod(dir)
	if err == nil {
		t.Fatal("expected error for missing go.mod")
	}
}

func TestLearn_FullPipeline(t *testing.T) {
	dir := createTestRepo(t)
	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{
		responses: []string{
			"Store.go defines the main storage interface and default implementation.",
			"Format.go provides ID formatting utilities.",
			"The util package provides formatting helpers.",
			"The testproject package provides storage with utility helpers.",
			"Testproject is a storage system with formatting utilities.",
		},
	}

	learner := NewCodebaseLearner(store, emb, gen)
	result, err := learner.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should have processed files.
	if result.Files < 2 {
		t.Errorf("files = %d, want >= 2", result.Files)
	}

	// Should have found symbols.
	if result.Symbols < 3 {
		t.Errorf("symbols = %d, want >= 3", result.Symbols)
	}

	// Should have created packages.
	if result.Packages < 2 {
		t.Errorf("packages = %d, want >= 2", result.Packages)
	}

	// Should have created links.
	if result.Links < 3 {
		t.Errorf("links = %d, want >= 3", result.Links)
	}

	// Should have made LLM calls (files + packages + repo).
	if result.LLMCalls < 4 {
		t.Errorf("LLM calls = %d, want >= 4", result.LLMCalls)
	}

	// Verify repo fact exists.
	if result.RepoFactID == 0 {
		t.Error("repo fact ID = 0")
	}

	// Verify warm-tier surfaces are set correctly.
	fileFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "file"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fileFacts) < 2 {
		t.Errorf("file-surface facts = %d, want >= 2", len(fileFacts))
	}

	symFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "symbol"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(symFacts) < 3 {
		t.Errorf("symbol-surface facts = %d, want >= 3", len(symFacts))
	}

	pkgFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "package"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgFacts) < 2 {
		t.Errorf("package-surface facts = %d, want >= 2", len(pkgFacts))
	}

	projectFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "project"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projectFacts) < 1 {
		t.Errorf("project-surface facts = %d, want >= 1", len(projectFacts))
	}
}

func TestLearn_RelearningSkipsUnchanged(t *testing.T) {
	dir := createTestRepo(t)
	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}

	learner := NewCodebaseLearner(store, emb, gen)

	// First run.
	result1, err := learner.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstLLMCalls := result1.LLMCalls

	// Second run — same files, should skip.
	gen2 := &mockGenerator{}
	learner2 := NewCodebaseLearner(store, emb, gen2)
	result2, err := learner2.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result2.Skipped < 2 {
		t.Errorf("skipped = %d, want >= 2 (unchanged files)", result2.Skipped)
	}

	// LLM calls should be less (only packages + repo re-synthesis, no file calls).
	if result2.LLMCalls >= firstLLMCalls {
		t.Errorf("second run LLM calls = %d, should be less than first run = %d", result2.LLMCalls, firstLLMCalls)
	}
}

func TestLearn_ForceRelearnsAll(t *testing.T) {
	dir := createTestRepo(t)
	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}

	learner := NewCodebaseLearner(store, emb, gen)

	// First run.
	_, err := learner.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second run with Force.
	gen2 := &mockGenerator{}
	learner2 := NewCodebaseLearner(store, emb, gen2)
	result2, err := learner2.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result2.Skipped != 0 {
		t.Errorf("skipped = %d, want 0 with Force=true", result2.Skipped)
	}
	if result2.Files < 2 {
		t.Errorf("files = %d, want >= 2 with Force", result2.Files)
	}
	if result2.Superseded < 2 {
		t.Errorf("superseded = %d, want >= 2 with Force", result2.Superseded)
	}
}

func TestLearn_MetadataHasSourceFiles(t *testing.T) {
	dir := createTestRepo(t)
	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}

	learner := NewCodebaseLearner(store, emb, gen)
	_, err := learner.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	fileFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "file"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range fileFacts {
		var meta map[string]any
		if err := json.Unmarshal(f.Metadata, &meta); err != nil {
			t.Errorf("unmarshal metadata for %s: %v", f.Subject, err)
			continue
		}
		sf, ok := meta["source_files"]
		if !ok {
			t.Errorf("file fact %s missing source_files metadata", f.Subject)
			continue
		}
		arr, ok := sf.([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("file fact %s has invalid source_files: %v", f.Subject, sf)
		}
	}
}
