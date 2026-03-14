package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
)

func runLearn(args []string) {
	fs := flag.NewFlagSet("learn", flag.ExitOnError)
	dbPath := fs.String("db", cliConfig.DB, "path to memstore database")
	namespace := fs.String("namespace", cliConfig.Namespace, "namespace")
	repoPath := fs.String("repo", "", "path to repository root (default: positional arg or cwd)")
	subject := fs.String("subject", "", "project subject name (default: directory name of repo path)")
	ollamaURL := fs.String("ollama", cliConfig.Ollama, "Ollama base URL")
	genModel := fs.String("gen-model", cliConfig.GenModel, "LLM model for summarization")
	embedModel := fs.String("embed-model", cliConfig.Model, "embedding model name")
	maxFileSize := fs.Int64("max-file-size", 256*1024, "skip files larger than this (bytes)")
	force := fs.Bool("force", false, "re-learn all files even if unchanged")
	excludeTests := fs.Bool("exclude-tests", false, "exclude _test.go files from ingestion")
	fs.Parse(args)

	if *repoPath == "" && fs.NArg() > 0 {
		*repoPath = fs.Arg(0)
	}
	if *repoPath == "" {
		fmt.Fprintln(os.Stderr, "learn: --repo or positional path argument is required")
		os.Exit(1)
	}

	absRepo, err := resolveAbsPath(*repoPath)
	if err != nil {
		log.Fatalf("learn: resolve repo path: %v", err)
	}

	if *subject == "" {
		*subject = filepath.Base(absRepo)
	}

	var learner memstore.Learner

	if cliConfig.Remote != "" {
		// Daemon mode: learning goes through memstored.
		learner = httpclient.New(cliConfig.Remote, cliConfig.APIKey)
	} else {
		// Local mode: direct store access with embedded LLM.
		embedder := memstore.NewOpenAIEmbedder(*ollamaURL, cliConfig.LLMAPIKey, *embedModel)
		store, closeStore, err := openStoreWithEmbedder(*dbPath, *namespace, embedder)
		if err != nil {
			log.Fatalf("learn: open store: %v", err)
		}
		if store == nil {
			log.Fatal("learn: database not found; run the MCP server first to initialize it")
		}
		defer closeStore()

		generator := memstore.NewOpenAIGenerator(*ollamaURL, cliConfig.LLMAPIKey, *genModel)
		learner = memstore.NewLocalLearner(store, embedder, generator)
	}

	fmt.Fprintf(os.Stderr, "Learning %s from %s...\n", *subject, absRepo)

	result, err := memstore.WalkAndLearn(context.Background(), learner, memstore.LearnWalkOpts{
		RepoPath:     absRepo,
		Subject:      *subject,
		MaxFileSize:  *maxFileSize,
		Force:        *force,
		ExcludeTests: *excludeTests,
		Progress: func(file string, skipped bool, symbols int, err error) {
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "  error: %s: %v\n", file, err)
			case skipped:
				fmt.Fprintf(os.Stderr, "  skip:  %s\n", file)
			default:
				fmt.Fprintf(os.Stderr, "  learn: %s (%d symbols)\n", file, symbols)
			}
		},
	})
	if err != nil {
		log.Fatalf("learn: %v", err)
	}

	fmt.Fprintf(os.Stderr, "Done. repo=%d packages=%d files=%d symbols=%d sections=%d links=%d skipped=%d superseded=%d llm_calls=%d errors=%d\n",
		result.RepoFactID, result.Packages, result.Files, result.Symbols, result.Sections,
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
