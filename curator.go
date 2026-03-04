package memstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Curator selects the most relevant subset of candidate facts for a given task.
// It is intended to sit between broad retrieval and context injection, reducing
// noise before facts are sent to the primary model.
type Curator interface {
	// Curate returns up to maxOutput facts from candidates that are most relevant
	// to the task, along with a brief rationale. If maxOutput <= 0 or >= len(candidates),
	// all candidates may be returned. On error the caller should fall back to the
	// full candidate list.
	Curate(ctx context.Context, task string, candidates []Fact, maxOutput int) (selected []Fact, rationale string, err error)
}

// NopCurator returns candidates unchanged (up to maxOutput), with no model call.
// Use this as a safe default when no curation model is configured.
type NopCurator struct{}

// Curate returns the first maxOutput candidates unmodified.
func (NopCurator) Curate(_ context.Context, _ string, candidates []Fact, maxOutput int) ([]Fact, string, error) {
	if maxOutput <= 0 || maxOutput >= len(candidates) {
		return candidates, "all candidates returned (no curation)", nil
	}
	return candidates[:maxOutput], fmt.Sprintf("top %d of %d returned (no curation model configured)", maxOutput, len(candidates)), nil
}

// OllamaCurator uses the Ollama /api/chat endpoint to curate candidates.
type OllamaCurator struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllamaCurator creates a curator backed by a local Ollama instance.
// baseURL is typically "http://localhost:11434"; model is e.g. "qwen2.5:3b".
func NewOllamaCurator(baseURL, model string) *OllamaCurator {
	return &OllamaCurator{baseURL: baseURL, model: model, client: &http.Client{}}
}

// Curate calls Ollama chat with a curation prompt and parses the JSON response.
func (c *OllamaCurator) Curate(ctx context.Context, task string, candidates []Fact, maxOutput int) ([]Fact, string, error) {
	prompt := buildCurationPrompt(task, candidates, maxOutput)
	selected, rationale, err := c.callChat(ctx, prompt)
	if err != nil {
		return nil, "", err
	}
	return pickFacts(candidates, selected), rationale, nil
}

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	Format   string              `json:"format"` // "json" for structured output
}

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message ollamaChatMessage `json:"message"`
}

func (c *OllamaCurator) callChat(ctx context.Context, prompt string) ([]int64, string, error) {
	body, _ := json.Marshal(ollamaChatRequest{
		Model: c.model,
		Messages: []ollamaChatMessage{
			{Role: "system", Content: curatorSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Stream: false,
		Format: "json",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("ollama curator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("ollama curator: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("ollama curator: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("ollama curator: HTTP %d: %s", resp.StatusCode, raw)
	}

	var chatResp ollamaChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return nil, "", fmt.Errorf("ollama curator: unmarshal chat response: %w", err)
	}
	return parseCurationResponse(chatResp.Message.Content)
}

// HTTPCurator uses any OpenAI-compatible /v1/chat/completions endpoint.
type HTTPCurator struct {
	url       string
	model     string
	authToken string
	client    *http.Client
}

// HTTPCuratorConfig holds configuration for NewHTTPCurator.
type HTTPCuratorConfig struct {
	// URL is the full chat completions endpoint (e.g. "https://api.openai.com/v1/chat/completions").
	URL string
	// Model is the chat model name (e.g. "gpt-4o-mini", "claude-haiku-4-5").
	Model string
	// AuthToken is an optional bearer token.
	AuthToken string
}

// NewHTTPCurator creates a curator backed by any OpenAI-compatible chat API.
func NewHTTPCurator(cfg HTTPCuratorConfig) *HTTPCurator {
	return &HTTPCurator{url: cfg.URL, model: cfg.Model, authToken: cfg.AuthToken, client: &http.Client{}}
}

// Curate calls the OpenAI-compatible chat endpoint with a curation prompt.
func (c *HTTPCurator) Curate(ctx context.Context, task string, candidates []Fact, maxOutput int) ([]Fact, string, error) {
	prompt := buildCurationPrompt(task, candidates, maxOutput)
	selected, rationale, err := c.callChat(ctx, prompt)
	if err != nil {
		return nil, "", err
	}
	return pickFacts(candidates, selected), rationale, nil
}

type httpChatRequest struct {
	Model          string            `json:"model"`
	Messages       []httpChatMessage `json:"messages"`
	ResponseFormat *responseFormat   `json:"response_format,omitempty"`
}

type httpChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"` // "json_object"
}

type httpChatResponse struct {
	Choices []struct {
		Message httpChatMessage `json:"message"`
	} `json:"choices"`
}

func (c *HTTPCurator) callChat(ctx context.Context, prompt string) ([]int64, string, error) {
	body, _ := json.Marshal(httpChatRequest{
		Model: c.model,
		Messages: []httpChatMessage{
			{Role: "system", Content: curatorSystemPrompt},
			{Role: "user", Content: prompt},
		},
		ResponseFormat: &responseFormat{Type: "json_object"},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("http curator: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("http curator: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("http curator: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http curator: HTTP %d: %s", resp.StatusCode, raw)
	}

	var chatResp httpChatResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return nil, "", fmt.Errorf("http curator: unmarshal chat response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, "", fmt.Errorf("http curator: empty choices in response")
	}
	return parseCurationResponse(chatResp.Choices[0].Message.Content)
}

// --- shared helpers ---

const curatorSystemPrompt = `You are a context curator for a software engineering assistant.
Your job is to select the most essential facts from a candidate list for a given task.
Return ONLY a JSON object with two fields:
  "selected_ids": an array of integer fact IDs you selected (most important first)
  "rationale": one sentence explaining what you selected and why`

// buildCurationPrompt formats the task and candidates into a prompt.
func buildCurationPrompt(task string, candidates []Fact, maxOutput int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\n\n", task)
	fmt.Fprintf(&b, "Candidate facts (select at most %d):\n\n", maxOutput)
	for _, f := range candidates {
		fmt.Fprintf(&b, "[id=%d] subject=%s category=%s", f.ID, f.Subject, f.Category)
		if f.Kind != "" {
			fmt.Fprintf(&b, " kind=%s", f.Kind)
		}
		if f.Subsystem != "" {
			fmt.Fprintf(&b, " subsystem=%s", f.Subsystem)
		}
		fmt.Fprintf(&b, "\n  %s\n\n", f.Content)
	}
	fmt.Fprintf(&b, `Return JSON: {"selected_ids": [<id>, ...], "rationale": "<one sentence>"}`)
	return b.String()
}

type curationResponseJSON struct {
	SelectedIDs []int64 `json:"selected_ids"`
	Rationale   string  `json:"rationale"`
}

// parseCurationResponse unmarshals the model's JSON output.
func parseCurationResponse(content string) ([]int64, string, error) {
	// Strip markdown code fences if present.
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		if idx := strings.Index(content, "\n"); idx != -1 {
			content = content[idx+1:]
		}
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
	}
	var out curationResponseJSON
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, "", fmt.Errorf("curator: parse response JSON: %w (raw: %s)", err, content)
	}
	return out.SelectedIDs, out.Rationale, nil
}

// pickFacts returns the candidates whose IDs appear in selected, preserving
// the order of selected. Unknown IDs are silently ignored.
func pickFacts(candidates []Fact, selected []int64) []Fact {
	byID := make(map[int64]Fact, len(candidates))
	for _, f := range candidates {
		byID[f.ID] = f
	}
	out := make([]Fact, 0, len(selected))
	for _, id := range selected {
		if f, ok := byID[id]; ok {
			out = append(out, f)
		}
	}
	return out
}
