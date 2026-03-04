package memstore_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore"
)

func makeHTTPEmbedServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPEmbedder_Basic(t *testing.T) {
	srv := makeHTTPEmbedServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("expected model test-model, got %s", req.Model)
		}

		resp := map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{0.1, 0.2, 0.3}},
				{"index": 1, "embedding": []float32{0.4, 0.5, 0.6}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	emb := memstore.NewHTTPEmbedder(memstore.HTTPEmbedderConfig{
		URL:   srv.URL,
		Model: "test-model",
	})

	results, err := emb.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(results))
	}
	if results[0][0] != 0.1 {
		t.Errorf("expected results[0][0]=0.1, got %v", results[0][0])
	}
	if results[1][0] != 0.4 {
		t.Errorf("expected results[1][0]=0.4, got %v", results[1][0])
	}
}

func TestHTTPEmbedder_AuthHeader(t *testing.T) {
	srv := makeHTTPEmbedServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer sk-test" {
			t.Errorf("expected 'Bearer sk-test', got %q", auth)
		}
		resp := map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{1.0}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	emb := memstore.NewHTTPEmbedder(memstore.HTTPEmbedderConfig{
		URL:       srv.URL,
		Model:     "m",
		AuthToken: "sk-test",
	})

	_, err := emb.Embed(context.Background(), []string{"text"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPEmbedder_HTTPError(t *testing.T) {
	srv := makeHTTPEmbedServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	emb := memstore.NewHTTPEmbedder(memstore.HTTPEmbedderConfig{URL: srv.URL, Model: "m"})
	_, err := emb.Embed(context.Background(), []string{"text"})
	if err == nil {
		t.Fatal("expected error for HTTP 401")
	}
}

func TestHTTPEmbedder_EmptyResponse(t *testing.T) {
	srv := makeHTTPEmbedServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"data": []any{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	emb := memstore.NewHTTPEmbedder(memstore.HTTPEmbedderConfig{URL: srv.URL, Model: "m"})
	_, err := emb.Embed(context.Background(), []string{"text"})
	if err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestHTTPEmbedder_OutOfOrderResponse(t *testing.T) {
	// Response items returned in reverse order — should still map correctly.
	srv := makeHTTPEmbedServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": []float32{0.9}},
				{"index": 0, "embedding": []float32{0.1}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	emb := memstore.NewHTTPEmbedder(memstore.HTTPEmbedderConfig{URL: srv.URL, Model: "m"})
	results, err := emb.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results[0][0] != 0.1 {
		t.Errorf("expected results[0][0]=0.1, got %v", results[0][0])
	}
	if results[1][0] != 0.9 {
		t.Errorf("expected results[1][0]=0.9, got %v", results[1][0])
	}
}

func TestHTTPEmbedder_Model(t *testing.T) {
	emb := memstore.NewHTTPEmbedder(memstore.HTTPEmbedderConfig{URL: "http://x", Model: "my-model"})
	if emb.Model() != "my-model" {
		t.Errorf("expected model 'my-model', got %q", emb.Model())
	}
}

func TestOpenAIEmbedder_Model(t *testing.T) {
	emb := memstore.NewOpenAIEmbedder("sk-test", "text-embedding-3-small")
	if emb.Model() != "text-embedding-3-small" {
		t.Errorf("expected model 'text-embedding-3-small', got %q", emb.Model())
	}
}
