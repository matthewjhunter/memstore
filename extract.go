package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matthewjhunter/airlock/unwrap"
	"github.com/matthewjhunter/go-embedding"
)

// ExtractHints provides domain context to guide fact extraction.
type ExtractHints struct {
	Persona    string   // name/role for domain context
	Focus      []string // domains to prioritize
	Categories []string // restrict to these; empty = all defaults
}

// ExtractOpts controls the extraction pipeline.
type ExtractOpts struct {
	Namespace string // target namespace for inserted facts
	Subject   string // default subject when LLM omits one
	Hints     ExtractHints
	Metadata  json.RawMessage // attached to every inserted fact (e.g. {"cwd":..., "source":...})
}

// ExtractResult summarizes the outcome of an extraction run.
type ExtractResult struct {
	Inserted   []Fact
	Duplicates int     // skipped via Exists() or in-batch dedup
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
	embedder  embedding.Embedder
	generator Generator
	promptFn  PromptFunc // nil = defaultPrompt
}

// NewFactExtractor creates an extractor that uses the given store, embedder,
// and generator to extract and persist facts from text.
func NewFactExtractor(store Store, embedder embedding.Embedder, generator Generator) *FactExtractor {
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
// no embeddings or IDs -- the caller handles those.
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

// candidate is a fact that has passed the dedup check and is ready for embedding and insertion.
type candidate struct {
	Fact
}

// Extract distills text into structured facts and persists them.
// It runs four phases:
//
//  1. Resolve and dedup -- skip empty content, resolve subject/category defaults,
//     check existence, and drop in-batch duplicates.
//  2. Batch embed -- one EmbedWithRetry call for all surviving candidates.
//  3. Insert -- persist each candidate.
//  4. Supersession -- one SearchBatch call per distinct subject to find and
//     supersede near-duplicate existing facts.
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

	// --- Phase A: resolve and dedup ---
	seen := make(map[string]bool)
	var candidates []candidate

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

		// In-batch dedup: same content+subject pair seen earlier in this batch.
		dedupKey := ef.Content + "\x00" + subject
		if seen[dedupKey] {
			result.Duplicates++
			continue
		}
		seen[dedupKey] = true

		// Existence check against the store.
		exists, err := e.store.Exists(ctx, ef.Content, subject)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("exists check for %q: %w", ef.Content, err))
			continue
		}
		if exists {
			result.Duplicates++
			continue
		}

		candidates = append(candidates, candidate{Fact: Fact{
			Content:  ef.Content,
			Subject:  subject,
			Category: category,
			Metadata: opts.Metadata,
		}})
	}

	if len(candidates) == 0 {
		return result, nil
	}

	// --- Phase B: batch embed ---
	if e.embedder != nil {
		contents := make([]string, len(candidates))
		for i, c := range candidates {
			contents[i] = c.Content
		}
		embs, err := embedding.EmbedWithRetry(ctx, e.embedder, contents)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("memstore: batch embedding %d facts: %w", len(contents), err))
			return result, nil
		}
		if len(embs) != len(contents) {
			result.Errors = append(result.Errors, fmt.Errorf("memstore: batch embedding %d facts: got %d embeddings, want %d", len(contents), len(embs), len(contents)))
			return result, nil
		}
		for i := range candidates {
			candidates[i].Embedding = embs[i]
		}
	}

	// --- Phase C: insert ---
	// batchIndexByID maps each inserted fact's ID to its position in the candidates
	// slice. Used in Phase D to prevent later batch-mates from superseding earlier ones.
	batchIndexByID := make(map[int64]int)

	for i, c := range candidates {
		id, err := e.store.Insert(ctx, c.Fact)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("inserting %q: %w", c.Content, err))
			continue
		}
		candidates[i].ID = id
		batchIndexByID[id] = i
		result.Inserted = append(result.Inserted, candidates[i].Fact)
	}

	// --- Phase D: supersession ---
	// Skip entirely when no embedder is configured (no embeddings to compare).
	if e.embedder == nil || len(result.Inserted) == 0 {
		return result, nil
	}

	// Group inserted facts by subject for one SearchBatch call per distinct subject.
	type insertedFact struct {
		fact       Fact
		batchIndex int
	}
	bySubject := make(map[string][]insertedFact)
	for _, f := range result.Inserted {
		if len(f.Embedding) == 0 {
			continue
		}
		idx := batchIndexByID[f.ID]
		bySubject[f.Subject] = append(bySubject[f.Subject], insertedFact{fact: f, batchIndex: idx})
	}

	// supersededInRun tracks IDs already superseded in this batch to prevent chains.
	supersededInRun := make(map[int64]bool)

	for subj, group := range bySubject {
		contents := make([]string, len(group))
		for i, g := range group {
			contents[i] = g.fact.Content
		}

		neighborSets, err := e.store.SearchBatch(ctx, contents, SearchOpts{
			MaxResults: 10,
			Subject:    subj,
			OnlyActive: true,
		})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("supersede search for subject %q: %w", subj, err))
			continue
		}

		for i, g := range group {
			if i >= len(neighborSets) {
				break
			}
			neighbors := neighborSets[i]
			supersededID := e.pickSupersessionTarget(g.fact, g.batchIndex, neighbors, batchIndexByID, supersededInRun)
			if supersededID == nil {
				continue
			}
			if err := e.store.Supersede(ctx, *supersededID, g.fact.ID); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("supersede check for %q: %w", g.fact.Content, err))
				continue
			}
			supersededInRun[*supersededID] = true
			result.Superseded++
		}
	}

	return result, nil
}

// pickSupersessionTarget examines pre-fetched search results for newFact and
// returns the ID of the best candidate to supersede, or nil if none qualifies.
//
// Filters applied (in order):
//   - Skip self
//   - Skip facts with no embedding
//   - Skip MetadataConflicts
//   - Skip batch-mates that have a later batch index (prevents A->B->A cycles)
//   - Skip facts already superseded in this run
//
// Among the remaining candidates, the one with the highest cosine similarity
// above similarityThreshold is chosen.
func (e *FactExtractor) pickSupersessionTarget(
	newFact Fact,
	newBatchIndex int,
	neighbors []SearchResult,
	batchIndexByID map[int64]int,
	supersededInRun map[int64]bool,
) *int64 {
	var bestID int64
	var bestSim float64

	for _, r := range neighbors {
		if r.Fact.ID == newFact.ID {
			continue // skip self
		}
		if len(r.Fact.Embedding) == 0 {
			continue
		}
		if MetadataConflicts(newFact.Metadata, r.Fact.Metadata) {
			continue // different contexts -- don't auto-supersede
		}
		// If the candidate is a batch-mate with a later index, skip it to
		// preserve sequential semantics and prevent A->B->A cycles.
		if laterIdx, isBatchMate := batchIndexByID[r.Fact.ID]; isBatchMate && laterIdx > newBatchIndex {
			continue
		}
		// Skip facts already superseded in this run.
		if supersededInRun[r.Fact.ID] {
			continue
		}
		sim := embedding.CosineSimilarity(newFact.Embedding, r.Fact.Embedding)
		if sim > bestSim {
			bestSim = sim
			bestID = r.Fact.ID
		}
	}

	if bestSim < similarityThreshold || bestID == 0 {
		return nil
	}
	return &bestID
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
		return false // can't parse -- don't block
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
// Handles: bare arrays, single objects, wrapper objects with a top-level
// array field (e.g. {"facts": [...]}), and markdown-fenced arrays.
func parseExtractResponse(raw string) ([]extractedFact, []error) {
	raw = strings.TrimSpace(raw)

	// Recover the first balanced JSON value, tolerating markdown fences and
	// surrounding prose via airlock/unwrap's string-aware scanner.
	candidate, err := unwrap.JSON(raw)
	if err != nil {
		return nil, []error{fmt.Errorf("memstore: failed to parse extraction response: %q", truncate(raw, 200))}
	}

	// Try array parse (the common case: a bare JSON array of facts).
	var facts []extractedFact
	if err := json.Unmarshal(candidate, &facts); err == nil {
		return facts, nil
	}

	// Try single object (model returned one fact instead of an array).
	var single extractedFact
	if err := json.Unmarshal(candidate, &single); err == nil && single.Content != "" {
		return []extractedFact{single}, nil
	}

	// Try wrapper object with a top-level array field (e.g. {"facts": [...]}).
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(candidate, &wrapper); err == nil {
		for _, v := range wrapper {
			if err2 := json.Unmarshal(v, &facts); err2 == nil && len(facts) > 0 {
				return facts, nil
			}
		}
	}

	return nil, []error{fmt.Errorf("memstore: failed to parse extraction response: %q", truncate(raw, 200))}
}

// truncate returns s trimmed to maxLen bytes, with "..." appended if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
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
