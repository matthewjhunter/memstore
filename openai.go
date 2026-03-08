package memstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// newOpenAIClient builds an openai.Client pointed at baseURL with an optional API key.
// For local proxies (LiteLLM, Ollama) that don't require auth, pass an empty apiKey.
//
// The SDK default base URL is https://api.openai.com/v1/ and appends endpoint
// paths (e.g. "embeddings") directly. If baseURL does not already end with /v1
// or /v1/, we append /v1/ so that Ollama and LiteLLM base URLs work without
// requiring callers to include the path prefix.
func newOpenAIClient(baseURL, apiKey string) openai.Client {
	trimmed := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(trimmed, "/v1") {
		baseURL = trimmed + "/v1/"
	}
	opts := []option.RequestOption{option.WithBaseURL(baseURL)}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	} else {
		// The SDK requires a non-empty key; use a placeholder for keyless proxies.
		opts = append(opts, option.WithAPIKey("placeholder"))
	}
	return openai.NewClient(opts...)
}

// OpenAIEmbedder implements Embedder using an OpenAI-compatible embeddings API.
// Works with OpenAI, LiteLLM, Ollama's /v1 endpoint, or any compatible proxy.
type OpenAIEmbedder struct {
	client openai.Client
	model  string
}

// NewOpenAIEmbedder creates an embedder backed by an OpenAI-compatible API.
// baseURL is the API base (e.g. "http://litellm:4000" or "https://api.openai.com/v1").
// apiKey may be empty for local proxies that don't require auth.
func NewOpenAIEmbedder(baseURL, apiKey, model string) *OpenAIEmbedder {
	return &OpenAIEmbedder{client: newOpenAIClient(baseURL, apiKey), model: model}
}

// Embed generates vector embeddings for the given texts.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	resp, err := e.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
		Model: e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai embed: empty response")
	}
	// Response items may not be in request order; place by index.
	result := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || int(d.Index) >= len(texts) {
			return nil, fmt.Errorf("openai embed: response index %d out of range for %d inputs", d.Index, len(texts))
		}
		f32 := make([]float32, len(d.Embedding))
		for j, v := range d.Embedding {
			f32[j] = float32(v)
		}
		result[d.Index] = f32
	}
	for i, emb := range result {
		if emb == nil {
			return nil, fmt.Errorf("openai embed: missing embedding for input index %d", i)
		}
	}
	return result, nil
}

// Model returns the configured embedding model name.
func (e *OpenAIEmbedder) Model() string { return e.model }

// OpenAIGenerator implements Generator and JSONGenerator using an OpenAI-compatible chat API.
type OpenAIGenerator struct {
	client openai.Client
	model  string
}

// NewOpenAIGenerator creates a generator backed by an OpenAI-compatible API.
// baseURL is the API base (e.g. "http://litellm:4000" or "https://api.openai.com/v1").
// apiKey may be empty for local proxies that don't require auth.
func NewOpenAIGenerator(baseURL, apiKey, model string) *OpenAIGenerator {
	return &OpenAIGenerator{client: newOpenAIClient(baseURL, apiKey), model: model}
}

// Model returns the configured model name.
func (g *OpenAIGenerator) Model() string { return g.model }

// Generate produces a plain text completion from the given prompt.
func (g *OpenAIGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	resp, err := g.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: g.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai generate: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai generate: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// GenerateJSON produces a JSON completion using the json_object response format.
func (g *OpenAIGenerator) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	resp, err := g.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: g.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openai.ResponseFormatJSONObjectParam{Type: "json_object"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai generate json: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai generate json: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
