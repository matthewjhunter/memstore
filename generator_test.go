package memstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaGenerator_Generate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req ollamaGenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Format != "" {
			t.Errorf("expected empty format for Generate, got %q", req.Format)
		}
		if req.Stream {
			t.Error("expected stream=false")
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		json.NewEncoder(w).Encode(ollamaGenResponse{
			Message: ollamaGenMessage{Role: "assistant", Content: "hello world"},
		})
	}))
	defer srv.Close()

	gen := NewOllamaGenerator(srv.URL, "test-model")
	result, err := gen.Generate(context.Background(), "say hello")
	if err != nil {
		t.Fatal(err)
	}
	if result != "hello world" {
		t.Errorf("got %q, want %q", result, "hello world")
	}
}

func TestOllamaGenerator_GenerateJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaGenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Format != "json" {
			t.Errorf("expected format=json, got %q", req.Format)
		}
		json.NewEncoder(w).Encode(ollamaGenResponse{
			Message: ollamaGenMessage{Role: "assistant", Content: `{"key": "value"}`},
		})
	}))
	defer srv.Close()

	gen := NewOllamaGenerator(srv.URL, "test-model")
	result, err := gen.GenerateJSON(context.Background(), "return json")
	if err != nil {
		t.Fatal(err)
	}
	if result != `{"key": "value"}` {
		t.Errorf("got %q", result)
	}
}

func TestOllamaGenerator_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	gen := NewOllamaGenerator(srv.URL, "missing-model")
	_, err := gen.Generate(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestOllamaGenerator_ImplementsJSONGenerator(t *testing.T) {
	var _ JSONGenerator = (*OllamaGenerator)(nil)
}
