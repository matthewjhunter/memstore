package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPGenerator implements memstore.Generator and memstore.JSONGenerator by
// calling the memstored /v1/generate endpoints. This keeps Ollama access
// server-side so clients don't need direct Ollama connectivity.
type HTTPGenerator struct {
	base   string
	apiKey string
	http   *http.Client
}

// NewHTTPGenerator creates a generator that talks to memstored.
func NewHTTPGenerator(baseURL, apiKey string) *HTTPGenerator {
	return &HTTPGenerator{
		base:   baseURL,
		apiKey: apiKey,
		http:   &http.Client{Timeout: 120 * time.Second},
	}
}

type generateRequest struct {
	Prompt string `json:"prompt"`
}

type generateResponse struct {
	Text  string `json:"text"`
	Model string `json:"model"`
}

func (g *HTTPGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return g.call(ctx, "/v1/generate", prompt)
}

func (g *HTTPGenerator) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	return g.call(ctx, "/v1/generate/json", prompt)
}

// Model returns "remote" since the actual model is determined server-side.
func (g *HTTPGenerator) Model() string { return "remote" }

func (g *HTTPGenerator) call(ctx context.Context, path, prompt string) (string, error) {
	body, err := json.Marshal(generateRequest{Prompt: prompt})
	if err != nil {
		return "", fmt.Errorf("http generator: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("http generator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http generator: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("http generator: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http generator: HTTP %d: %s", resp.StatusCode, raw)
	}

	var genResp generateResponse
	if err := json.Unmarshal(raw, &genResp); err != nil {
		return "", fmt.Errorf("http generator: unmarshal: %w", err)
	}
	return genResp.Text, nil
}
