package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
)

// runLearnViaHTTP walks a local repo and POSTs each .go file to memstored
// for server-side AST parsing, LLM summarization, and fact storage.
func runLearnViaHTTP(absRepo, subject string, maxFileSize int64, force, excludeTests bool) {
	client := httpclient.New(cliConfig.Remote, cliConfig.APIKey)
	ctx := context.Background()

	modPath := parseModulePath(absRepo)

	fmt.Fprintf(os.Stderr, "Learning %s from %s via %s...\n", subject, absRepo, cliConfig.Remote)

	var (
		total    int
		learned  int
		skipped  int
		errCount int
		symbols  int
	)

	err := filepath.WalkDir(absRepo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if excludeTests && strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}

		relPath, err := filepath.Rel(absRepo, path)
		if err != nil {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		hash := sha256.Sum256(content)
		contentHash := hex.EncodeToString(hash[:])

		total++
		result, err := client.LearnFile(ctx, memstore.LearnFileOpts{
			Subject:     subject,
			FilePath:    relPath,
			Content:     string(content),
			ContentHash: contentHash,
			ModulePath:  modPath,
			Force:       force,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error: %s: %v\n", relPath, err)
			errCount++
			return nil
		}

		if result.Skipped {
			skipped++
		} else {
			learned++
			symbols += result.Symbols
			fmt.Fprintf(os.Stderr, "  learn: %s (%d symbols)\n", relPath, result.Symbols)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("learn: walk: %v", err)
	}

	fmt.Fprintf(os.Stderr, "Done. total=%d learned=%d skipped=%d symbols=%d errors=%d\n",
		total, learned, skipped, symbols, errCount)
}

// parseModulePath reads the module path from go.mod, or returns empty string.
func parseModulePath(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module"))
		}
	}
	return ""
}
