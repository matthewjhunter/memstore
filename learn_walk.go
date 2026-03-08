package memstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Learner is the interface for file-level learning and session finalization.
// httpclient.Client implements it for daemon mode; LocalLearner implements it
// for embedded mode (direct store access with in-process LLM).
type Learner interface {
	LearnFile(ctx context.Context, opts LearnFileOpts) (*LearnFileResult, error)
	LearnFinalize(ctx context.Context, opts LearnFinalizeOpts) (*LearnFinalizeResult, error)
}

// LearnFinalizeOpts controls the finalize pass after all files are learned.
type LearnFinalizeOpts struct {
	SessionID  string  `json:"session_id"`
	Subject    string  `json:"subject"`
	ModulePath string  `json:"module_path,omitempty"` // Go module path for import path construction
	Threshold  float64 `json:"threshold,omitempty"`   // cosine similarity threshold (default 0.55)
}

// LearnFinalizeResult summarizes the synthesis and cross-linking pass.
type LearnFinalizeResult struct {
	RepoFactID int64 `json:"repo_fact_id"`
	Packages   int   `json:"packages"`
	Links      int   `json:"links"`
	Superseded int   `json:"superseded"`
	LLMCalls   int   `json:"llm_calls"`
	Facts      int   `json:"facts"` // total facts in the session
}

// LearnWalkOpts controls the walk-and-learn pipeline used by both MCP and CLI.
type LearnWalkOpts struct {
	RepoPath     string
	Subject      string
	MaxFileSize  int64
	ExcludeTests bool
	Force        bool
	Threshold    float64
}

// WalkAndLearn walks a repository, sends each supported file to the Learner,
// and calls finalize for session-level synthesis. This is the single entry
// point for all learn operations — both MCP and CLI use this.
func WalkAndLearn(ctx context.Context, learner Learner, opts LearnWalkOpts) (*LearnResult, error) {
	result := &LearnResult{}
	sessionID := randomSessionID()

	maxSize := opts.MaxFileSize
	if maxSize <= 0 {
		maxSize = defaultMaxFileSize
	}

	modPath := parseModulePathSafe(opts.RepoPath)

	excludes := map[string]bool{
		"vendor": true, "testdata": true, ".git": true, "node_modules": true, ".omc": true,
	}

	err := filepath.WalkDir(opts.RepoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if excludes[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !isSupportedExt(ext) {
			return nil
		}
		if opts.ExcludeTests && ext == ".go" && strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		info, err := d.Info()
		if err != nil || info.Size() > maxSize {
			return nil
		}

		relPath, err := filepath.Rel(opts.RepoPath, path)
		if err != nil {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		hash := sha256.Sum256(content)

		fileResult, err := learner.LearnFile(ctx, LearnFileOpts{
			Subject:     opts.Subject,
			FilePath:    relPath,
			Content:     string(content),
			ContentHash: hex.EncodeToString(hash[:]),
			ModulePath:  modPath,
			Force:       opts.Force,
			SessionID:   sessionID,
		})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s: %w", relPath, err))
			return nil
		}

		if fileResult.Skipped {
			result.Skipped++
		} else {
			result.Files++
			result.Symbols += fileResult.Symbols
			result.Sections += fileResult.Sections
			result.Links += fileResult.Links
			result.Superseded += fileResult.Superseded
			result.LLMCalls += fileResult.LLMCalls
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk repo: %w", err)
	}

	// Finalize: package/repo synthesis + cross-file linking.
	fin, err := learner.LearnFinalize(ctx, LearnFinalizeOpts{
		SessionID:  sessionID,
		Subject:    opts.Subject,
		ModulePath: modPath,
		Threshold:  opts.Threshold,
	})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("finalize: %w", err))
	} else {
		result.RepoFactID = fin.RepoFactID
		result.Packages = fin.Packages
		result.Links += fin.Links
		result.Superseded += fin.Superseded
		result.LLMCalls += fin.LLMCalls
	}

	return result, nil
}

func isSupportedExt(ext string) bool {
	switch ext {
	case ".go", ".md", ".markdown":
		return true
	}
	return false
}

func randomSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("learn-%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// parseModulePathSafe reads go.mod for the module path, returning "" on failure.
func parseModulePathSafe(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if mod, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(mod)
		}
	}
	return ""
}
