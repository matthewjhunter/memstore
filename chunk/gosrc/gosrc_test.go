package gosrc_test

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/chunk"
	"github.com/matthewjhunter/memstore/chunk/gosrc"
)

func mustChunk(t *testing.T, path, content string) *chunk.Result {
	t.Helper()
	res, err := gosrc.New().Chunk(path, []byte(content))
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	return res
}

func verifySpans(t *testing.T, content string, chunks []chunk.Chunk) {
	t.Helper()
	prevStart, prevEnd := -1, 0
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Errorf("chunk %d: ordinal %d", i, c.Ordinal)
		}
		if c.ByteStart <= prevStart || c.ByteStart < prevEnd {
			t.Errorf("chunk %d: span [%d,%d) does not advance past [%d,%d)", i, c.ByteStart, c.ByteEnd, prevStart, prevEnd)
		}
		if c.ByteEnd <= c.ByteStart || c.ByteEnd > len(content) {
			t.Fatalf("chunk %d: bad span [%d,%d)", i, c.ByteStart, c.ByteEnd)
		}
		if c.Content != content[c.ByteStart:c.ByteEnd] {
			t.Errorf("chunk %d violates the verbatim invariant", i)
		}
		prevStart, prevEnd = c.ByteStart, c.ByteEnd
	}
}

const sample = `// Copyright notice.

// Package sample does sample things.
package sample

import (
	"fmt"
	"strings"
)

// Greeting is the exported greeting format.
const Greeting = "hello %s"

// Greeter greets people by name with a stored prefix string value.
type Greeter struct {
	prefix string
}

// Greet returns the greeting for name, using the configured prefix and
// the package-level format string to produce a printable message.
func (g *Greeter) Greet(name string) string {
	upper := strings.ToUpper(name)
	return fmt.Sprintf(Greeting, g.prefix+upper)
}

// helper is unexported and formats a plain string for internal usage only.
func helper(s string) string {
	return strings.TrimSpace(s)
}
`

func TestGosrc_DeclarationChunks(t *testing.T) {
	res := mustChunk(t, "pkg/sample/sample.go", sample)
	verifySpans(t, sample, res.Chunks)

	if res.Generated {
		t.Error("plain file marked generated")
	}

	// Header chunk: file start through the import block.
	h := res.Chunks[0]
	if h.DeclKind != "import" || h.ByteStart != 0 {
		t.Errorf("header chunk wrong: %+v", h)
	}
	for _, want := range []string{"Copyright notice", "Package sample does sample things", `"strings"`} {
		if !strings.Contains(h.Content, want) {
			t.Errorf("header chunk missing %q", want)
		}
	}
	if h.Package != "sample" || h.ImportPath != "pkg/sample" {
		t.Errorf("header package identity wrong: %+v", h)
	}

	// The remaining decls are all under Min, so they merge -- but every doc
	// comment must sit in the same chunk as its declaration.
	all := ""
	for _, c := range res.Chunks {
		all += c.Content + "\n---\n"
	}
	pairs := [][2]string{
		{"// Greeting is the exported", "const Greeting"},
		{"// Greeter greets people", "type Greeter struct"},
		{"// Greet returns the greeting", "func (g *Greeter) Greet"},
		{"// helper is unexported", "func helper"},
	}
	for _, p := range pairs {
		found := false
		for _, c := range res.Chunks {
			hasDoc := strings.Contains(c.Content, p[0])
			hasDecl := strings.Contains(c.Content, p[1])
			if hasDoc != hasDecl {
				t.Errorf("doc comment and declaration separated across chunks: %q / %q", p[0], p[1])
			}
			if hasDoc && hasDecl {
				found = true
			}
		}
		if !found {
			t.Errorf("declaration %q not found in any chunk", p[1])
		}
	}
}

func TestGosrc_DerivedColumns(t *testing.T) {
	// Pad the method's doc so it stands alone: big enough that no preceding
	// undersized sibling can absorb it within Target.
	pad := strings.Repeat("// Detailed explanation line with many words about the method behavior.\n", 40)
	content := strings.Replace(sample,
		"// Greet returns the greeting for name, using the configured prefix and\n",
		pad+"// Greet returns the greeting for name, using the configured prefix and\n", 1)

	res := mustChunk(t, "pkg/sample/sample.go", content)
	verifySpans(t, content, res.Chunks)

	var greet *chunk.Chunk
	for i := range res.Chunks {
		if res.Chunks[i].Symbol == "Greet" {
			greet = &res.Chunks[i]
			break
		}
	}
	if greet == nil {
		t.Fatalf("no chunk with symbol Greet: %+v", res.Chunks)
	}
	if greet.DeclKind != "method" || greet.Receiver != "Greeter" || !greet.Exported {
		t.Errorf("method identity wrong: %+v", greet)
	}
	if greet.ScopePath != "sample > Greeter > Greet" {
		t.Errorf("scope_path = %q", greet.ScopePath)
	}
	if greet.Signature != "func (g *Greeter) Greet(name string) string" {
		t.Errorf("signature = %q", greet.Signature)
	}
	for _, imp := range []string{"fmt", "strings"} {
		found := false
		for _, u := range greet.ImportsUsed {
			if u == imp {
				found = true
			}
		}
		if !found {
			t.Errorf("imports_used missing %q: %v", imp, greet.ImportsUsed)
		}
	}
}

func TestGosrc_SmallDeclsMerge(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package accessors\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "func Get%02d() int { return %d }\n\n", i, i)
	}
	content := sb.String()

	res := mustChunk(t, "accessors.go", content)
	verifySpans(t, content, res.Chunks)
	// 40 one-liners plus header; merging should collapse the one-liners
	// dramatically.
	if got := len(res.Chunks); got > 6 {
		t.Errorf("40 one-line accessors produced %d chunks; merging is not working", got)
	}
}

func TestGosrc_OversizedFunctionSplitsAtStatements(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&body, "\tvalue%03d := transformInputIntoOutput(value, %d) // stmt with width\n", i, i)
	}
	content := "package big\n\n// Process is a very long function used to test splitting.\nfunc Process(value int) {\n" + body.String() + "}\n"

	res := mustChunk(t, "big.go", content)
	verifySpans(t, content, res.Chunks)

	var pieces []chunk.Chunk
	for _, c := range res.Chunks {
		if c.Symbol == "Process" {
			pieces = append(pieces, c)
		}
	}
	if len(pieces) < 2 {
		t.Fatalf("oversized function did not split: %d pieces", len(pieces))
	}
	if !strings.Contains(pieces[0].Content, "// Process is a very long function") ||
		!strings.Contains(pieces[0].Content, "func Process(value int) {") {
		t.Error("first piece lost the doc comment or signature")
	}
	for i, p := range pieces {
		if w := chunk.Weight([]byte(p.Content)); w > chunk.Max {
			t.Errorf("piece %d weight %d exceeds max", i, w)
		}
		// Statement boundaries: each piece after the first starts at a
		// statement start.
		if i > 0 && !strings.HasPrefix(strings.TrimLeft(p.Content, "\t "), "value") {
			t.Errorf("piece %d does not start at a statement boundary: %.40q", i, p.Content)
		}
		if p.Signature == "" || p.DeclKind != "func" {
			t.Errorf("piece %d lost decl identity: %+v", i, p)
		}
	}
	last := pieces[len(pieces)-1]
	if !strings.HasSuffix(strings.TrimSpace(last.Content), "}") {
		t.Error("last piece lost the closing brace")
	}
}

// Whole-declaration chunks must re-parse as a valid declaration list.
// Statement-level pieces from an oversized body are exempt (and none exist in
// this input).
func TestGosrc_ChunksReparse(t *testing.T) {
	res := mustChunk(t, "sample.go", sample)
	for i, c := range res.Chunks {
		src := c.Content
		if c.DeclKind == "import" || c.DeclKind == "package_doc" {
			// The header already contains the package clause.
			if !strings.Contains(src, "package sample") {
				t.Errorf("header chunk lost the package clause")
			}
			continue
		}
		wrapped := "package p\n\n" + src
		if _, err := parser.ParseFile(token.NewFileSet(), "chunk.go", wrapped, parser.ParseComments); err != nil {
			t.Errorf("chunk %d does not re-parse as a declaration list: %v\n%s", i, err, src)
		}
	}
}

func TestGosrc_GeneratedDetection(t *testing.T) {
	content := "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage gen\n\nvar X = 1\n"
	res := mustChunk(t, "gen.pb.go", content)
	if !res.Generated {
		t.Error("generated marker not detected")
	}

	// The marker must match the standard form exactly, not any mention.
	content2 := "package gen\n\n// This file is not code generated, honest. DO NOT EDIT casually.\nvar X = 1\n"
	res2 := mustChunk(t, "gen.go", content2)
	if res2.Generated {
		t.Error("false positive generated detection")
	}
}

func TestGosrc_UnparseableFallsBack(t *testing.T) {
	_, err := gosrc.New().Chunk("broken.go", []byte("package broken\n\nfunc unclosed( {\n"))
	if !errors.Is(err, gosrc.ErrUnparseable) {
		t.Fatalf("expected ErrUnparseable, got %v", err)
	}
}

func TestGosrc_Deterministic(t *testing.T) {
	content, err := os.ReadFile("gosrc.go")
	if err != nil {
		t.Fatalf("reading own source: %v", err)
	}
	c := gosrc.New()
	a, err := c.Chunk("a/gosrc.go", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	b, err := c.Chunk("a/gosrc.go", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("re-chunking identical bytes produced different output")
	}
}

// TestGosrc_OwnSource is the real-corpus check: chunk this package's own
// implementation and hold the invariants.
func TestGosrc_OwnSource(t *testing.T) {
	content, err := os.ReadFile("gosrc.go")
	if err != nil {
		t.Fatalf("reading own source: %v", err)
	}
	res, err := gosrc.New().Chunk("chunk/gosrc/gosrc.go", content)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	verifySpans(t, string(content), res.Chunks)
	if len(res.Chunks) < 5 {
		t.Errorf("suspiciously few chunks: %d", len(res.Chunks))
	}
	if res.Chunks[0].DeclKind != "import" {
		t.Errorf("first chunk is not the header: %+v", res.Chunks[0])
	}
}
