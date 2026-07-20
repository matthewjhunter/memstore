// Package gosrc implements the Go source chunker from
// docs/code-chunking.md: cAST-style split-then-merge over the stdlib AST.
// The unit is the top-level declaration, spans start at the doc comment,
// oversized declarations recurse into statement or spec boundaries, and
// undersized siblings merge. go/ast over tree-sitter: no cgo, doc comments
// attached, receivers and export status known exactly.
package gosrc

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/matthewjhunter/memstore/chunk"
)

// Version identifies boundary-affecting behavior. Bump on any change that
// can move a cut, including toolchain-driven parser changes.
const Version = 1

// ErrUnparseable reports a file go/parser rejects -- a syntax error, or
// language features newer than this toolchain. The caller falls back to
// line-window chunking and records the strategy on the document, so the
// fallback is visible and re-ingest can retry when the toolchain catches up.
var ErrUnparseable = errors.New("gosrc: file does not parse")

// generatedRe is the standard marker from https://golang.org/s/generatedcode.
var generatedRe = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// Chunker chunks Go source files.
type Chunker struct{}

// New returns a Go source chunker.
func New() *Chunker { return &Chunker{} }

// Version implements chunk.Chunker.
func (c *Chunker) Version() int { return Version }

// Strategy implements chunk.Chunker.
func (c *Chunker) Strategy() string { return "go" }

var _ chunk.Chunker = (*Chunker)(nil)

// decl is one top-level declaration located by absolute byte span.
type decl struct {
	start, end int // [start, end); start includes the doc comment
	weight     int

	symbol   string
	receiver string
	kind     string
	exported bool
	sig      string

	node ast.Decl
}

// Chunk implements chunk.Chunker. path informs derived metadata (import
// path) only; boundaries are a pure function of content.
func (c *Chunker) Chunk(filePath string, content []byte) (*chunk.Result, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
	tf := fset.File(f.Pos())
	off := func(p token.Pos) int { return tf.Offset(p) }

	res := &chunk.Result{Generated: generatedRe.Match(content)}
	ix := chunk.NewLineIndex(content)
	pkgName := f.Name.Name
	importPath := ""
	if dir := path.Dir(filePath); dir != "." && dir != "/" {
		importPath = dir
	}
	imports := importAliases(f)

	emit := func(start, end int, d decl, nodes []ast.Node) {
		end = chunk.TrimTrailingWS(content, start, end)
		if end <= start {
			return
		}
		scope := pkgName
		if d.receiver != "" {
			scope += " > " + d.receiver
		}
		if d.symbol != "" && d.symbol != pkgName {
			scope += " > " + d.symbol
		}
		res.Chunks = append(res.Chunks, chunk.Chunk{
			Ordinal:     len(res.Chunks),
			Content:     string(content[start:end]),
			ByteStart:   start,
			ByteEnd:     end,
			LineStart:   ix.Line(start),
			LineEnd:     ix.Line(end - 1),
			Package:     pkgName,
			ImportPath:  importPath,
			Symbol:      d.symbol,
			Receiver:    d.receiver,
			DeclKind:    d.kind,
			Exported:    d.exported,
			Signature:   d.sig,
			ScopePath:   scope,
			ImportsUsed: importsUsed(nodes, imports),
		})
	}

	// Header chunk: everything from the start of the file (copyright,
	// build tags, package doc) through the last leading import block. For a
	// doc.go this is the whole file.
	rest := f.Decls
	headerEnd := off(f.Name.End())
	headerKind := "package_doc"
	for len(rest) > 0 {
		g, ok := rest[0].(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT {
			break
		}
		headerEnd = off(g.End())
		headerKind = "import"
		rest = rest[1:]
	}
	emit(0, headerEnd, decl{
		symbol: pkgName,
		kind:   headerKind,
		sig:    "package " + pkgName,
	}, nil)

	decls := make([]decl, 0, len(rest))
	for _, d := range rest {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			// A stray import block below other decls: its own chunk.
			decls = append(decls, locate(content, off, d, "import"))
			continue
		}
		decls = append(decls, locate(content, off, d, ""))
	}

	for i := 0; i < len(decls); i++ {
		d := decls[i]
		switch {
		case d.weight > chunk.Max:
			c.emitOversized(content, off, emit, d)
		case d.weight < chunk.Min:
			// Merge with following siblings up to Target. A file of
			// one-line accessors becomes a few chunks, not forty.
			start, end := d.start, d.end
			weight := d.weight
			nodes := []ast.Node{d.node}
			for i+1 < len(decls) && weight < chunk.Min && weight+decls[i+1].weight <= chunk.Target {
				i++
				end = decls[i].end
				weight += decls[i].weight
				nodes = append(nodes, decls[i].node)
			}
			emit(start, end, d, nodes)
		default:
			emit(d.start, d.end, d, []ast.Node{d.node})
		}
	}

	return res, nil
}

// locate builds the decl record for one top-level declaration. The span
// starts at Doc.Pos(): in idiomatic Go the doc comment is the most
// retrievable prose in the file, and separating it from its declaration
// would put the question and the answer in different chunks.
func locate(content []byte, off func(token.Pos) int, d ast.Decl, kindOverride string) decl {
	rec := decl{node: d}
	startPos := d.Pos()

	switch n := d.(type) {
	case *ast.FuncDecl:
		if n.Doc != nil {
			startPos = n.Doc.Pos()
		}
		rec.symbol = n.Name.Name
		rec.exported = ast.IsExported(n.Name.Name)
		rec.kind = "func"
		if n.Recv != nil && len(n.Recv.List) > 0 {
			rec.kind = "method"
			rec.receiver = strings.TrimPrefix(string(content[off(n.Recv.List[0].Type.Pos()):off(n.Recv.List[0].Type.End())]), "*")
		}
		rec.sig = oneLine(content[off(n.Pos()):off(n.Type.End())])
	case *ast.GenDecl:
		if n.Doc != nil {
			startPos = n.Doc.Pos()
		}
		switch n.Tok {
		case token.TYPE:
			rec.kind = "type"
		case token.CONST:
			rec.kind = "const"
		case token.VAR:
			rec.kind = "var"
		case token.IMPORT:
			rec.kind = "import"
		}
		if name, exported := firstSpecName(n); name != "" {
			rec.symbol = name
			rec.exported = exported
		}
		rec.sig = oneLine(firstLineOf(content[off(n.Pos()):off(n.End())]))
	default:
		rec.kind = "decl"
	}
	if kindOverride != "" {
		rec.kind = kindOverride
	}

	rec.start = off(startPos)
	rec.end = off(d.End())
	rec.weight = chunk.Weight(content[rec.start:rec.end])
	return rec
}

// emitOversized recurses into an oversized declaration, cAST-style: a long
// function splits at top-level statement boundaries in its body, a large
// const/var/type block at spec boundaries. Never inside a statement or
// composite literal -- an atomic child over Max is emitted oversized.
func (c *Chunker) emitOversized(content []byte, off func(token.Pos) int, emit func(int, int, decl, []ast.Node), d decl) {
	var boundaries []int // candidate cut offsets, each the start of a child
	switch n := d.node.(type) {
	case *ast.FuncDecl:
		if n.Body != nil {
			for _, stmt := range n.Body.List {
				boundaries = append(boundaries, off(stmt.Pos()))
			}
		}
	case *ast.GenDecl:
		for _, spec := range n.Specs {
			boundaries = append(boundaries, off(spec.Pos()))
		}
	}
	if len(boundaries) < 2 {
		emit(d.start, d.end, d, []ast.Node{d.node})
		return
	}

	// Greedy pack: pieces grow toward Target and cut at the next child
	// boundary. The first piece carries the doc comment and signature by
	// construction (it starts at d.start); the last runs to d.end so the
	// closing brace stays attached.
	pieceStart := d.start
	for idx := 1; idx < len(boundaries); idx++ {
		w := chunk.Weight(content[pieceStart:boundaries[idx]])
		if w >= chunk.Target {
			emit(pieceStart, boundaries[idx], d, []ast.Node{d.node})
			pieceStart = boundaries[idx]
		}
	}
	emit(pieceStart, d.end, d, []ast.Node{d.node})
}

// importAliases maps the identifier a file refers to an import by (explicit
// alias, or last path element) to the import path.
func importAliases(f *ast.File) map[string]string {
	m := make(map[string]string, len(f.Imports))
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
			if name == "_" || name == "." {
				continue
			}
		}
		m[name] = p
	}
	return m
}

// importsUsed reports which imports the given subtrees actually reference,
// as a sorted list capped at ten -- a genuine signal about what a function
// does that its body alone may not make obvious.
func importsUsed(nodes []ast.Node, aliases map[string]string) []string {
	if len(nodes) == 0 || len(aliases) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok {
				if p, ok := aliases[id.Name]; ok {
					seen[p] = true
				}
			}
			return true
		})
	}
	if len(seen) == 0 {
		return nil
	}
	used := make([]string, 0, len(seen))
	for p := range seen {
		used = append(used, p)
	}
	sort.Strings(used)
	if len(used) > 10 {
		used = used[:10]
	}
	return used
}

func firstSpecName(g *ast.GenDecl) (string, bool) {
	for _, spec := range g.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			return s.Name.Name, ast.IsExported(s.Name.Name)
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return s.Names[0].Name, ast.IsExported(s.Names[0].Name)
			}
		}
	}
	return "", false
}

func firstLineOf(b []byte) []byte {
	if i := strings.IndexByte(string(b), '\n'); i >= 0 {
		return b[:i]
	}
	return b
}

func oneLine(b []byte) string {
	return strings.Join(strings.Fields(string(b)), " ")
}
