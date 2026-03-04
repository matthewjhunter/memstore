package memstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HTTPEmbedder implements Embedder using any OpenAI-compatible embeddings API.
// This covers OpenAI, vLLM, llama.cpp server, LiteLLM proxy, Azure OpenAI,
// and any other service that speaks the POST /v1/embeddings protocol.
type HTTPEmbedder struct {
	url       string
	model     string
	authToken string // optional; sent as "Authorization: Bearer <token>"
	client    *http.Client
}

// HTTPEmbedderConfig holds configuration for NewHTTPEmbedder.
type HTTPEmbedderConfig struct {
	// URL is the full embeddings endpoint (e.g. "https://api.openai.com/v1/embeddings").
	URL string
	// Model is the embedding model name (e.g. "text-embedding-3-small").
	// Returned by Model() and sent in the request body.
	Model string
	// AuthToken is an optional bearer token. When non-empty, the request
	// includes "Authorization: Bearer <AuthToken>".
	AuthToken string
}

// NewHTTPEmbedder creates an embedder that calls any OpenAI-compatible
// embeddings endpoint. The response must follow the standard format:
//
//	{"data": [{"embedding": [...], "index": 0}, ...]}
func NewHTTPEmbedder(cfg HTTPEmbedderConfig) *HTTPEmbedder {
	return &HTTPEmbedder{
		url:       cfg.URL,
		model:     cfg.Model,
		authToken: cfg.AuthToken,
		client:    &http.Client{},
	}
}

type httpEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type httpEmbedResponseItem struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type httpEmbedResponse struct {
	Data []httpEmbedResponseItem `json:"data"`
}

// Embed generates vector embeddings for the given texts.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := httpEmbedRequest{
		Model: e.model,
		Input: texts,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("http embed: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("http embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.authToken)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http embed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http embed: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http embed: HTTP %d: %s", resp.StatusCode, body)
	}

	var embedResp httpEmbedResponse
	if err := json.Unmarshal(body, &embedResp); err != nil {
		return nil, fmt.Errorf("http embed: unmarshal: %w", err)
	}

	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("http embed: empty response")
	}

	// Response items may not be in request order; sort by index.
	result := make([][]float32, len(texts))
	for _, item := range embedResp.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("http embed: response index %d out of range for %d inputs", item.Index, len(texts))
		}
		result[item.Index] = item.Embedding
	}

	// Verify every slot was filled.
	for i, emb := range result {
		if emb == nil {
			return nil, fmt.Errorf("http embed: missing embedding for input index %d", i)
		}
	}

	return result, nil
}

// Model returns the configured embedding model name.
func (e *HTTPEmbedder) Model() string { return e.model }

// OpenAIEmbedder is a convenience wrapper around HTTPEmbedder preset for
// the official OpenAI embeddings API (api.openai.com/v1/embeddings).
type OpenAIEmbedder struct {
	*HTTPEmbedder
}

// NewOpenAIEmbedder creates an embedder that calls the OpenAI embeddings API.
// apiKey is your OpenAI API key; model is the embedding model name
// (e.g. "text-embedding-3-small" or "text-embedding-3-large").
func NewOpenAIEmbedder(apiKey, model string) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		HTTPEmbedder: NewHTTPEmbedder(HTTPEmbedderConfig{
			URL:       "https://api.openai.com/v1/embeddings",
			Model:     model,
			AuthToken: apiKey,
		}),
	}
}
