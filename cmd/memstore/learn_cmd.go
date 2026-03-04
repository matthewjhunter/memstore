package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/matthewjhunter/memstore"
)

func runLearn(args []string) {
	fs := flag.NewFlagSet("learn", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "path to memstore database")
	namespace := fs.String("namespace", "default", "namespace")
	repoPath := fs.String("repo", "", "path to Go repository root (required)")
	subject := fs.String("subject", "", "project subject name (required)")
	ollamaURL := fs.String("ollama", "http://localhost:11434", "Ollama base URL")
	genModel := fs.String("gen-model", "qwen2.5:7b", "LLM model for summarization")
	embedModel := fs.String("embed-model", "embeddinggemma", "embedding model name")
	maxFileSize := fs.Int64("max-file-size", 64*1024, "skip files larger than this (bytes)")
	force := fs.Bool("force", false, "re-learn all files even if unchanged")
	fs.Parse(args)

	if *repoPath == "" {
		fmt.Fprintln(os.Stderr, "learn: --repo is required")
		os.Exit(1)
	}
	if *subject == "" {
		fmt.Fprintln(os.Stderr, "learn: --subject is required")
		os.Exit(1)
	}

	// Resolve absolute path.
	absRepo, err := resolveAbsPath(*repoPath)
	if err != nil {
		log.Fatalf("learn: resolve repo path: %v", err)
	}

	embedder := memstore.NewOllamaEmbedder(*ollamaURL, *embedModel)
	store, closeStore, err := openStoreWithEmbedder(*dbPath, *namespace, embedder)
	if err != nil {
		log.Fatalf("learn: open store: %v", err)
	}
	if store == nil {
		log.Fatal("learn: database not found; run the MCP server first to initialize it")
	}
	defer closeStore()

	generator := memstore.NewOllamaGenerator(*ollamaURL, *genModel)
	learner := memstore.NewCodebaseLearner(store, embedder, generator)

	fmt.Fprintf(os.Stderr, "Learning %s from %s...\n", *subject, absRepo)

	result, err := learner.Learn(context.Background(), memstore.LearnOpts{
		RepoPath:         absRepo,
		Subject:          *subject,
		Namespace:        *namespace,
		MaxFileSizeBytes: *maxFileSize,
		Force:            *force,
	})
	if err != nil {
		log.Fatalf("learn: %v", err)
	}

	fmt.Fprintf(os.Stderr, "Done. repo=%d packages=%d files=%d symbols=%d links=%d skipped=%d superseded=%d llm_calls=%d errors=%d\n",
		result.RepoFactID, result.Packages, result.Files, result.Symbols,
		result.Links, result.Skipped, result.Superseded, result.LLMCalls, len(result.Errors))

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  error: %v\n", e)
	}
}

func resolveAbsPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if !isAbsPath(p) {
		p = abs + "/" + p
	}
	return p, nil
}

func isAbsPath(p string) bool {
	return len(p) > 0 && p[0] == '/'
}
