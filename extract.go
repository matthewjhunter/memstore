package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractHints provides domain context to guide fact extraction.
type ExtractHints struct {
	Persona    string   // name/role for domain context
	Focus      []string // domains to prioritize
	Categories []string // restrict to these; empty = all defaults
}

// ExtractOpts controls the extraction pipeline.
type ExtractOpts struct {
	Namespace string       // target namespace for inserted facts
	Subject   string       // default subject when LLM omits one
	Hints     ExtractHints
}

// ExtractResult summarizes the outcome of an extraction run.
type ExtractResult struct {
	Inserted   []Fact
	Duplicates int     // skipped via Exists()
	Superseded int     // stub: always 0 for now
	Errors     []error // per-fact parse/insert failures
}

// PromptFunc builds the extraction prompt from input text and hints.
type PromptFunc func(text string, hints ExtractHints) string

// FactExtractor distills unstructured text into structured facts using an LLM.
type FactExtractor struct {
	store     Store
	embedder  Embedder
	generator Generator
	promptFn  PromptFunc // nil = defaultPrompt
}

// NewFactExtractor creates an extractor that uses the given store, embedder,
// and generator to extract and persist facts from text.
func NewFactExtractor(store Store, embedder Embedder, generator Generator) *FactExtractor {
	return &FactExtractor{
		store:     store,
		embedder:  embedder,
		generator: generator,
	}
}

// SetPromptFunc overrides the default prompt builder.
func (e *FactExtractor) SetPromptFunc(fn PromptFunc) {
	e.promptFn = fn
}

// extractedFact is the intermediate representation parsed from LLM output.
type extractedFact struct {
	Content  string `json:"content"`
	Subject  string `json:"subject"`
	Category string `json:"category"`
}

// Extract distills text into structured facts and persists them.
func (e *FactExtractor) Extract(ctx context.Context, text string, opts ExtractOpts) (*ExtractResult, error) {
	promptFn := e.promptFn
	if promptFn == nil {
		promptFn = defaultPrompt
	}
	prompt := promptFn(text, opts.Hints)

	// Prefer structured JSON output when available.
	var raw string
	var err error
	if jg, ok := e.generator.(JSONGenerator); ok {
		raw, err = jg.GenerateJSON(ctx, prompt)
	} else {
		raw, err = e.generator.Generate(ctx, prompt)
	}
	if err != nil {
		return nil, fmt.Errorf("memstore: extraction generation failed: %w", err)
	}

	facts, parseErrs := parseExtractResponse(raw)
	result := &ExtractResult{
		Errors: parseErrs,
	}

	for _, ef := range facts {
		if strings.TrimSpace(ef.Content) == "" {
			continue
		}

		subject := ef.Subject
		if subject == "" {
			subject = opts.Subject
		}

		category := ef.Category
		if category == "" {
			category = "note"
		}

		// Dedup check.
		exists, err := e.store.Exists(ctx, ef.Content, subject)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("exists check for %q: %w", ef.Content, err))
			continue
		}
		if exists {
			result.Duplicates++
			continue
		}

		fact := Fact{
			Content:  ef.Content,
			Subject:  subject,
			Category: category,
		}

		// Supersession stub.
		if _, err := e.trySupersedeExisting(ctx, fact); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("supersede check for %q: %w", ef.Content, err))
		}

		// Compute embedding.
		emb, err := Single(ctx, e.embedder, ef.Content)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("embedding %q: %w", ef.Content, err))
			continue
		}
		fact.Embedding = emb

		// Persist.
		id, err := e.store.Insert(ctx, fact)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("inserting %q: %w", ef.Content, err))
			continue
		}
		fact.ID = id

		result.Inserted = append(result.Inserted, fact)
	}

	return result, nil
}

// trySupersedeExisting is a stub for future supersession logic.
// It will eventually search for same-subject facts with high similarity
// and call Supersede.
func (e *FactExtractor) trySupersedeExisting(_ context.Context, _ Fact) (*int64, error) {
	return nil, nil
}

// parseExtractResponse parses the LLM JSON output into extracted facts.
// It returns successfully parsed facts and any parse errors encountered.
func parseExtractResponse(raw string) ([]extractedFact, []error) {
	raw = strings.TrimSpace(raw)

	// Try direct array parse.
	var facts []extractedFact
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		// Try extracting a JSON array from markdown fences or surrounding text.
		if start := strings.Index(raw, "["); start >= 0 {
			if end := strings.LastIndex(raw, "]"); end > start {
				if err2 := json.Unmarshal([]byte(raw[start:end+1]), &facts); err2 == nil {
					return facts, nil
				}
			}
		}
		return nil, []error{fmt.Errorf("memstore: failed to parse extraction response: %w", err)}
	}

	return facts, nil
}

// defaultPrompt builds the extraction prompt for the LLM.
func defaultPrompt(text string, hints ExtractHints) string {
	var b strings.Builder

	b.WriteString("Extract factual claims from the following text. Return a JSON array of objects, each with these fields:\n")
	b.WriteString("- \"content\": the factual claim as a concise sentence\n")
	b.WriteString("- \"subject\": the primary entity being described\n")
	b.WriteString("- \"category\": one of: preference, identity, project, capability, world, relationship, note\n\n")

	if hints.Persona != "" {
		fmt.Fprintf(&b, "Context: you are extracting facts for the persona %q.\n", hints.Persona)
	}
	if len(hints.Focus) > 0 {
		fmt.Fprintf(&b, "Prioritize facts about: %s.\n", strings.Join(hints.Focus, ", "))
	}
	if len(hints.Categories) > 0 {
		fmt.Fprintf(&b, "Only extract facts in these categories: %s.\n", strings.Join(hints.Categories, ", "))
	}

	b.WriteString("\nReturn ONLY the JSON array, no other text.\n\nText:\n")
	b.WriteString(text)

	return b.String()
}
