package memstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OllamaGenerator implements Generator and JSONGenerator using the Ollama
// /api/chat endpoint. It mirrors the HTTP pattern used by OllamaCurator.
type OllamaGenerator struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllamaGenerator creates a generator backed by a local Ollama instance.
// baseURL is typically "http://localhost:11434"; model is e.g. "qwen2.5:7b".
func NewOllamaGenerator(baseURL, model string) *OllamaGenerator {
	return &OllamaGenerator{baseURL: baseURL, model: model, client: &http.Client{}}
}

// Generate produces a plain text completion from the given prompt.
func (g *OllamaGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return g.callChat(ctx, prompt, "")
}

// GenerateJSON produces a JSON completion by requesting structured output.
func (g *OllamaGenerator) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	return g.callChat(ctx, prompt, "json")
}

func (g *OllamaGenerator) callChat(ctx context.Context, prompt, format string) (string, error) {
	reqBody := ollamaGenRequest{
		Model: g.model,
		Messages: []ollamaGenMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}
	if format != "" {
		reqBody.Format = format
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ollama generator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("ollama generator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama generator: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ollama generator: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama generator: HTTP %d: %s", resp.StatusCode, raw)
	}

	var chatResp ollamaGenResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return "", fmt.Errorf("ollama generator: unmarshal: %w", err)
	}
	return chatResp.Message.Content, nil
}

// ollamaGenRequest is the request body for the Ollama /api/chat endpoint.
type ollamaGenRequest struct {
	Model    string             `json:"model"`
	Messages []ollamaGenMessage `json:"messages"`
	Stream   bool               `json:"stream"`
	Format   string             `json:"format,omitempty"`
}

type ollamaGenMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaGenResponse struct {
	Message ollamaGenMessage `json:"message"`
}
