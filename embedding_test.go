package memstore_test

import (
	"context"

	"github.com/matthewjhunter/go-embedding"
)

// mockEmbedder is a deterministic in-memory Embedder for tests in package
// memstore_test. Each input text gets a vector of length dim where the j-th
// component is (i+1)*0.1*(j+1) — distinct per input position, so cosine
// similarity differs across inputs.
type mockEmbedder struct {
	dim       int
	model     string
	callCount int
	err       error
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
	result := make([][]float32, len(texts))
	for i := range texts {
		emb := make([]float32, m.dim)
		for j := range emb {
			emb[j] = float32(i+1) * 0.1 * float32(j+1)
		}
		result[i] = emb
	}
	return result, nil
}

func (m *mockEmbedder) Model() string {
	if m.model != "" {
		return m.model
	}
	return "mock"
}

func (m *mockEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: m.Model(), Dim: m.dim}
}
