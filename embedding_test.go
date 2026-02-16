package memstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore"
)

type mockEmbedder struct {
	dim       int
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

func TestSingle(t *testing.T) {
	e := &mockEmbedder{dim: 4}
	result, err := memstore.Single(context.Background(), e, "hello")
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	if len(result) != 4 {
		t.Errorf("got %d dims, want 4", len(result))
	}
}

func TestSingle_Error(t *testing.T) {
	e := &mockEmbedder{dim: 4, err: fmt.Errorf("service down")}
	_, err := memstore.Single(context.Background(), e, "hello")
	if err == nil {
		t.Error("expected error from failing embedder")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	original := []float32{1.0, -2.5, 3.14159, 0, math.MaxFloat32}
	encoded := memstore.EncodeFloat32s(original)

	if len(encoded) != len(original)*4 {
		t.Fatalf("encoded length = %d, want %d", len(encoded), len(original)*4)
	}

	decoded := memstore.DecodeFloat32s(encoded)
	if len(decoded) != len(original) {
		t.Fatalf("decoded length = %d, want %d", len(decoded), len(original))
	}

	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("index %d: got %f, want %f", i, decoded[i], original[i])
		}
	}
}

func TestEncodeEmpty(t *testing.T) {
	encoded := memstore.EncodeFloat32s(nil)
	if len(encoded) != 0 {
		t.Errorf("nil encode: got %d bytes, want 0", len(encoded))
	}
	decoded := memstore.DecodeFloat32s(nil)
	if len(decoded) != 0 {
		t.Errorf("nil decode: got %d elements, want 0", len(decoded))
	}
}

func TestCosineSimilarity_Identical(t *testing.T) {
	v := []float32{1, 2, 3}
	sim := memstore.CosineSimilarity(v, v)
	if math.Abs(sim-1.0) > 1e-6 {
		t.Errorf("identical vectors: got %f, want 1.0", sim)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	sim := memstore.CosineSimilarity(a, b)
	if math.Abs(sim) > 1e-6 {
		t.Errorf("orthogonal vectors: got %f, want 0.0", sim)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{-1, 0, 0}
	sim := memstore.CosineSimilarity(a, b)
	if math.Abs(sim+1.0) > 1e-6 {
		t.Errorf("opposite vectors: got %f, want -1.0", sim)
	}
}

func TestCosineSimilarity_Partial(t *testing.T) {
	a := []float32{1, 0, 0, 0}
	b := []float32{0.9, 0.1, 0, 0}
	sim := memstore.CosineSimilarity(a, b)
	if sim <= 0.9 || sim >= 1.0 {
		t.Errorf("partial similarity = %f, expected between 0.9 and 1.0", sim)
	}
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{1, 2, 3}
	if sim := memstore.CosineSimilarity(a, b); sim != 0 {
		t.Errorf("zero vector: got %f, want 0", sim)
	}
}

func TestCosineSimilarity_DifferentLengths(t *testing.T) {
	a := []float32{1, 2}
	b := []float32{1, 2, 3}
	if sim := memstore.CosineSimilarity(a, b); sim != 0 {
		t.Errorf("different lengths: got %f, want 0", sim)
	}
}

func TestCosineSimilarity_Empty(t *testing.T) {
	if sim := memstore.CosineSimilarity(nil, nil); sim != 0 {
		t.Errorf("nil vectors: got %f, want 0", sim)
	}
}

// -- OllamaEmbedder tests --

func TestOllamaEmbedder(t *testing.T) {
	wantModel := "embeddinggemma"
	wantVec := []float32{0.1, 0.2, 0.3}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s, want /api/embed", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != wantModel {
			t.Errorf("model = %s, want %s", req.Model, wantModel)
		}
		if len(req.Input) != 2 {
			t.Fatalf("input count = %d, want 2", len(req.Input))
		}

		resp := struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{
			Embeddings: [][]float32{wantVec, wantVec},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := memstore.NewOllamaEmbedder(srv.URL, wantModel)
	results, err := e.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if len(results[0]) != 3 {
		t.Errorf("dim = %d, want 3", len(results[0]))
	}
}

func TestOllamaEmbedder_Single(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{
			Embeddings: [][]float32{{0.5, 0.6}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := memstore.NewOllamaEmbedder(srv.URL, "test")
	result, err := memstore.Single(context.Background(), e, "hello")
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("dim = %d, want 2", len(result))
	}
}

func TestOllamaEmbedder_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := memstore.NewOllamaEmbedder(srv.URL, "nonexistent")
	_, err := e.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Error("expected error for HTTP 404")
	}
}

func TestOllamaEmbedder_ConnectionRefused(t *testing.T) {
	e := memstore.NewOllamaEmbedder("http://localhost:1", "test")
	_, err := e.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Error("expected error for connection refused")
	}
}
