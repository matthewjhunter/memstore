// Package memstore provides a shared fact/knowledge store with FTS5 hybrid
// search and vector embeddings, backed by SQLite. The caller provides the
// *sql.DB; memstore creates its own namespaced tables (memstore_*).
package memstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
)

// Embedder produces vector embeddings for text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model returns a stable identifier for the embedding model (e.g.
	// "embeddinggemma"). The store records this on first use and rejects
	// mismatched embedders on subsequent opens.
	Model() string
}

// Single embeds a single text using the given Embedder.
func Single(ctx context.Context, e Embedder, text string) ([]float32, error) {
	results, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("memstore: empty embedding response")
	}
	return results[0], nil
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 if the vectors differ in length, are empty, or have zero magnitude.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// EncodeFloat32s serializes a float32 slice to a little-endian byte slice,
// suitable for storing as a BLOB in SQLite.
func EncodeFloat32s(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// DecodeFloat32s deserializes a little-endian byte slice back to a float32 slice.
func DecodeFloat32s(buf []byte) []float32 {
	n := len(buf) / 4
	v := make([]float32, n)
	for i := range n {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return v
}
