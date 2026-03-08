package memstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// createTestMarkdownRepo creates a temporary directory with markdown files for testing.
func createTestMarkdownRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	readme := `# My Project

This is a test project for markdown learning.

## Overview

The project provides tools for doing things.

## Architecture

The system uses a layered design with three components.
`
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}

	docsDir := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docsDir, 0755); err != nil {
		t.Fatal(err)
	}

	design := `# Security Design

## Threat Model

We assume a hostile network. All connections require TLS.

### Internal Threats

Compromised processes cannot escalate.

## Key Management

Keys are derived from passwords using Argon2id.

## Open Questions

- Should we support hardware tokens?
- Key rotation schedule TBD.
`
	if err := os.WriteFile(filepath.Join(docsDir, "security.md"), []byte(design), 0644); err != nil {
		t.Fatal(err)
	}

	// Non-markdown file (should be skipped).
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Excluded directory.
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "notes.md"), []byte("# Git notes\n"), 0644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestDiscoverMarkdown(t *testing.T) {
	dir := createTestMarkdownRepo(t)

	files, err := discoverMarkdown(dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}

	// Should be sorted by relative path.
	if files[0].RelPath != "README.md" {
		t.Errorf("files[0] = %q, want README.md", files[0].RelPath)
	}
	if files[1].RelPath != "docs/security.md" {
		t.Errorf("files[1] = %q, want docs/security.md", files[1].RelPath)
	}

	// Check content hash is set.
	for _, f := range files {
		if f.ContentHash == "" {
			t.Errorf("file %s has empty content hash", f.RelPath)
		}
		if len(f.ContentHash) != 64 {
			t.Errorf("file %s hash length = %d, want 64", f.RelPath, len(f.ContentHash))
		}
	}
}

func TestDiscoverMarkdown_SkipsExcludedDirs(t *testing.T) {
	dir := createTestMarkdownRepo(t)

	files, err := discoverMarkdown(dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		if filepath.Dir(f.RelPath) == ".git" {
			t.Error(".git file was not skipped")
		}
	}
}

func TestParseMarkdownSections(t *testing.T) {
	content := `# Top Title

Intro text.

## First Section

First content.

### Subsection

Sub content.

## Second Section

Second content.
`
	sections := parseMarkdownSections(content)

	// Should have 2 H2 sections.
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}

	if sections[0].Heading != "First Section" {
		t.Errorf("sections[0].Heading = %q", sections[0].Heading)
	}
	if sections[0].Level != 2 {
		t.Errorf("sections[0].Level = %d, want 2", sections[0].Level)
	}
	if sections[1].Heading != "Second Section" {
		t.Errorf("sections[1].Heading = %q", sections[1].Heading)
	}
}

func TestParseMarkdownSections_NoH2(t *testing.T) {
	content := `# Just a title

Some text without any H2 sections.
`
	sections := parseMarkdownSections(content)
	if len(sections) != 0 {
		t.Errorf("got %d sections, want 0", len(sections))
	}
}

func TestParseMarkdownSections_ContentInclusion(t *testing.T) {
	content := `# Title

## Section One

Line one.
Line two.

## Section Two

Line three.
`
	sections := parseMarkdownSections(content)
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}

	// Section one should include its content up to section two.
	if !containsLine(sections[0].Content, "Line one.") {
		t.Error("section one missing 'Line one.'")
	}
	if !containsLine(sections[0].Content, "Line two.") {
		t.Error("section one missing 'Line two.'")
	}
	if containsLine(sections[0].Content, "Line three.") {
		t.Error("section one should not contain 'Line three.'")
	}

	// Section two should include its content.
	if !containsLine(sections[1].Content, "Line three.") {
		t.Error("section two missing 'Line three.'")
	}
}

func containsLine(text, line string) bool {
	for l := range splitLines(text) {
		if l == line {
			return true
		}
	}
	return false
}

func TestLearnFile_Markdown(t *testing.T) {
	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{
		responses: []string{
			"A security design document covering threat model and key management.",
			"Section about threat model assuming hostile network with TLS.",
			"Section about key derivation using Argon2id.",
			"Section listing open questions about tokens and rotation.",
		},
	}

	learner := NewCodebaseLearner(store, emb, gen)

	content := `# Security Design

## Threat Model

We assume a hostile network. All connections require TLS.

## Key Management

Keys are derived from passwords using Argon2id.

## Open Questions

- Should we support hardware tokens?
`

	result, err := learner.LearnFile(context.Background(), LearnFileOpts{
		Subject:  "testproject",
		FilePath: "docs/security.md",
		Content:  content,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Skipped {
		t.Error("expected not skipped")
	}
	if result.FileFactID == 0 {
		t.Error("file fact ID = 0")
	}
	if result.Sections != 3 {
		t.Errorf("sections = %d, want 3", result.Sections)
	}
	if result.Links < 3 {
		t.Errorf("links = %d, want >= 3", result.Links)
	}
	if result.LLMCalls < 4 {
		t.Errorf("LLM calls = %d, want >= 4 (1 doc + 3 sections)", result.LLMCalls)
	}

	// Verify doc-surface fact exists.
	docFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "doc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docFacts) != 1 {
		t.Errorf("doc-surface facts = %d, want 1", len(docFacts))
	}

	// Verify section-surface facts exist.
	sectionFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "section"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sectionFacts) != 3 {
		t.Errorf("section-surface facts = %d, want 3", len(sectionFacts))
	}

	// Verify source_files metadata is set on doc facts.
	for _, f := range docFacts {
		var meta map[string]any
		if err := json.Unmarshal(f.Metadata, &meta); err != nil {
			t.Errorf("unmarshal metadata: %v", err)
			continue
		}
		sf, ok := meta["source_files"]
		if !ok {
			t.Errorf("doc fact missing source_files metadata")
			continue
		}
		arr, ok := sf.([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("doc fact has invalid source_files: %v", sf)
		}
	}
}

func TestLearnFile_Markdown_SkipsUnchanged(t *testing.T) {
	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}
	learner := NewCodebaseLearner(store, emb, gen)

	content := "# Doc\n\n## Section\n\nContent.\n"

	// First learn.
	result1, err := learner.LearnFile(context.Background(), LearnFileOpts{
		Subject:     "test",
		FilePath:    "README.md",
		Content:     content,
		ContentHash: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result1.Skipped {
		t.Error("first learn should not be skipped")
	}

	// Second learn with same hash.
	result2, err := learner.LearnFile(context.Background(), LearnFileOpts{
		Subject:     "test",
		FilePath:    "README.md",
		Content:     content,
		ContentHash: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result2.Skipped {
		t.Error("second learn should be skipped")
	}
}

func TestLearn_IncludesMarkdownFiles(t *testing.T) {
	dir := t.TempDir()

	// go.mod (required for Go discovery).
	gomod := "module example.com/mixedproject\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	// Go file.
	goFile := `package mixedproject

// Foo does something.
func Foo() string { return "foo" }
`
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(goFile), 0644); err != nil {
		t.Fatal(err)
	}

	// Markdown file.
	mdFile := `# Design

## Overview

This project does things.

## Architecture

Three layers.
`
	if err := os.WriteFile(filepath.Join(dir, "DESIGN.md"), []byte(mdFile), 0644); err != nil {
		t.Fatal(err)
	}

	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}
	learner := NewCodebaseLearner(store, emb, gen)

	result, err := learner.Learn(context.Background(), LearnOpts{
		RepoPath: dir,
		Subject:  "mixedproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should process both Go and markdown files.
	if result.Files < 2 {
		t.Errorf("files = %d, want >= 2 (1 go + 1 md)", result.Files)
	}

	// Should have markdown sections.
	if result.Sections < 2 {
		t.Errorf("sections = %d, want >= 2", result.Sections)
	}

	// Should have doc-surface facts.
	docFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "doc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docFacts) < 1 {
		t.Errorf("doc-surface facts = %d, want >= 1", len(docFacts))
	}

	// Should have file-surface facts (from Go).
	fileFacts, err := store.List(context.Background(), QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "file"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fileFacts) < 1 {
		t.Errorf("file-surface facts = %d, want >= 1", len(fileFacts))
	}
}
