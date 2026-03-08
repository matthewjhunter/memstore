package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go"
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

// OpenAICurator uses an OpenAI-compatible chat API to curate candidate facts.
// Works with OpenAI, LiteLLM, or any compatible proxy.
type OpenAICurator struct {
	client openai.Client
	model  string
}

// NewOpenAICurator creates a curator backed by an OpenAI-compatible chat API.
// baseURL is the API base (e.g. "http://litellm:4000" or "https://api.openai.com/v1").
// apiKey may be empty for local proxies that don't require auth.
func NewOpenAICurator(baseURL, apiKey, model string) *OpenAICurator {
	return &OpenAICurator{client: newOpenAIClient(baseURL, apiKey), model: model}
}

// Curate calls the chat API with a curation prompt and parses the JSON response.
func (c *OpenAICurator) Curate(ctx context.Context, task string, candidates []Fact, maxOutput int) ([]Fact, string, error) {
	prompt := buildCurationPrompt(task, candidates, maxOutput)
	resp, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(curatorSystemPrompt),
			openai.UserMessage(prompt),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openai.ResponseFormatJSONObjectParam{Type: "json_object"},
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("openai curator: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, "", fmt.Errorf("openai curator: empty choices in response")
	}
	selected, rationale, err := parseCurationResponse(resp.Choices[0].Message.Content)
	if err != nil {
		return nil, "", err
	}
	return pickFacts(candidates, selected), rationale, nil
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
