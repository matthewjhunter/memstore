package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const DefaultCrossLinkThreshold = 0.55

// LearnedFactRef identifies a fact produced during a learn session.
type LearnedFactRef struct {
	FactID  int64
	Surface string // "file", "doc", "symbol", "section"
	RelPath string
}

// SynthesizeSession performs the finalize pass over a learn session:
// groups files into packages, creates package and repo summary facts,
// builds the containment graph, and creates cross-file links.
func SynthesizeSession(ctx context.Context, store Store, embedder Embedder, gen Generator, refs []LearnedFactRef, opts LearnFinalizeOpts) (*LearnFinalizeResult, error) {
	result := &LearnFinalizeResult{Facts: len(refs)}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = DefaultCrossLinkThreshold
	}

	// Partition by surface type.
	var fileRefs, docRefs []LearnedFactRef
	for _, ref := range refs {
		switch ref.Surface {
		case "file":
			fileRefs = append(fileRefs, ref)
		case "doc":
			docRefs = append(docRefs, ref)
		}
	}

	// --- Package synthesis (Go files grouped by directory) ---
	type pkgGroup struct {
		dir       string
		fileRefs  []LearnedFactRef
		summaries []string
	}
	pkgMap := make(map[string]*pkgGroup)
	for _, ref := range fileRefs {
		dir := filepath.Dir(ref.RelPath)
		if dir == "." {
			dir = "."
		}
		pg, ok := pkgMap[dir]
		if !ok {
			pg = &pkgGroup{dir: dir}
			pkgMap[dir] = pg
		}
		pg.fileRefs = append(pg.fileRefs, ref)

		// Fetch the file fact's summary.
		f, err := store.Get(ctx, ref.FactID)
		if err == nil && f != nil {
			pg.summaries = append(pg.summaries, f.Content)
		}
	}

	// Sort package dirs for determinism.
	var pkgDirs []string
	for dir := range pkgMap {
		pkgDirs = append(pkgDirs, dir)
	}
	sort.Strings(pkgDirs)

	// Create package summary facts.
	pkgFactIDs := make(map[string]int64)
	var pkgSummaryLines []string
	for _, dir := range pkgDirs {
		pg := pkgMap[dir]
		if len(pg.summaries) == 0 {
			continue
		}

		// Infer import path.
		importPath := dir
		if opts.ModulePath != "" {
			if dir == "." {
				importPath = opts.ModulePath
			} else {
				importPath = opts.ModulePath + "/" + filepath.ToSlash(dir)
			}
		}

		// Infer package name from last path segment.
		pkgName := filepath.Base(dir)
		if dir == "." && opts.ModulePath != "" {
			parts := strings.Split(opts.ModulePath, "/")
			pkgName = parts[len(parts)-1]
		}

		if gen == nil {
			continue
		}

		prompt := buildPackageSummaryPrompt(pkgName, importPath, pg.summaries)
		pkgSummary, err := gen.Generate(ctx, prompt)
		if err != nil {
			continue
		}
		result.LLMCalls++
		pkgSummary = strings.TrimSpace(pkgSummary)

		pkgMeta := map[string]any{
			"surface":      "package",
			"rel_dir":      dir,
			"import_path":  importPath,
			"quality":      qualityTag(gen.Model()),
			"source_files": relPathsFromRefs(pg.fileRefs),
		}
		pkgMetaJSON, _ := json.Marshal(pkgMeta)

		emb, err := Single(ctx, embedder, pkgSummary)
		if err != nil {
			continue
		}

		pkgSubject := fmt.Sprintf("pkg:%s", opts.Subject)
		if dir != "." {
			pkgSubject = fmt.Sprintf("pkg:%s/%s", opts.Subject, filepath.ToSlash(dir))
		}

		pkgID, err := store.Insert(ctx, Fact{
			Content:   pkgSummary,
			Subject:   pkgSubject,
			Category:  "project",
			Kind:      "pattern",
			Metadata:  pkgMetaJSON,
			Embedding: emb,
		})
		if err != nil {
			continue
		}
		result.Packages++
		pkgFactIDs[dir] = pkgID

		// Supersede old package fact.
		oldPkgs, _ := store.List(ctx, QueryOpts{
			Subject:    pkgSubject,
			OnlyActive: true,
			MetadataFilters: []MetadataFilter{
				{Key: "surface", Op: "=", Value: "package"},
			},
		})
		for _, old := range oldPkgs {
			if old.ID != pkgID {
				if err := store.Supersede(ctx, old.ID, pkgID); err == nil {
					result.Superseded++
				}
			}
		}

		// Link: package contains each file.
		for _, fr := range pg.fileRefs {
			if _, err := store.LinkFacts(ctx, pkgID, fr.FactID, "contains", false, "", nil); err == nil {
				result.Links++
			}
		}

		pkgSummaryLines = append(pkgSummaryLines, fmt.Sprintf("- %s: %s", importPath, pkgSummary))
	}

	// Collect doc summaries for repo synthesis.
	for _, ref := range docRefs {
		f, err := store.Get(ctx, ref.FactID)
		if err == nil && f != nil {
			pkgSummaryLines = append(pkgSummaryLines, fmt.Sprintf("- (doc) %s", f.Content))
		}
	}

	// --- Repo synthesis ---
	if gen != nil && len(pkgSummaryLines) > 0 {
		modulePath := opts.ModulePath
		if modulePath == "" {
			modulePath = opts.Subject
		}
		prompt := buildRepoSummaryPrompt(opts.Subject, modulePath, pkgSummaryLines)
		repoSummary, err := gen.Generate(ctx, prompt)
		if err == nil {
			result.LLMCalls++
			repoSummary = strings.TrimSpace(repoSummary)

			repoMeta := map[string]any{
				"surface":     "project",
				"module_path": modulePath,
				"quality":     qualityTag(gen.Model()),
			}
			repoMetaJSON, _ := json.Marshal(repoMeta)

			emb, err := Single(ctx, embedder, repoSummary)
			if err == nil {
				repoSubject := fmt.Sprintf("repo:%s", opts.Subject)
				repoID, err := store.Insert(ctx, Fact{
					Content:   repoSummary,
					Subject:   repoSubject,
					Category:  "project",
					Kind:      "pattern",
					Metadata:  repoMetaJSON,
					Embedding: emb,
				})
				if err == nil {
					result.RepoFactID = repoID

					// Supersede old repo fact.
					oldRepos, _ := store.List(ctx, QueryOpts{
						Subject:    repoSubject,
						OnlyActive: true,
						MetadataFilters: []MetadataFilter{
							{Key: "surface", Op: "=", Value: "project"},
						},
					})
					for _, old := range oldRepos {
						if old.ID != repoID {
							if err := store.Supersede(ctx, old.ID, repoID); err == nil {
								result.Superseded++
							}
						}
					}

					// Link: repo contains each package.
					for _, pkgID := range pkgFactIDs {
						if _, err := store.LinkFacts(ctx, repoID, pkgID, "contains", false, "", nil); err == nil {
							result.Links++
						}
					}

					// Link: repo contains each doc.
					for _, ref := range docRefs {
						if _, err := store.LinkFacts(ctx, repoID, ref.FactID, "contains", false, "", nil); err == nil {
							result.Links++
						}
					}
				}
			}
		}
	}

	// --- Cross-file linking ---
	crossRefs := append(docRefs, fileRefs...)
	if len(crossRefs) > 1 {
		cl, err := crossLinkFacts(ctx, store, crossRefs, threshold)
		if err == nil {
			result.Links += cl
		}
	}

	return result, nil
}

// crossLinkFacts creates doc↔file and doc↔doc links based on embedding similarity.
func crossLinkFacts(ctx context.Context, store Store, refs []LearnedFactRef, threshold float64) (int, error) {
	var docRefs, fileRefs []LearnedFactRef
	for _, ref := range refs {
		switch ref.Surface {
		case "doc":
			docRefs = append(docRefs, ref)
		case "file":
			fileRefs = append(fileRefs, ref)
		}
	}

	links := 0
	if len(docRefs) > 0 && len(fileRefs) > 0 {
		n, err := linkBySimilarity(ctx, store, docRefs, fileRefs, threshold, "describes")
		if err != nil {
			return 0, err
		}
		links += n
	}
	if len(docRefs) > 1 {
		n, err := linkPairwise(ctx, store, docRefs, threshold, "related_to")
		if err != nil {
			return 0, err
		}
		links += n
	}
	return links, nil
}

func linkBySimilarity(ctx context.Context, store Store, sources, targets []LearnedFactRef, threshold float64, linkType string) (int, error) {
	sourceEmbs, err := fetchEmbeddings(ctx, store, sources)
	if err != nil {
		return 0, err
	}
	targetEmbs, err := fetchEmbeddings(ctx, store, targets)
	if err != nil {
		return 0, err
	}

	links := 0
	for i, src := range sources {
		if sourceEmbs[i] == nil {
			continue
		}
		for j, tgt := range targets {
			if src.FactID == tgt.FactID {
				continue
			}
			if targetEmbs[j] == nil {
				continue
			}
			if CosineSimilarity(sourceEmbs[i], targetEmbs[j]) >= threshold {
				if _, err := store.LinkFacts(ctx, src.FactID, tgt.FactID, linkType, false, "", nil); err == nil {
					links++
				}
			}
		}
	}
	return links, nil
}

func linkPairwise(ctx context.Context, store Store, refs []LearnedFactRef, threshold float64, linkType string) (int, error) {
	embs, err := fetchEmbeddings(ctx, store, refs)
	if err != nil {
		return 0, err
	}

	links := 0
	for i := range len(refs) {
		if embs[i] == nil {
			continue
		}
		for j := i + 1; j < len(refs); j++ {
			if embs[j] == nil {
				continue
			}
			if CosineSimilarity(embs[i], embs[j]) >= threshold {
				if _, err := store.LinkFacts(ctx, refs[i].FactID, refs[j].FactID, linkType, true, "", nil); err == nil {
					links++
				}
			}
		}
	}
	return links, nil
}

func fetchEmbeddings(ctx context.Context, store Store, refs []LearnedFactRef) ([][]float32, error) {
	embs := make([][]float32, len(refs))
	for i, ref := range refs {
		f, err := store.Get(ctx, ref.FactID)
		if err != nil {
			return nil, fmt.Errorf("fetch fact %d: %w", ref.FactID, err)
		}
		if f != nil && len(f.Embedding) > 0 {
			embs[i] = f.Embedding
		}
	}
	return embs, nil
}

func relPathsFromRefs(refs []LearnedFactRef) []string {
	paths := make([]string, len(refs))
	for i, r := range refs {
		paths[i] = r.RelPath
	}
	return paths
}
