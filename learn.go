package memstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LearnOpts controls the codebase ingestion pipeline.
type LearnOpts struct {
	RepoPath         string   // absolute path to repo root
	Subject          string   // project subject name (e.g. "memstore")
	Namespace        string   // store namespace
	ModulePath       string   // Go module path; empty = parse from go.mod
	MaxFileSizeBytes int64    // skip files larger than this (default 64KB)
	ExcludeDirs      []string // dirs to skip (default: vendor, testdata, .git)
	Force            bool     // re-learn all files even if unchanged
	ExcludeTests     bool     // exclude _test.go files from ingestion
}

// LearnResult summarizes the outcome of a learn run.
type LearnResult struct {
	RepoFactID int64
	Packages   int
	Files      int
	Symbols    int
	Sections   int
	Links      int
	Skipped    int // unchanged files
	Superseded int
	LLMCalls   int
	Errors     []error
}

// CodebaseLearner ingests a Go codebase into structured facts with a
// four-level containment graph: repo → package → file → symbol.
type CodebaseLearner struct {
	store     Store
	embedder  Embedder
	generator Generator
}

// NewCodebaseLearner creates a learner that uses the given store, embedder,
// and generator to ingest and persist codebase knowledge.
func NewCodebaseLearner(store Store, embedder Embedder, generator Generator) *CodebaseLearner {
	return &CodebaseLearner{store: store, embedder: embedder, generator: generator}
}

const (
	defaultMaxFileSize = 256 * 1024 // 256KB
)

var defaultExcludeDirs = []string{"vendor", "testdata", ".git"}

// LearnFileOpts controls single-file learning via the HTTP API.
type LearnFileOpts struct {
	Subject     string // project name (e.g. "herald")
	FilePath    string // relative path (e.g. "internal/feeds/parser.go")
	Content     string // file source code
	ContentHash string // SHA256 for dedup; empty = always learn
	ModulePath  string // Go module path (e.g. "github.com/matthewjhunter/herald")
	PackageName string // Go package name; empty = parsed from AST
	Force       bool   // re-learn even if hash unchanged
	SessionID   string // optional; enables cross-file linking via finalize
}

// LearnFileResult summarizes a single-file learn operation.
type LearnFileResult struct {
	FileFactID int64   `json:"file_fact_id"`
	SymbolIDs  []int64 `json:"symbol_ids,omitempty"`
	Symbols    int     `json:"symbols"`
	Sections   int     `json:"sections"`
	Links      int     `json:"links"`
	Superseded int     `json:"superseded"`
	Skipped    bool    `json:"skipped"` // true if content hash matched and Force=false
	LLMCalls   int     `json:"llm_calls"`
}

// LearnFile processes a single source file: detects file type by extension
// and dispatches to the appropriate learner (Go AST for .go, markdown section
// parser for .md/.markdown). The caller provides file content directly — no
// filesystem access is needed on the server.
func (cl *CodebaseLearner) LearnFile(ctx context.Context, opts LearnFileOpts) (*LearnFileResult, error) {
	ext := strings.ToLower(filepath.Ext(opts.FilePath))
	if ext == ".md" || ext == ".markdown" {
		return cl.learnMarkdownFile(ctx, opts)
	}
	return cl.learnGoFile(ctx, opts)
}

// learnGoFile processes a single Go source file: parses AST, generates an LLM
// summary, and stores file and symbol facts.
func (cl *CodebaseLearner) learnGoFile(ctx context.Context, opts LearnFileOpts) (*LearnFileResult, error) {
	result := &LearnFileResult{}

	// Change detection: check if we already have a file fact with the same hash.
	if opts.ContentHash != "" && !opts.Force {
		existing, err := cl.store.List(ctx, QueryOpts{
			OnlyActive: true,
			MetadataFilters: []MetadataFilter{
				{Key: "surface", Op: "=", Value: "file"},
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

	// Parse AST from content.
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, opts.FilePath, opts.Content, parser.ParseComments)
	pkgName := opts.PackageName
	var symbols []goSymbol
	if err == nil {
		if pkgName == "" {
			pkgName = astFile.Name.Name
		}
		symbols = extractSymbols(fset, astFile)
	} else if pkgName == "" {
		pkgName = "unknown"
	}

	// Build symbol signatures for LLM prompt.
	var sigLines []string
	for _, sym := range symbols {
		line := fmt.Sprintf("  %s %s", sym.Kind, sym.Name)
		if sym.Signature != "" {
			line = fmt.Sprintf("  %s", sym.Signature)
		}
		if sym.DocComment != "" {
			firstLine := strings.SplitN(sym.DocComment, "\n", 2)[0]
			line += " // " + firstLine
		}
		sigLines = append(sigLines, line)
	}

	// LLM call for file summary.
	prompt := buildFileSummaryPrompt(opts.FilePath, pkgName, opts.Content, sigLines)
	summary, err := cl.generator.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("summarize %s: %w", opts.FilePath, err)
	}
	result.LLMCalls++
	summary = strings.TrimSpace(summary)

	// Compute content hash if not provided.
	contentHash := opts.ContentHash
	if contentHash == "" {
		h := sha256.Sum256([]byte(opts.Content))
		contentHash = hex.EncodeToString(h[:])
	}

	// Create file fact.
	fileMeta := map[string]any{
		"surface":      "file",
		"rel_path":     opts.FilePath,
		"content_hash": contentHash,
		"quality":      qualityTag(cl.generator.Model()),
	}
	metaJSON, _ := json.Marshal(fileMeta)

	emb, err := Single(ctx, cl.embedder, summary)
	if err != nil {
		return nil, fmt.Errorf("embed file %s: %w", opts.FilePath, err)
	}

	fileSubject := fmt.Sprintf("file:%s/%s", opts.Subject, opts.FilePath)
	fileFact := Fact{
		Content:   summary,
		Subject:   fileSubject,
		Category:  "project",
		Kind:      "pattern",
		Metadata:  metaJSON,
		Embedding: emb,
	}

	fileID, err := cl.store.Insert(ctx, fileFact)
	if err != nil {
		return nil, fmt.Errorf("insert file %s: %w", opts.FilePath, err)
	}
	result.FileFactID = fileID

	// Supersede old file fact if exists.
	oldFiles, _ := cl.store.List(ctx, QueryOpts{
		Subject:    fileSubject,
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "file"},
		},
	})
	for _, old := range oldFiles {
		if old.ID != fileID {
			if err := cl.store.Supersede(ctx, old.ID, fileID); err == nil {
				result.Superseded++
			}
		}
	}

	// Create symbol facts.
	for _, sym := range symbols {
		symContent := sym.DocComment
		if symContent == "" {
			symContent = fmt.Sprintf("%s %s", sym.Kind, sym.Name)
			if sym.Signature != "" {
				symContent = sym.Signature
			}
		}

		symMeta := map[string]any{
			"surface":     "symbol",
			"rel_path":    opts.FilePath,
			"symbol_name": sym.Name,
			"symbol_kind": sym.Kind,
			"line":        sym.Line,
			"quality":     qualityTag(cl.generator.Model()),
		}
		if sym.Signature != "" {
			symMeta["signature"] = sym.Signature
		}
		symMetaJSON, _ := json.Marshal(symMeta)

		symEmb, err := Single(ctx, cl.embedder, symContent)
		if err != nil {
			continue
		}

		symSubject := fmt.Sprintf("sym:%s.%s", pkgName, sym.Name)
		symFact := Fact{
			Content:   symContent,
			Subject:   symSubject,
			Category:  "project",
			Kind:      "pattern",
			Metadata:  symMetaJSON,
			Embedding: symEmb,
		}

		symID, err := cl.store.Insert(ctx, symFact)
		if err != nil {
			continue
		}
		result.SymbolIDs = append(result.SymbolIDs, symID)
		result.Symbols++

		// Supersede old symbol fact.
		oldSyms, _ := cl.store.List(ctx, QueryOpts{
			OnlyActive: true,
			MetadataFilters: []MetadataFilter{
				{Key: "surface", Op: "=", Value: "symbol"},
				{Key: "rel_path", Op: "=", Value: opts.FilePath},
				{Key: "symbol_name", Op: "=", Value: sym.Name},
			},
		})
		for _, old := range oldSyms {
			if old.ID != symID {
				if err := cl.store.Supersede(ctx, old.ID, symID); err == nil {
					result.Superseded++
				}
			}
		}

		// Link: file contains symbol.
		if _, err := cl.store.LinkFacts(ctx, fileID, symID, "contains", false, "", nil); err == nil {
			result.Links++
		}
	}

	return result, nil
}

// --- Phase 1: Discovery types ---

// goSymbol represents a symbol extracted from Go AST.
type goSymbol struct {
	Name       string // e.g. "Store", "CosineSimilarity", "(*SQLiteStore).Insert"
	Kind       string // "func", "type", "method", "interface", "const", "var"
	DocComment string // godoc comment text
	Signature  string // e.g. "func(ctx context.Context, f Fact) (int64, error)"
	Line       int    // source line number
}

// goFile represents a parsed Go source file.
type goFile struct {
	RelPath     string     // relative to repo root (e.g. "mcpserver/server.go")
	AbsPath     string     // absolute filesystem path
	PackageDir  string     // relative directory (e.g. "mcpserver")
	PackageName string     // Go package name from AST
	Imports     []string   // import paths
	Symbols     []goSymbol // extracted symbols
	ContentHash string     // SHA-256 of file contents
	Source      string     // raw file source (for LLM summarization)
}

// goPackage groups files by package directory.
type goPackage struct {
	RelDir      string   // relative directory (e.g. "mcpserver", "." for root)
	ImportPath  string   // full import path (e.g. "github.com/matthewjhunter/memstore/mcpserver")
	PackageName string   // Go package name
	Files       []goFile // source files in this package
}

// --- Phase 1: Discovery ---

// discover walks the repo, parses Go files, and extracts AST information.
// No LLM or store calls — pure filesystem and parsing.
func discover(opts LearnOpts) (string, []goPackage, error) {
	modulePath := opts.ModulePath
	if modulePath == "" {
		var err error
		modulePath, err = parseGoMod(opts.RepoPath)
		if err != nil {
			return "", nil, fmt.Errorf("learn: parse go.mod: %w", err)
		}
	}

	maxSize := opts.MaxFileSizeBytes
	if maxSize <= 0 {
		maxSize = defaultMaxFileSize
	}

	excludes := opts.ExcludeDirs
	if len(excludes) == 0 {
		excludes = defaultExcludeDirs
	}
	excludeSet := make(map[string]bool, len(excludes))
	for _, d := range excludes {
		excludeSet[d] = true
	}

	// Walk and collect .go files.
	var files []goFile
	err := filepath.WalkDir(opts.RepoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if excludeSet[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") && opts.ExcludeTests {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // skip files we can't stat
		}
		if info.Size() > maxSize {
			return nil // skip oversized files
		}

		relPath, err := filepath.Rel(opts.RepoPath, path)
		if err != nil {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}

		hash := sha256.Sum256(source)

		gf := goFile{
			RelPath:     relPath,
			AbsPath:     path,
			PackageDir:  filepath.Dir(relPath),
			ContentHash: hex.EncodeToString(hash[:]),
			Source:      string(source),
		}

		// Parse AST.
		fset := token.NewFileSet()
		astFile, err := parser.ParseFile(fset, path, source, parser.ParseComments)
		if err != nil {
			// Store what we can without AST.
			gf.PackageName = "unknown"
			files = append(files, gf)
			return nil
		}

		gf.PackageName = astFile.Name.Name

		// Extract imports.
		for _, imp := range astFile.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			gf.Imports = append(gf.Imports, importPath)
		}

		// Extract symbols.
		gf.Symbols = extractSymbols(fset, astFile)

		files = append(files, gf)
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("learn: walk repo: %w", err)
	}

	// Group files by package directory.
	pkgMap := make(map[string]*goPackage)
	for _, f := range files {
		dir := f.PackageDir
		if dir == "." {
			dir = "."
		}
		pkg, ok := pkgMap[dir]
		if !ok {
			importPath := modulePath
			if dir != "." {
				importPath = modulePath + "/" + filepath.ToSlash(dir)
			}
			pkg = &goPackage{
				RelDir:      dir,
				ImportPath:  importPath,
				PackageName: f.PackageName,
			}
			pkgMap[dir] = pkg
		}
		pkg.Files = append(pkg.Files, f)
	}

	// Sort packages for deterministic output.
	var packages []goPackage
	for _, pkg := range pkgMap {
		packages = append(packages, *pkg)
	}
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].RelDir < packages[j].RelDir
	})

	return modulePath, packages, nil
}

// parseGoMod reads go.mod to extract the module path.
func parseGoMod(repoPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if mod, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(mod), nil
		}
	}
	return "", fmt.Errorf("no module directive found in go.mod")
}

// extractSymbols pulls exported symbols from a parsed Go file.
func extractSymbols(fset *token.FileSet, file *ast.File) []goSymbol {
	var symbols []goSymbol

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			sym := goSymbol{
				Name:       funcDeclName(d),
				Kind:       "func",
				DocComment: docText(d.Doc),
				Signature:  funcSignature(fset, d),
				Line:       fset.Position(d.Pos()).Line,
			}
			if d.Recv != nil {
				sym.Kind = "method"
			}
			symbols = append(symbols, sym)

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					sym := goSymbol{
						Name:       s.Name.Name,
						Kind:       typeSpecKind(s),
						DocComment: docText(firstNonNilDoc(d.Doc, s.Doc, s.Comment)),
						Line:       fset.Position(s.Pos()).Line,
					}
					symbols = append(symbols, sym)

				case *ast.ValueSpec:
					for _, name := range s.Names {
						if !name.IsExported() {
							continue
						}
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						symbols = append(symbols, goSymbol{
							Name:       name.Name,
							Kind:       kind,
							DocComment: docText(firstNonNilDoc(d.Doc, s.Doc, s.Comment)),
							Line:       fset.Position(name.Pos()).Line,
						})
					}
				}
			}
		}
	}
	return symbols
}

// funcDeclName returns the qualified name for a function declaration.
// Methods include the receiver type: "(*SQLiteStore).Insert".
func funcDeclName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	recv := d.Recv.List[0].Type
	return fmt.Sprintf("(%s).%s", exprName(recv), d.Name.Name)
}

// exprName extracts a human-readable type name from a receiver expression.
func exprName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + exprName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprName(t.X) + "." + t.Sel.Name
	case *ast.IndexExpr:
		return exprName(t.X) + "[" + exprName(t.Index) + "]"
	default:
		return "?"
	}
}

// funcSignature returns a human-readable signature for a function declaration.
func funcSignature(fset *token.FileSet, d *ast.FuncDecl) string {
	if d.Type == nil {
		return ""
	}
	start := fset.Position(d.Type.Pos()).Offset
	end := fset.Position(d.Type.End()).Offset
	// Read the source span from the file.
	fname := fset.Position(d.Pos()).Filename
	src, err := os.ReadFile(fname)
	if err != nil || end > len(src) {
		return ""
	}
	sig := string(src[start:end])
	if d.Recv != nil && len(d.Recv.List) > 0 {
		return fmt.Sprintf("func (%s) %s%s", exprName(d.Recv.List[0].Type), d.Name.Name, sig[len("func"):])
	}
	return fmt.Sprintf("func %s%s", d.Name.Name, sig[len("func"):])
}

func typeSpecKind(s *ast.TypeSpec) string {
	switch s.Type.(type) {
	case *ast.InterfaceType:
		return "interface"
	case *ast.StructType:
		return "type"
	default:
		return "type"
	}
}

func docText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimSpace(cg.Text())
}

func firstNonNilDoc(groups ...*ast.CommentGroup) *ast.CommentGroup {
	for _, g := range groups {
		if g != nil {
			return g
		}
	}
	return nil
}

// --- Phase 2-5: Full pipeline ---

// Learn ingests a Go codebase, producing a four-level containment graph
// (repo → package → file → symbol) with LLM-distilled summaries.
func (cl *CodebaseLearner) Learn(ctx context.Context, opts LearnOpts) (*LearnResult, error) {
	result := &LearnResult{}

	// Phase 1: Discovery
	modulePath, packages, err := discover(opts)
	if err != nil {
		return nil, err
	}

	// Phase 2: Re-learn detection — load existing file facts for change detection.
	existingFiles := make(map[string]*Fact) // keyed by relative path
	facts, err := cl.store.List(ctx, QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "file"},
		},
	})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list existing file facts: %w", err))
	} else {
		for i, f := range facts {
			// Match by subject prefix for this project.
			if !strings.HasPrefix(f.Subject, "file:"+opts.Subject+"/") && f.Subject != "file:"+opts.Subject {
				continue
			}
			var meta map[string]any
			if len(f.Metadata) > 0 {
				_ = json.Unmarshal(f.Metadata, &meta)
			}
			if relPath, _ := meta["rel_path"].(string); relPath != "" {
				existingFiles[relPath] = &facts[i]
			}
		}
	}

	// Also load existing symbol facts for supersession.
	existingSymbols := make(map[string]*Fact) // keyed by "relpath:symbolname"
	symFacts, err := cl.store.List(ctx, QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "symbol"},
		},
	})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list existing symbol facts: %w", err))
	} else {
		for i, f := range symFacts {
			if !strings.HasPrefix(f.Subject, "sym:"+opts.Subject) {
				continue
			}
			var meta map[string]any
			if len(f.Metadata) > 0 {
				_ = json.Unmarshal(f.Metadata, &meta)
			}
			relPath, _ := meta["rel_path"].(string)
			symName, _ := meta["symbol_name"].(string)
			if relPath != "" && symName != "" {
				existingSymbols[relPath+":"+symName] = &symFacts[i]
			}
		}
	}

	// Load existing package facts for supersession.
	existingPackages := make(map[string]*Fact) // keyed by relative dir
	pkgFacts, err := cl.store.List(ctx, QueryOpts{
		OnlyActive: true,
		MetadataFilters: []MetadataFilter{
			{Key: "surface", Op: "=", Value: "package"},
		},
	})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list existing package facts: %w", err))
	} else {
		for i, f := range pkgFacts {
			if !strings.HasPrefix(f.Subject, "pkg:"+opts.Subject) {
				continue
			}
			var meta map[string]any
			if len(f.Metadata) > 0 {
				_ = json.Unmarshal(f.Metadata, &meta)
			}
			if relDir, _ := meta["rel_dir"].(string); relDir != "" {
				existingPackages[relDir] = &pkgFacts[i]
			}
		}
	}

	// Track file fact IDs per package for Phase 4 linking.
	type fileResult struct {
		factID  int64
		relPath string
	}
	pkgFileResults := make(map[string][]fileResult) // pkgDir -> file results
	pkgFileSummaries := make(map[string][]string)   // pkgDir -> file summaries
	fileSymbolIDs := make(map[string][]int64)       // relPath -> symbol fact IDs
	// pkgDir -> relPath -> symbolName -> factID (for test-to-source linking)
	pkgFileSymNames := make(map[string]map[string]map[string]int64)

	// Phase 3: File-level processing
	for _, pkg := range packages {
		for _, f := range pkg.Files {
			// Change detection.
			if existing, ok := existingFiles[f.RelPath]; ok && !opts.Force {
				var meta map[string]any
				if len(existing.Metadata) > 0 {
					_ = json.Unmarshal(existing.Metadata, &meta)
				}
				if hash, _ := meta["content_hash"].(string); hash == f.ContentHash {
					result.Skipped++
					// Keep existing fact for linking.
					pkgFileResults[pkg.RelDir] = append(pkgFileResults[pkg.RelDir], fileResult{
						factID: existing.ID, relPath: f.RelPath,
					})
					pkgFileSummaries[pkg.RelDir] = append(pkgFileSummaries[pkg.RelDir], existing.Content)
					continue
				}
			}

			// Build symbol signatures for the LLM prompt.
			var sigLines []string
			for _, sym := range f.Symbols {
				line := fmt.Sprintf("  %s %s", sym.Kind, sym.Name)
				if sym.Signature != "" {
					line = fmt.Sprintf("  %s", sym.Signature)
				}
				if sym.DocComment != "" {
					// Include first line of doc comment.
					firstLine := strings.SplitN(sym.DocComment, "\n", 2)[0]
					line += " // " + firstLine
				}
				sigLines = append(sigLines, line)
			}

			// LLM call for file summary.
			prompt := buildFileSummaryPrompt(f.RelPath, f.PackageName, f.Source, sigLines)
			summary, err := cl.generator.Generate(ctx, prompt)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("summarize %s: %w", f.RelPath, err))
				continue
			}
			result.LLMCalls++
			summary = strings.TrimSpace(summary)

			// Create file fact.
			fileMeta := map[string]any{
				"surface":      "file",
				"file_path":    f.AbsPath,
				"rel_path":     f.RelPath,
				"content_hash": f.ContentHash,
				"source_files": []string{f.RelPath},
				"quality":      qualityTag(cl.generator.Model()),
			}
			metaJSON, _ := json.Marshal(fileMeta)

			emb, err := Single(ctx, cl.embedder, summary)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("embed file %s: %w", f.RelPath, err))
				continue
			}

			fileSubject := fmt.Sprintf("file:%s/%s", opts.Subject, f.RelPath)
			fileFact := Fact{
				Content:   summary,
				Subject:   fileSubject,
				Category:  "project",
				Kind:      "pattern",
				Metadata:  metaJSON,
				Embedding: emb,
			}

			fileID, err := cl.store.Insert(ctx, fileFact)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("insert file %s: %w", f.RelPath, err))
				continue
			}
			result.Files++

			// Supersede old file fact if exists.
			if old, ok := existingFiles[f.RelPath]; ok {
				if err := cl.store.Supersede(ctx, old.ID, fileID); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("supersede file %s: %w", f.RelPath, err))
				} else {
					result.Superseded++
				}
			}

			pkgFileResults[pkg.RelDir] = append(pkgFileResults[pkg.RelDir], fileResult{
				factID: fileID, relPath: f.RelPath,
			})
			pkgFileSummaries[pkg.RelDir] = append(pkgFileSummaries[pkg.RelDir], summary)

			// Create symbol facts from AST (no LLM call).
			for _, sym := range f.Symbols {
				symContent := sym.DocComment
				if symContent == "" {
					symContent = fmt.Sprintf("%s %s", sym.Kind, sym.Name)
					if sym.Signature != "" {
						symContent = sym.Signature
					}
				}

				symMeta := map[string]any{
					"surface":     "symbol",
					"file_path":   f.AbsPath,
					"rel_path":    f.RelPath,
					"symbol_name": sym.Name,
					"symbol_kind": sym.Kind,
					"line":        sym.Line,
					"quality":     qualityTag(cl.generator.Model()),
				}
				if sym.Signature != "" {
					symMeta["signature"] = sym.Signature
				}
				symMetaJSON, _ := json.Marshal(symMeta)

				symEmb, err := Single(ctx, cl.embedder, symContent)
				if err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("embed symbol %s.%s: %w", f.RelPath, sym.Name, err))
					continue
				}

				symSubject := fmt.Sprintf("sym:%s.%s", pkg.PackageName, sym.Name)
				if sym.Kind == "method" {
					symSubject = fmt.Sprintf("sym:%s.%s", pkg.PackageName, sym.Name)
				}

				symFact := Fact{
					Content:   symContent,
					Subject:   symSubject,
					Category:  "project",
					Kind:      "pattern",
					Metadata:  symMetaJSON,
					Embedding: symEmb,
				}

				symID, err := cl.store.Insert(ctx, symFact)
				if err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("insert symbol %s.%s: %w", f.RelPath, sym.Name, err))
					continue
				}
				result.Symbols++

				// Supersede old symbol fact.
				if old, ok := existingSymbols[f.RelPath+":"+sym.Name]; ok {
					if err := cl.store.Supersede(ctx, old.ID, symID); err != nil {
						result.Errors = append(result.Errors, fmt.Errorf("supersede symbol %s: %w", sym.Name, err))
					} else {
						result.Superseded++
					}
				}

				fileSymbolIDs[f.RelPath] = append(fileSymbolIDs[f.RelPath], symID)

				// Track symbol name → factID for test linking.
				if pkgFileSymNames[pkg.RelDir] == nil {
					pkgFileSymNames[pkg.RelDir] = make(map[string]map[string]int64)
				}
				if pkgFileSymNames[pkg.RelDir][f.RelPath] == nil {
					pkgFileSymNames[pkg.RelDir][f.RelPath] = make(map[string]int64)
				}
				pkgFileSymNames[pkg.RelDir][f.RelPath][sym.Name] = symID

				// Link: file contains symbol.
				if _, err := cl.store.LinkFacts(ctx, fileID, symID, "contains", false, "", nil); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("link file->symbol %s: %w", sym.Name, err))
				} else {
					result.Links++
				}
			}
		}

		// Link test functions to source symbols within this package.
		if symsByFile := pkgFileSymNames[pkg.RelDir]; symsByFile != nil {
			srcSymbols := make(map[string]int64)
			testSymbols := make(map[string]int64)
			for relPath, syms := range symsByFile {
				if strings.HasSuffix(relPath, "_test.go") {
					for name, id := range syms {
						if strings.HasPrefix(name, "Test") {
							testSymbols[name] = id
						}
					}
				} else {
					for name, id := range syms {
						srcSymbols[name] = id
					}
				}
			}
			for testName, testID := range testSymbols {
				if _, srcID := matchTestToSource(testName, srcSymbols); srcID != 0 {
					if _, err := cl.store.LinkFacts(ctx, testID, srcID, "tests", true, "", nil); err != nil {
						result.Errors = append(result.Errors, fmt.Errorf("link test %s: %w", testName, err))
					} else {
						result.Links++
					}
				}
			}
		}
	}

	// Phase 4: Package-level synthesis
	pkgFactIDs := make(map[string]int64) // pkgDir -> fact ID
	for _, pkg := range packages {
		summaries := pkgFileSummaries[pkg.RelDir]
		if len(summaries) == 0 {
			continue
		}

		// LLM call for package summary.
		prompt := buildPackageSummaryPrompt(pkg.PackageName, pkg.ImportPath, summaries)
		pkgSummary, err := cl.generator.Generate(ctx, prompt)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("summarize package %s: %w", pkg.RelDir, err))
			continue
		}
		result.LLMCalls++
		pkgSummary = strings.TrimSpace(pkgSummary)

		absDir := filepath.Join(opts.RepoPath, pkg.RelDir)
		pkgMeta := map[string]any{
			"surface":      "package",
			"package_path": absDir,
			"rel_dir":      pkg.RelDir,
			"import_path":  pkg.ImportPath,
			"quality":      qualityTag(cl.generator.Model()),
		}
		pkgMetaJSON, _ := json.Marshal(pkgMeta)

		emb, err := Single(ctx, cl.embedder, pkgSummary)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("embed package %s: %w", pkg.RelDir, err))
			continue
		}

		pkgSubject := fmt.Sprintf("pkg:%s", opts.Subject)
		if pkg.RelDir != "." {
			pkgSubject = fmt.Sprintf("pkg:%s/%s", opts.Subject, filepath.ToSlash(pkg.RelDir))
		}

		pkgFact := Fact{
			Content:   pkgSummary,
			Subject:   pkgSubject,
			Category:  "project",
			Kind:      "pattern",
			Metadata:  pkgMetaJSON,
			Embedding: emb,
		}

		pkgID, err := cl.store.Insert(ctx, pkgFact)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("insert package %s: %w", pkg.RelDir, err))
			continue
		}
		result.Packages++
		pkgFactIDs[pkg.RelDir] = pkgID

		// Supersede old package fact.
		if old, ok := existingPackages[pkg.RelDir]; ok {
			if err := cl.store.Supersede(ctx, old.ID, pkgID); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("supersede package %s: %w", pkg.RelDir, err))
			} else {
				result.Superseded++
			}
		}

		// Link: package contains each file.
		for _, fr := range pkgFileResults[pkg.RelDir] {
			if _, err := cl.store.LinkFacts(ctx, pkgID, fr.factID, "contains", false, "", nil); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("link pkg->file %s: %w", fr.relPath, err))
			} else {
				result.Links++
			}
		}
	}

	// Create depends_on links between packages based on internal imports.
	importToPkgDir := make(map[string]string)
	for _, pkg := range packages {
		importToPkgDir[pkg.ImportPath] = pkg.RelDir
	}
	for _, pkg := range packages {
		srcPkgID, ok := pkgFactIDs[pkg.RelDir]
		if !ok {
			continue
		}
		// Collect all internal imports across files in this package.
		seen := make(map[string]bool)
		for _, f := range pkg.Files {
			for _, imp := range f.Imports {
				if targetDir, ok := importToPkgDir[imp]; ok && targetDir != pkg.RelDir && !seen[imp] {
					seen[imp] = true
					if targetID, ok := pkgFactIDs[targetDir]; ok {
						if _, err := cl.store.LinkFacts(ctx, srcPkgID, targetID, "depends_on", false, "", nil); err != nil {
							result.Errors = append(result.Errors, fmt.Errorf("link pkg depends_on %s->%s: %w", pkg.RelDir, targetDir, err))
						} else {
							result.Links++
						}
					}
				}
			}
		}
	}

	// Phase 4b: Markdown file processing
	mdFiles, mdErr := discoverMarkdown(opts.RepoPath, opts.MaxFileSizeBytes, opts.ExcludeDirs)
	if mdErr != nil {
		result.Errors = append(result.Errors, fmt.Errorf("discover markdown: %w", mdErr))
	}
	var mdDocFactIDs []int64
	var mdDocSummaries []string
	for _, mf := range mdFiles {
		mdResult, err := cl.LearnFile(ctx, LearnFileOpts{
			Subject:     opts.Subject,
			FilePath:    mf.RelPath,
			Content:     mf.Source,
			ContentHash: mf.ContentHash,
			Force:       opts.Force,
		})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("learn markdown %s: %w", mf.RelPath, err))
			continue
		}
		result.LLMCalls += mdResult.LLMCalls
		result.Superseded += mdResult.Superseded
		result.Links += mdResult.Links
		result.Sections += mdResult.Sections
		if mdResult.Skipped {
			result.Skipped++
			// Recover the summary from the existing fact for repo synthesis.
			if f, err := cl.store.Get(ctx, mdResult.FileFactID); err == nil && f != nil {
				mdDocSummaries = append(mdDocSummaries, f.Content)
				mdDocFactIDs = append(mdDocFactIDs, mdResult.FileFactID)
			}
		} else {
			result.Files++
			if f, err := cl.store.Get(ctx, mdResult.FileFactID); err == nil && f != nil {
				mdDocSummaries = append(mdDocSummaries, f.Content)
			}
			mdDocFactIDs = append(mdDocFactIDs, mdResult.FileFactID)
		}
	}

	// Phase 5: Repo-level synthesis
	var pkgSummaryLines []string
	for _, pkg := range packages {
		if id, ok := pkgFactIDs[pkg.RelDir]; ok {
			f, err := cl.store.Get(ctx, id)
			if err == nil && f != nil {
				label := pkg.ImportPath
				pkgSummaryLines = append(pkgSummaryLines, fmt.Sprintf("- %s: %s", label, f.Content))
			}
		}
	}

	// Include markdown doc summaries in repo-level synthesis.
	for _, s := range mdDocSummaries {
		pkgSummaryLines = append(pkgSummaryLines, fmt.Sprintf("- (doc) %s", s))
	}

	if len(pkgSummaryLines) > 0 {
		prompt := buildRepoSummaryPrompt(opts.Subject, modulePath, pkgSummaryLines)
		repoSummary, err := cl.generator.Generate(ctx, prompt)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("summarize repo: %w", err))
		} else {
			result.LLMCalls++
			repoSummary = strings.TrimSpace(repoSummary)

			repoMeta := map[string]any{
				"surface":      "project",
				"project_path": opts.RepoPath,
				"module_path":  modulePath,
				"quality":      qualityTag(cl.generator.Model()),
			}
			repoMetaJSON, _ := json.Marshal(repoMeta)

			emb, err := Single(ctx, cl.embedder, repoSummary)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("embed repo: %w", err))
			} else {
				repoSubject := fmt.Sprintf("repo:%s", opts.Subject)
				repoFact := Fact{
					Content:   repoSummary,
					Subject:   repoSubject,
					Category:  "project",
					Kind:      "pattern",
					Metadata:  repoMetaJSON,
					Embedding: emb,
				}

				repoID, err := cl.store.Insert(ctx, repoFact)
				if err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("insert repo: %w", err))
				} else {
					result.RepoFactID = repoID

					// Supersede old repo fact.
					oldRepos, _ := cl.store.List(ctx, QueryOpts{
						Subject:    repoSubject,
						OnlyActive: true,
						MetadataFilters: []MetadataFilter{
							{Key: "surface", Op: "=", Value: "project"},
						},
					})
					for _, old := range oldRepos {
						if old.ID != repoID {
							if err := cl.store.Supersede(ctx, old.ID, repoID); err != nil {
								result.Errors = append(result.Errors, fmt.Errorf("supersede repo: %w", err))
							} else {
								result.Superseded++
							}
						}
					}

					// Link: repo contains each package.
					for _, pkg := range packages {
						if pkgID, ok := pkgFactIDs[pkg.RelDir]; ok {
							if _, err := cl.store.LinkFacts(ctx, repoID, pkgID, "contains", false, "", nil); err != nil {
								result.Errors = append(result.Errors, fmt.Errorf("link repo->pkg %s: %w", pkg.RelDir, err))
							} else {
								result.Links++
							}
						}
					}

					// Link: repo contains each markdown doc.
					for _, docID := range mdDocFactIDs {
						if _, err := cl.store.LinkFacts(ctx, repoID, docID, "contains", false, "", nil); err != nil {
							result.Errors = append(result.Errors, fmt.Errorf("link repo->doc: %w", err))
						} else {
							result.Links++
						}
					}
				}
			}
		}
	}

	// Phase 6: Cross-file linking (doc ↔ file, doc ↔ doc).
	var crossRefs []LearnedFactRef
	for _, docID := range mdDocFactIDs {
		crossRefs = append(crossRefs, LearnedFactRef{FactID: docID, Surface: "doc"})
	}
	for _, pkg := range packages {
		for _, fr := range pkgFileResults[pkg.RelDir] {
			crossRefs = append(crossRefs, LearnedFactRef{FactID: fr.factID, Surface: "file", RelPath: fr.relPath})
		}
	}
	if len(crossRefs) > 1 {
		n, err := crossLinkFacts(ctx, cl.store, crossRefs, DefaultCrossLinkThreshold)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("cross-link: %w", err))
		} else {
			result.Links += n
		}
	}

	return result, nil
}

// qualityTag returns the quality metadata value for learned facts.
// If genModel is set, it returns "local:<model>"; otherwise just "local".
func qualityTag(genModel string) string {
	if genModel != "" {
		return "local:" + genModel
	}
	return "local"
}

// matchTestToSource returns the source symbol name and fact ID that a test
// function exercises, or ("", 0) if no match is found. Supports:
//   - Exact match: TestFoo → Foo
//   - Subtest suffix: TestFoo_Error → Foo
//   - Method expansion: TestDefaultStore_Get → (*DefaultStore).Get
func matchTestToSource(testName string, srcNames map[string]int64) (string, int64) {
	candidate := strings.TrimPrefix(testName, "Test")
	if candidate == "" || candidate == testName {
		return "", 0
	}

	// Exact match (e.g., TestCosineSimilarity → CosineSimilarity).
	if id, ok := srcNames[candidate]; ok {
		return candidate, id
	}

	// Strip subtest suffix (e.g., TestFoo_Error → Foo).
	base, _, hasSuffix := strings.Cut(candidate, "_")
	if hasSuffix {
		if id, ok := srcNames[base]; ok {
			return base, id
		}
	}

	// Method expansion (e.g., TestDefaultStore_Get → (*DefaultStore).Get).
	if parts := strings.SplitN(candidate, "_", 2); len(parts) == 2 {
		methodName := fmt.Sprintf("(*%s).%s", parts[0], parts[1])
		if id, ok := srcNames[methodName]; ok {
			return methodName, id
		}
	}

	return "", 0
}

// --- LLM prompt builders ---

func buildFileSummaryPrompt(relPath, pkgName, source string, symbolSigs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the Go source file %q (package %s) in 2-4 sentences.\n", relPath, pkgName)
	b.WriteString("Focus on: what this file does, key types/functions, and its role in the package.\n")
	b.WriteString("Return ONLY the summary text, no markdown formatting.\n\n")

	if len(symbolSigs) > 0 {
		b.WriteString("Exported symbols:\n")
		for _, sig := range symbolSigs {
			b.WriteString(sig)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	b.WriteString("Source:\n")
	// Truncate very long files to avoid context overflow.
	if len(source) > 16000 {
		b.WriteString(source[:16000])
		b.WriteString("\n... (truncated)\n")
	} else {
		b.WriteString(source)
	}
	return b.String()
}

func buildPackageSummaryPrompt(pkgName, importPath string, fileSummaries []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the Go package %q (import path: %s) in 2-4 sentences.\n", pkgName, importPath)
	b.WriteString("Focus on: the package's purpose, key abstractions, and how it fits into the project.\n")
	b.WriteString("Return ONLY the summary text, no markdown formatting.\n\n")
	b.WriteString("File summaries:\n")
	for _, s := range fileSummaries {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	return b.String()
}

func buildRepoSummaryPrompt(subject, modulePath string, pkgSummaries []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the Go project %q (module: %s) in 3-5 sentences.\n", subject, modulePath)
	b.WriteString("Focus on: the project's purpose, architecture, and key packages.\n")
	b.WriteString("Return ONLY the summary text, no markdown formatting.\n\n")
	b.WriteString("Package summaries:\n")
	for _, s := range pkgSummaries {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String()
}
