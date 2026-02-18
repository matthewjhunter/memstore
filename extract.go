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
	Superseded int     // count of old facts auto-superseded by new ones
	Errors     []error // per-fact parse/insert failures
}

// similarityThreshold is the minimum cosine similarity between a new fact's
// embedding and an existing same-subject fact's embedding to trigger automatic
// supersession. Conservative to avoid false positives.
const similarityThreshold = 0.85

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

// ExtractFacts uses a generator to extract structured facts from text
// without persisting them. Returns parsed facts with subject and category
// populated, ready for caller-managed insertion. The returned facts have
// no embeddings or IDs — the caller handles those.
func ExtractFacts(ctx context.Context, gen Generator, text string, opts ExtractOpts) ([]Fact, error) {
	prompt := defaultPrompt(text, opts.Hints)

	var raw string
	var err error
	if jg, ok := gen.(JSONGenerator); ok {
		raw, err = jg.GenerateJSON(ctx, prompt)
	} else {
		raw, err = gen.Generate(ctx, prompt)
	}
	if err != nil {
		return nil, fmt.Errorf("memstore: extraction generation failed: %w", err)
	}

	parsed, parseErrs := parseExtractResponse(raw)
	if len(parseErrs) > 0 && len(parsed) == 0 {
		return nil, parseErrs[0]
	}

	var facts []Fact
	for _, ef := range parsed {
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
		facts = append(facts, Fact{
			Content:  ef.Content,
			Subject:  subject,
			Category: category,
		})
	}
	return facts, nil
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

		// Compute embedding (if embedder is available).
		if e.embedder != nil {
			emb, err := Single(ctx, e.embedder, ef.Content)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("embedding %q: %w", ef.Content, err))
				continue
			}
			fact.Embedding = emb
		}

		// Persist.
		id, err := e.store.Insert(ctx, fact)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("inserting %q: %w", ef.Content, err))
			continue
		}
		fact.ID = id

		// Auto-supersession: after insert so we have the new fact's ID.
		if supersededID, err := e.trySupersedeExisting(ctx, fact); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("supersede check for %q: %w", ef.Content, err))
		} else if supersededID != nil {
			result.Superseded++
		}

		result.Inserted = append(result.Inserted, fact)
	}

	return result, nil
}

// trySupersedeExisting searches for same-subject active facts with high
// embedding similarity and supersedes the best match. Returns the superseded
// fact's ID if one was found, or nil if no match exceeded the threshold.
//
// Metadata acts as a context discriminator: if both facts have metadata and
// any shared keys have different values, supersession is skipped. This prevents
// facts from different contexts (e.g., different projects or sources) from
// incorrectly superseding each other.
func (e *FactExtractor) trySupersedeExisting(ctx context.Context, newFact Fact) (*int64, error) {
	if e.embedder == nil || len(newFact.Embedding) == 0 || newFact.ID == 0 {
		return nil, nil
	}

	// Search for same-subject active facts.
	results, err := e.store.Search(ctx, newFact.Content, SearchOpts{
		MaxResults: 10,
		Subject:    newFact.Subject,
		OnlyActive: true,
	})
	if err != nil {
		return nil, err
	}

	var bestID int64
	var bestSim float64
	for _, r := range results {
		if r.Fact.ID == newFact.ID {
			continue // skip self
		}
		if len(r.Fact.Embedding) == 0 {
			continue
		}
		if MetadataConflicts(newFact.Metadata, r.Fact.Metadata) {
			continue // different contexts — don't auto-supersede
		}
		sim := CosineSimilarity(newFact.Embedding, r.Fact.Embedding)
		if sim > bestSim {
			bestSim = sim
			bestID = r.Fact.ID
		}
	}

	if bestSim < similarityThreshold || bestID == 0 {
		return nil, nil
	}

	if err := e.store.Supersede(ctx, bestID, newFact.ID); err != nil {
		return nil, err
	}
	return &bestID, nil
}

// MetadataConflicts returns true if both metadata values are non-empty JSON
// objects and any shared top-level keys have different values. This prevents
// auto-supersession across different contexts (different projects, sources, etc.).
// Returns false if either is nil/empty or if they have no conflicting keys.
func MetadataConflicts(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var ma, mb map[string]any
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return false // can't parse — don't block
	}
	for k, va := range ma {
		if vb, ok := mb[k]; ok {
			if fmt.Sprintf("%v", va) != fmt.Sprintf("%v", vb) {
				return true
			}
		}
	}
	return false
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
