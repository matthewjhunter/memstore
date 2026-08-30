package memstore_test

import (
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// A chunk embedded as bare content is prose with nothing saying what it is or
// where it came from. The fact side already solved this (FactEmbedText applies
// a subject header); documents carry richer structure and had none of it
// reaching the embedder, because until now nothing embedded them at all.
func TestChunkEmbedText_CarriesStructuralIdentity(t *testing.T) {
	code := memstore.DocumentChunk{
		Content: "func Parse(b []byte) (*AST, error) { ... }",
		Package: "parser", Symbol: "Parse", DeclKind: "func",
		ScopePath: "parser > Parse",
	}
	got := memstore.ChunkEmbedText("nomic-embed-text", "parser/parse.go", code, code.Content)
	for _, want := range []string{"parse.go", "parser > Parse", "func", "func Parse(b []byte)"} {
		if !strings.Contains(got, want) {
			t.Errorf("code chunk embed text missing %q:\n%s", want, got)
		}
	}

	prose := memstore.DocumentChunk{
		Content:     "Recall is flat from 800 tokens down to 200.",
		HeadingPath: "Results > Chunk size",
	}
	got = memstore.ChunkEmbedText("nomic-embed-text", "evaluating-chunking.md", prose, prose.Content)
	for _, want := range []string{"evaluating-chunking.md", "Results > Chunk size", "Recall is flat"} {
		if !strings.Contains(got, want) {
			t.Errorf("prose chunk embed text missing %q:\n%s", want, got)
		}
	}
}

// The document side must render under the same task as the fact side, or
// stored vectors and query vectors sit in spaces the model was trained to keep
// apart -- which fails silently, as the comment on FactQueryText warns.
func TestChunkEmbedText_UsesTheDocumentTask(t *testing.T) {
	c := memstore.DocumentChunk{Content: "body"}
	doc := memstore.ChunkEmbedText("nomic-embed-text", "a.md", c, c.Content)
	fact := memstore.FactEmbedText("nomic-embed-text", "subject", "body")

	docPrefix, _, _ := strings.Cut(doc, ":")
	factPrefix, _, _ := strings.Cut(fact, ":")
	if docPrefix != factPrefix {
		t.Errorf("document task prefix %q != fact task prefix %q; the two corpora would not be comparable", docPrefix, factPrefix)
	}
	// And the query side is shared, not duplicated.
	if q := memstore.FactQueryText("nomic-embed-text", "a query"); !strings.HasPrefix(q, "search_query") {
		t.Errorf("query text = %q", q)
	}
}

// A chunk with no structure at all still embeds; the header is simply absent.
func TestChunkEmbedText_BareChunk(t *testing.T) {
	c := memstore.DocumentChunk{Content: "just some text"}
	got := memstore.ChunkEmbedText("", "", c, c.Content)
	if !strings.Contains(got, "just some text") {
		t.Errorf("bare chunk lost its content: %q", got)
	}
}
