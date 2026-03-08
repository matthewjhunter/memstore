package memstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWalkAndLearn(t *testing.T) {
	dir := t.TempDir()

	// go.mod.
	gomod := "module example.com/testproject\n\ngo 1.21\n"
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644)

	// Go file.
	goFile := `package testproject

// Greeter says hello.
type Greeter struct{}

// Greet returns a greeting.
func (g *Greeter) Greet(name string) string { return "Hello, " + name }
`
	os.WriteFile(filepath.Join(dir, "greeter.go"), []byte(goFile), 0644)

	// Markdown file.
	mdFile := `# Test Project

## Overview

This project provides greeting functionality.

## API

The Greeter type is the main entry point.
`
	os.WriteFile(filepath.Join(dir, "README.md"), []byte(mdFile), 0644)

	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}
	learner := NewLocalLearner(store, emb, gen)

	result, err := WalkAndLearn(context.Background(), learner, LearnWalkOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Files < 2 {
		t.Errorf("files = %d, want >= 2 (1 go + 1 md)", result.Files)
	}
	if result.Symbols == 0 {
		t.Error("expected symbols from Go file")
	}
	if result.Sections == 0 {
		t.Error("expected sections from markdown file")
	}
	if result.Packages == 0 {
		t.Error("expected package synthesis from finalize")
	}
	if result.RepoFactID == 0 {
		t.Error("expected repo fact from finalize")
	}
	if result.Links == 0 {
		t.Error("expected links from finalize")
	}
}

func TestWalkAndLearn_SkipsUnsupported(t *testing.T) {
	dir := t.TempDir()

	// Only unsupported files.
	os.WriteFile(filepath.Join(dir, "data.csv"), []byte("a,b,c\n"), 0644)
	os.WriteFile(filepath.Join(dir, "image.png"), []byte("fake png"), 0644)

	store, emb := newTestStoreForLearn(t)
	gen := &mockGenerator{}
	learner := NewLocalLearner(store, emb, gen)

	result, err := WalkAndLearn(context.Background(), learner, LearnWalkOpts{
		RepoPath: dir,
		Subject:  "testproject",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Files != 0 {
		t.Errorf("expected 0 files for unsupported types, got %d", result.Files)
	}
}
