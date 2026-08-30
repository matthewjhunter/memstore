package memstore

import (
	embedding "github.com/matthewjhunter/go-embedding"
)

// Embedding text for document chunks.
//
// The fact side established the shape in factembed.go: a task prefix the model
// was trained to key off, then a small header naming what the text is, then
// the body. Documents get the same treatment for the same reason, and it
// matters more here -- a chunk of a file is a fragment by construction, and a
// mid-file fragment with no identity is prose the embedder has to guess the
// topic of.
//
// This is the pattern task 3135 describes (GAIA prefixes each code chunk with
// "function: parse_python_file"), and DocumentChunk's own doc comment already
// promised it: the heading and scope fields are "derived context assembled
// into embed text at embed time; they are never prepended to Content". That
// promise could not be kept while nothing embedded documents at all.

// chunkFields is the header applied to a document chunk: where it came from,
// and what it is within that file.
//
// Kept to at most three short fields on purpose. The header is charged against
// the embedder's input budget along with the body, and the Chroma chunking
// measurements say precision comes from chunks being small -- a header that
// grows with the file's structure would spend that budget on ancestry rather
// than content.
func chunkFields(docPath string, c DocumentChunk) []embedding.Field {
	var f []embedding.Field
	if docPath != "" {
		f = append(f, embedding.Field{Key: "source", Value: docPath})
	}
	switch {
	case c.ScopePath != "":
		f = append(f, embedding.Field{Key: "symbol", Value: c.ScopePath})
	case c.Symbol != "":
		f = append(f, embedding.Field{Key: "symbol", Value: c.Symbol})
	case c.HeadingPath != "":
		f = append(f, embedding.Field{Key: "section", Value: c.HeadingPath})
	}
	if c.DeclKind != "" {
		f = append(f, embedding.Field{Key: "kind", Value: c.DeclKind})
	}
	return f
}

// ChunkEmbedText renders what is sent to the model for one document chunk:
// the document task prefix, the structural header, then the body.
//
// body is passed separately from c.Content so a caller that had to shorten an
// oversized chunk sends what it actually embedded rather than the original.
//
// The task must match FactEmbedText's. Both corpora are searched with queries
// rendered by FactQueryText, so a document vector produced under a different
// task would be compared across a boundary the model was trained to
// distinguish -- which does not fail, it just retrieves worse, quietly.
func ChunkEmbedText(model, docPath string, c DocumentChunk, body string) string {
	return embedding.FormatRecordForTask(model, embedding.TaskRetrievalDocument,
		chunkFields(docPath, c), body)
}
