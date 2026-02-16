// Package memstore provides a shared fact/knowledge store with FTS5 hybrid
// search and vector embeddings, backed by SQLite. The caller provides the
// *sql.DB; memstore creates its own namespaced tables (memstore_*).
//
// # Conventions
//
// Relationship facts are directional: a fact like "Alice trusts Bob" with
// Subject "Alice" is only indexed under Alice. Searching for "Bob" depends
// on FTS matching "Bob" in the content text, which is fragile.
//
// To ensure reliable lookup from either side of a relationship, store both
// directions at insert time:
//
//	{Content: "Alice trusts Bob",        Subject: "Alice", Category: "relationship"}
//	{Content: "Bob is trusted by Alice", Subject: "Bob",   Category: "relationship"}
//
// This gives each direction its own FTS entry and embedding, so searches
// from either subject work naturally. The caller controls the inverse
// phrasing, which varies by relationship type.
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

// embedMaxRetries is the number of retries for transient embedding failures
// (e.g. model loading timeouts). Total attempts = embedMaxRetries + 1.
const embedMaxRetries = 2

// embedWithRetry calls e.Embed, retrying up to embedMaxRetries times on
// failure. Returns immediately on context cancellation.
func embedWithRetry(ctx context.Context, e Embedder, texts []string) ([][]float32, error) {
	var result [][]float32
	var err error
	for attempt := range embedMaxRetries + 1 {
		result, err = e.Embed(ctx, texts)
		if err == nil {
			return result, nil
		}
		if attempt < embedMaxRetries && ctx.Err() != nil {
			break // caller gave up; don't burn retries
		}
	}
	return nil, fmt.Errorf("memstore: embedding failed after %d attempts: %w", embedMaxRetries+1, err)
}

// Single embeds a single text using the given Embedder, with retries.
func Single(ctx context.Context, e Embedder, text string) ([]float32, error) {
	results, err := embedWithRetry(ctx, e, []string{text})
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
