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
// paths (e.g. "chat/completions") directly. If baseURL does not already end with
// /v1 or /v1/, we append /v1/ so that Ollama and LiteLLM base URLs work without
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

// GenerateJSONSchema produces a JSON completion constrained by the given
// schema. Uses OpenAI's structured outputs (json_schema) with strict=true.
// Providers and proxies vary in how strictly they enforce the schema; even
// when only treated as a hint, enumerated fields and required-property lists
// reduce format lapses materially.
func (g *OpenAIGenerator) GenerateJSONSchema(ctx context.Context, prompt, name string, schema any) (string, error) {
	resp, err := g.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: g.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &openai.ResponseFormatJSONSchemaParam{
				JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   name,
					Schema: schema,
					Strict: openai.Bool(true),
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai generate json_schema: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai generate json_schema: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
