package memstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mdFile represents a discovered markdown file.
type mdFile struct {
	RelPath     string      // relative to repo root
	AbsPath     string      // absolute filesystem path
	ContentHash string      // SHA-256 of file contents
	Source      string      // raw file content
	Sections    []mdSection // parsed H2 sections
}

// mdSection represents a heading-delimited section in a markdown file.
type mdSection struct {
	Heading string // heading text without the # prefix
	Level   int    // heading level (2 for ##, 3 for ###, etc.)
	Content string // full text content of the section (including subsections)
}

var defaultMarkdownExcludeDirs = []string{"vendor", "testdata", ".git", "node_modules", ".omc"}

// discoverMarkdown walks the repo and collects markdown files.
func discoverMarkdown(repoPath string, maxSize int64, excludes []string) ([]mdFile, error) {
	if maxSize <= 0 {
		maxSize = defaultMaxFileSize
	}
	if len(excludes) == 0 {
		excludes = defaultMarkdownExcludeDirs
	}
	excludeSet := make(map[string]bool, len(excludes))
	for _, d := range excludes {
		excludeSet[d] = true
	}

	var files []mdFile
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if excludeSet[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".md" && ext != ".markdown" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxSize {
			return nil
		}

		relPath, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		hash := sha256.Sum256(source)
		content := string(source)

		files = append(files, mdFile{
			RelPath:     relPath,
			AbsPath:     path,
			ContentHash: hex.EncodeToString(hash[:]),
			Source:      content,
			Sections:    parseMarkdownSections(content),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("learn markdown: walk repo: %w", err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})

	return files, nil
}

// splitLines returns an iterator over lines in text.
func splitLines(text string) iter.Seq[string] {
	return strings.SplitSeq(text, "\n")
}

// parseMarkdownSections splits markdown content into H2-level sections.
// Each section includes all content from its heading to the next H2 heading
// (or end of file), including any subsections (H3, H4, etc.).
func parseMarkdownSections(content string) []mdSection {
	var sections []mdSection
	var current *mdSection
	var currentLines []string

	for line := range splitLines(content) {
		trimmed := strings.TrimSpace(line)

		// Check for H2 heading (## but not ###).
		if strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
			// Save previous section.
			if current != nil {
				current.Content = strings.TrimSpace(strings.Join(currentLines, "\n"))
				sections = append(sections, *current)
			}
			heading := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			current = &mdSection{
				Heading: heading,
				Level:   2,
			}
			currentLines = nil
			continue
		}

		if current != nil {
			currentLines = append(currentLines, line)
		}
	}

	// Save last section.
	if current != nil {
		current.Content = strings.TrimSpace(strings.Join(currentLines, "\n"))
		sections = append(sections, *current)
	}

	return sections
}

// learnMarkdownFile processes a single markdown file: parses sections,
// generates an LLM summary, and stores doc and section facts with links.
func (cl *CodebaseLearner) learnMarkdownFile(ctx context.Context, opts LearnFileOpts) (*LearnFileResult, error) {
	result := &LearnFileResult{}

	// Change detection.
	if opts.ContentHash != "" && !opts.Force {
		existing, err := cl.store.List(ctx, QueryOpts{
			OnlyActive: true,
			MetadataFilters: []MetadataFilter{
				{Key: "surface", Op: "=", Value: "doc"},
				{Key: "rel_path", Op: "=", Value: opts.FilePath},
				{Key: "content_hash", Op: "=", Value: opts.ContentHash},
			},
		})
		if err == nil && len(existing) > 0 {
			result.Skipped = true
			result.FileFactID = existing[0].ID
			return result, nil
		}
	}

	// Compute content hash if not provided.
	contentHash := opts.ContentHash
	if contentHash == "" {
		h := sha256.Sum256([]byte(opts.Content))
		contentHash = hex.EncodeToString(h[:])
	}

	sections := parseMarkdownSections(opts.Content)

	// LLM call for doc summary.
	prompt := buildDocSummaryPrompt(opts.FilePath, opts.Content, sections)
	summary, err := cl.generator.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("summarize %s: %w", opts.FilePath, err)
	}
	result.LLMCalls++
	summary = strings.TrimSpace(summary)

	// Create doc fact.
	docMeta := map[string]any{
		"surface":      "doc",
		"rel_path":     opts.FilePath,
		"content_hash": contentHash,
		"source_files": []string{opts.FilePath},
		"quality":      qualityTag(cl.generator.Model()),
	}
	metaJSON, _ := json.Marshal(docMeta)

	emb, err := Single(ctx, cl.embedder, summary)
	if err != nil {
		return nil, fmt.Errorf("embed doc %s: %w", opts.FilePath, err)
	}

	docSubject := fmt.Sprintf("doc:%s/%s", opts.Subject, opts.FilePath)
	docFact := Fact{
		Content:   summary,
		Subject:   docSubject,
		Category:  "project",
		Kind:      "pattern",
		Metadata:  metaJSON,
		Embedding: emb,
	}

	docID, err := cl.store.Insert(ctx, docFact)
	if err != nil {
		return nil, fmt.Errorf("insert doc %s: %w", opts.FilePath, err)
	}
	result.FileFactID = docID

	// Supersede old doc fact.
	oldDocs, _ := cl.store.List(ctx, QueryOpts{
		Subject:    docSubject,
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "doc"},
		},
	})
	for _, old := range oldDocs {
		if old.ID != docID {
			if err := cl.store.Supersede(ctx, old.ID, docID); err == nil {
				result.Superseded++
			}
		}
	}

	// Create section facts.
	for _, sec := range sections {
		if strings.TrimSpace(sec.Content) == "" {
			continue
		}

		secPrompt := buildSectionSummaryPrompt(opts.FilePath, sec.Heading, sec.Content)
		secSummary, err := cl.generator.Generate(ctx, secPrompt)
		if err != nil {
			continue
		}
		result.LLMCalls++
		secSummary = strings.TrimSpace(secSummary)

		secMeta := map[string]any{
			"surface":      "section",
			"rel_path":     opts.FilePath,
			"heading":      sec.Heading,
			"source_files": []string{opts.FilePath},
			"quality":      qualityTag(cl.generator.Model()),
		}
		secMetaJSON, _ := json.Marshal(secMeta)

		secEmb, err := Single(ctx, cl.embedder, secSummary)
		if err != nil {
			continue
		}

		headingSlug := strings.ToLower(strings.ReplaceAll(sec.Heading, " ", "-"))
		secSubject := fmt.Sprintf("doc:%s/%s#%s", opts.Subject, opts.FilePath, headingSlug)
		secFact := Fact{
			Content:   secSummary,
			Subject:   secSubject,
			Category:  "project",
			Kind:      "pattern",
			Metadata:  secMetaJSON,
			Embedding: secEmb,
		}

		secID, err := cl.store.Insert(ctx, secFact)
		if err != nil {
			continue
		}
		result.Sections++

		// Supersede old section fact.
		oldSections, _ := cl.store.List(ctx, QueryOpts{
			Subject:    secSubject,
			OnlyActive: true,
			MetadataFilters: []MetadataFilter{
				{Key: "surface", Op: "=", Value: "section"},
			},
		})
		for _, old := range oldSections {
			if old.ID != secID {
				if err := cl.store.Supersede(ctx, old.ID, secID); err == nil {
					result.Superseded++
				}
			}
		}

		// Link: doc contains section.
		if _, err := cl.store.LinkFacts(ctx, docID, secID, "contains", false, "", nil); err == nil {
			result.Links++
		}
	}

	return result, nil
}

// --- LLM prompt builders for markdown ---

func buildDocSummaryPrompt(relPath, source string, sections []mdSection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the markdown document %q in 2-4 sentences.\n", relPath)
	b.WriteString("Focus on: what this document covers, key topics and decisions, and its role in the project.\n")
	b.WriteString("Return ONLY the summary text, no markdown formatting.\n\n")

	if len(sections) > 0 {
		b.WriteString("Sections:\n")
		for _, sec := range sections {
			fmt.Fprintf(&b, "  - %s\n", sec.Heading)
		}
		b.WriteByte('\n')
	}

	b.WriteString("Content:\n")
	if len(source) > 16000 {
		b.WriteString(source[:16000])
		b.WriteString("\n... (truncated)\n")
	} else {
		b.WriteString(source)
	}
	return b.String()
}

func buildSectionSummaryPrompt(relPath, heading, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the section %q from document %q in 1-3 sentences.\n", heading, relPath)
	b.WriteString("Focus on: key points, decisions, and technical details.\n")
	b.WriteString("Return ONLY the summary text, no markdown formatting.\n\n")
	b.WriteString("Content:\n")
	if len(content) > 8000 {
		b.WriteString(content[:8000])
		b.WriteString("\n... (truncated)\n")
	} else {
		b.WriteString(content)
	}
	return b.String()
}
