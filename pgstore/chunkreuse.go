package pgstore

// Vector reuse across re-ingest.
//
// Re-ingest replaces a document's whole chunk set -- the corpus design is
// explicit that chunks are replaced, not merged -- so before this every chunk
// of a changed file was re-embedded even when its own bytes never moved. On a
// repo sync that is most of the embedding work in the run and all of it is
// waste: the same text under the same header renders the same input to the
// same model, so the vector that comes back is the one already stored.
//
// What makes it safe is the store-level embedder fingerprint. reconcileEmbedder
// refuses to open when the model or dimension changed, and clears every vector
// when the recipe changed, so vectors already in the table are always from the
// embedder currently configured. Reuse would be unsound without that guarantee
// and must not outlive it.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/matthewjhunter/memstore"
)

// chunkReuseKey identifies a chunk by everything that reaches the embedder,
// and by nothing else.
//
// That is the whole subtlety. ChunkEmbedText renders the section heading, the
// symbol scope and the declaration kind into a header ahead of the body, so
// two chunks with identical text under different headings are different inputs
// and must not share a vector. Ordinal and the byte and line offsets are
// deliberately absent: a chunk that shifted down the file because something
// above it grew is the same input to the model, and re-embedding it would be
// the waste this exists to remove.
//
// The document path is in the key even though reusableChunkVectors already
// scopes its query to one document, so the two are belt and braces. The path
// is the header's source field, so a key without it would silently permit
// cross-document reuse the moment someone widened that query -- and the
// resulting vector would carry another file's name in its header.
//
// Fields are length-prefixed rather than delimited, so no field value can
// impersonate a boundary between two others.
func chunkReuseKey(docPath string, c memstore.DocumentChunk) string {
	h := sha256.New()
	for _, f := range []string{docPath, c.Content, c.HeadingPath, c.ScopePath, c.Symbol, c.DeclKind} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reusableChunkVectors maps reuse key to stored vector for one document's
// currently stored chunks. Called inside the upsert transaction, before the
// old chunk set is deleted.
func reusableChunkVectors(ctx context.Context, tx pgx.Tx, documentID int64, docPath string) (map[string]pgvector.Vector, error) {
	rows, err := tx.Query(ctx,
		`SELECT content, heading_path, scope_path, symbol, decl_kind, embedding
		   FROM memstore_document_chunks
		  WHERE document_id = $1 AND embedding IS NOT NULL`, documentID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: reading chunk vectors for reuse: %w", err)
	}
	defer rows.Close()

	out := map[string]pgvector.Vector{}
	for rows.Next() {
		var c memstore.DocumentChunk
		var v pgvector.Vector
		if err := rows.Scan(&c.Content, &c.HeadingPath, &c.ScopePath, &c.Symbol, &c.DeclKind, &v); err != nil {
			return nil, fmt.Errorf("pgstore: scanning chunk vector for reuse: %w", err)
		}
		out[chunkReuseKey(docPath, c)] = v
	}
	return out, rows.Err()
}

// reusedVector returns the stored vector for a chunk whose embedder input is
// unchanged, or nil to leave the column NULL so the queue embeds it.
func reusedVector(reuse map[string]pgvector.Vector, docPath string, c memstore.DocumentChunk) any {
	if v, ok := reuse[chunkReuseKey(docPath, c)]; ok {
		return v
	}
	return nil
}
