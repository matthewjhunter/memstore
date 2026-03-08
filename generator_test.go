package memstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// openAIChatResponse builds a minimal OpenAI chat completion response.
func openAIChatResponse(content string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"model":   "test-model",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
}

func TestOpenAIGenerator_Generate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openAIChatResponse("hello world"))
	}))
	defer srv.Close()

	gen := NewOpenAIGenerator(srv.URL, "", "test-model")
	result, err := gen.Generate(context.Background(), "say hello")
	if err != nil {
		t.Fatal(err)
	}
	if result != "hello world" {
		t.Errorf("got %q, want %q", result, "hello world")
	}
}

func TestOpenAIGenerator_GenerateJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ResponseFormat map[string]string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.ResponseFormat["type"] != "json_object" {
			t.Errorf("expected response_format.type=json_object, got %q", req.ResponseFormat["type"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openAIChatResponse(`{"key": "value"}`))
	}))
	defer srv.Close()

	gen := NewOpenAIGenerator(srv.URL, "", "test-model")
	result, err := gen.GenerateJSON(context.Background(), "return json")
	if err != nil {
		t.Fatal(err)
	}
	if result != `{"key": "value"}` {
		t.Errorf("got %q", result)
	}
}

func TestOpenAIGenerator_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	gen := NewOpenAIGenerator(srv.URL, "", "missing-model")
	_, err := gen.Generate(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

func TestOpenAIGenerator_ImplementsJSONGenerator(t *testing.T) {
	var _ JSONGenerator = (*OpenAIGenerator)(nil)
}
