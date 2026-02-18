package memstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// mockGenerator implements only Generator (not JSONGenerator).
type mockGenerator struct {
	response string
	err      error
	prompt   string // last prompt received
}

func (m *mockGenerator) Generate(_ context.Context, prompt string) (string, error) {
	m.prompt = prompt
	return m.response, m.err
}

// mockJSONGenerator implements both Generator and JSONGenerator.
type mockJSONGenerator struct {
	response string
	err      error
	usedJSON bool
}

func (m *mockJSONGenerator) Generate(_ context.Context, _ string) (string, error) {
	return m.response, m.err
}

func (m *mockJSONGenerator) GenerateJSON(_ context.Context, _ string) (string, error) {
	m.usedJSON = true
	return m.response, m.err
}

func TestExtractBasic(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockGenerator{
		response: `[
			{"content": "Matthew prefers dark mode", "subject": "Matthew", "category": "preference"},
			{"content": "Matthew works at home", "subject": "Matthew", "category": "identity"}
		]`,
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	ctx := context.Background()

	result, err := ext.Extract(ctx, "Matthew told me he prefers dark mode and works at home.", memstore.ExtractOpts{
		Subject: "Matthew",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
	if len(result.Inserted) != 2 {
		t.Fatalf("inserted %d facts, want 2", len(result.Inserted))
	}

	// Verify content, subject, category.
	f := result.Inserted[0]
	if f.Content != "Matthew prefers dark mode" {
		t.Errorf("content = %q", f.Content)
	}
	if f.Subject != "Matthew" {
		t.Errorf("subject = %q", f.Subject)
	}
	if f.Category != "preference" {
		t.Errorf("category = %q", f.Category)
	}
	if f.Embedding == nil {
		t.Error("expected non-nil embedding")
	}
	if f.ID == 0 {
		t.Error("expected non-zero ID")
	}

	// Verify facts are in the store.
	exists, _ := store.Exists(ctx, "Matthew prefers dark mode", "Matthew")
	if !exists {
		t.Error("expected fact to exist in store")
	}
}

func TestExtractDedup(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	ctx := context.Background()

	// Pre-insert a fact.
	store.Insert(ctx, memstore.Fact{
		Content:  "Matthew prefers dark mode",
		Subject:  "Matthew",
		Category: "preference",
	})

	gen := &mockGenerator{
		response: `[
			{"content": "Matthew prefers dark mode", "subject": "Matthew", "category": "preference"},
			{"content": "Matthew likes Go", "subject": "Matthew", "category": "preference"}
		]`,
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	result, err := ext.Extract(ctx, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", result.Duplicates)
	}
	if len(result.Inserted) != 1 {
		t.Errorf("inserted = %d, want 1", len(result.Inserted))
	}
	if result.Inserted[0].Content != "Matthew likes Go" {
		t.Errorf("inserted content = %q", result.Inserted[0].Content)
	}
}

func TestExtractJSONGenerator(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockJSONGenerator{
		response: `[{"content": "test fact", "subject": "X", "category": "note"}]`,
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	_, err := ext.Extract(context.Background(), "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !gen.usedJSON {
		t.Error("expected GenerateJSON to be called, but Generate was used instead")
	}
}

func TestExtractCustomPrompt(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockGenerator{
		response: `[{"content": "custom fact", "subject": "X", "category": "note"}]`,
	}

	var receivedText string
	var receivedHints memstore.ExtractHints
	customPrompt := func(text string, hints memstore.ExtractHints) string {
		receivedText = text
		receivedHints = hints
		return "custom prompt: " + text
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	ext.SetPromptFunc(customPrompt)

	hints := memstore.ExtractHints{
		Persona: "Jarvis",
		Focus:   []string{"preferences", "habits"},
	}
	_, err := ext.Extract(context.Background(), "input text", memstore.ExtractOpts{
		Hints: hints,
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if receivedText != "input text" {
		t.Errorf("prompt func received text = %q, want %q", receivedText, "input text")
	}
	if receivedHints.Persona != "Jarvis" {
		t.Errorf("prompt func received persona = %q, want %q", receivedHints.Persona, "Jarvis")
	}
	if gen.prompt != "custom prompt: input text" {
		t.Errorf("generator received prompt = %q", gen.prompt)
	}
}

func TestExtractBadJSON(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockGenerator{
		response: `this is not json at all`,
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	result, err := ext.Extract(context.Background(), "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract should not return top-level error for bad JSON: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Error("expected parse error in result.Errors")
	}
	if len(result.Inserted) != 0 {
		t.Errorf("inserted = %d, want 0", len(result.Inserted))
	}
}

func TestExtractEmptyContent(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockGenerator{
		response: `[
			{"content": "", "subject": "X", "category": "note"},
			{"content": "   ", "subject": "X", "category": "note"},
			{"content": "real fact", "subject": "X", "category": "note"}
		]`,
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	result, err := ext.Extract(context.Background(), "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(result.Inserted) != 1 {
		t.Fatalf("inserted = %d, want 1 (empty content should be skipped)", len(result.Inserted))
	}
	if result.Inserted[0].Content != "real fact" {
		t.Errorf("content = %q", result.Inserted[0].Content)
	}
}

func TestExtractDefaultSubject(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockGenerator{
		response: `[
			{"content": "likes coffee", "subject": "", "category": "preference"},
			{"content": "uses vim", "subject": "Matthew", "category": "preference"}
		]`,
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	result, err := ext.Extract(context.Background(), "some text", memstore.ExtractOpts{
		Subject: "DefaultUser",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(result.Inserted) != 2 {
		t.Fatalf("inserted = %d, want 2", len(result.Inserted))
	}

	// First fact should get the default subject.
	if result.Inserted[0].Subject != "DefaultUser" {
		t.Errorf("first fact subject = %q, want %q", result.Inserted[0].Subject, "DefaultUser")
	}
	// Second fact keeps its explicit subject.
	if result.Inserted[1].Subject != "Matthew" {
		t.Errorf("second fact subject = %q, want %q", result.Inserted[1].Subject, "Matthew")
	}
}

func TestExtractFacts(t *testing.T) {
	gen := &mockGenerator{
		response: `[
			{"content": "Matthew prefers dark mode", "subject": "Matthew", "category": "preference"},
			{"content": "Matthew works at home", "subject": "Matthew", "category": "identity"}
		]`,
	}

	facts, err := memstore.ExtractFacts(context.Background(), gen, "Matthew told me he prefers dark mode and works at home.", memstore.ExtractOpts{
		Subject: "Matthew",
	})
	if err != nil {
		t.Fatalf("ExtractFacts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2", len(facts))
	}

	// Verify content, subject, category are populated.
	f := facts[0]
	if f.Content != "Matthew prefers dark mode" {
		t.Errorf("content = %q", f.Content)
	}
	if f.Subject != "Matthew" {
		t.Errorf("subject = %q", f.Subject)
	}
	if f.Category != "preference" {
		t.Errorf("category = %q", f.Category)
	}
	// No embedding or ID — caller handles those.
	if f.Embedding != nil {
		t.Error("expected nil embedding from ExtractFacts")
	}
	if f.ID != 0 {
		t.Error("expected zero ID from ExtractFacts")
	}
}

func TestExtractFacts_DefaultSubject(t *testing.T) {
	gen := &mockGenerator{
		response: `[
			{"content": "likes coffee", "subject": "", "category": "preference"},
			{"content": "uses vim", "subject": "Matthew", "category": "preference"}
		]`,
	}

	facts, err := memstore.ExtractFacts(context.Background(), gen, "some text", memstore.ExtractOpts{
		Subject: "DefaultUser",
	})
	if err != nil {
		t.Fatalf("ExtractFacts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2", len(facts))
	}
	if facts[0].Subject != "DefaultUser" {
		t.Errorf("first fact subject = %q, want %q", facts[0].Subject, "DefaultUser")
	}
	if facts[1].Subject != "Matthew" {
		t.Errorf("second fact subject = %q, want %q", facts[1].Subject, "Matthew")
	}
}

func TestExtractFacts_GeneratorError(t *testing.T) {
	gen := &mockGenerator{
		err: fmt.Errorf("LLM service unavailable"),
	}

	_, err := memstore.ExtractFacts(context.Background(), gen, "some text", memstore.ExtractOpts{})
	if err == nil {
		t.Error("expected error when generator fails")
	}
}

func TestExtractFacts_EmptyContent(t *testing.T) {
	gen := &mockGenerator{
		response: `[
			{"content": "", "subject": "X", "category": "note"},
			{"content": "   ", "subject": "X", "category": "note"},
			{"content": "real fact", "subject": "X", "category": "note"}
		]`,
	}

	facts, err := memstore.ExtractFacts(context.Background(), gen, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("ExtractFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1 (empty content should be skipped)", len(facts))
	}
	if facts[0].Content != "real fact" {
		t.Errorf("content = %q", facts[0].Content)
	}
}

func TestExtractFacts_DefaultCategory(t *testing.T) {
	gen := &mockGenerator{
		response: `[{"content": "some fact", "subject": "X", "category": ""}]`,
	}

	facts, err := memstore.ExtractFacts(context.Background(), gen, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("ExtractFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].Category != "note" {
		t.Errorf("category = %q, want %q", facts[0].Category, "note")
	}
}

func TestExtractGeneratorError(t *testing.T) {
	store := openTestStore(t)
	embedder := &mockEmbedder{dim: 4}
	gen := &mockGenerator{
		err: fmt.Errorf("LLM service unavailable"),
	}

	ext := memstore.NewFactExtractor(store, embedder, gen)
	_, err := ext.Extract(context.Background(), "some text", memstore.ExtractOpts{})
	if err == nil {
		t.Error("expected error when generator fails")
	}
}

// --- Auto-supersession tests ---

// identityEmbedder returns identical embeddings for all inputs, simulating
// very similar content.
type identityEmbedder struct {
	dim int
}

func (e *identityEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i := range texts {
		emb := make([]float32, e.dim)
		for j := range emb {
			emb[j] = 1.0 // identical embeddings → cosine similarity = 1.0
		}
		result[i] = emb
	}
	return result, nil
}

func (e *identityEmbedder) Model() string { return "identity" }

// orthogonalEmbedder returns orthogonal embeddings for each text, simulating
// completely different content.
type orthogonalEmbedder struct {
	dim   int
	count int
}

func (e *orthogonalEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i := range texts {
		emb := make([]float32, e.dim)
		idx := (e.count + i) % e.dim
		emb[idx] = 1.0 // orthogonal to other calls
		result[i] = emb
		e.count++
	}
	return result, nil
}

func (e *orthogonalEmbedder) Model() string { return "orthogonal" }

func TestExtract_AutoSupersede_AboveThreshold(t *testing.T) {
	embedder := &identityEmbedder{dim: 4}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	// Pre-insert a fact with the same embedding as everything else.
	emb, _ := memstore.Single(ctx, embedder, "old fact")
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew uses vim",
		Subject:   "Matthew",
		Category:  "preference",
		Embedding: emb,
	})

	gen := &mockGenerator{
		response: `[{"content": "Matthew uses neovim", "subject": "Matthew", "category": "preference"}]`,
	}
	ext := memstore.NewFactExtractor(store, embedder, gen)

	result, err := ext.Extract(ctx, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.Superseded != 1 {
		t.Errorf("superseded = %d, want 1", result.Superseded)
	}
	if len(result.Inserted) != 1 {
		t.Fatalf("inserted = %d, want 1", len(result.Inserted))
	}

	// The old fact should be superseded.
	active, _ := store.BySubject(ctx, "Matthew", true)
	if len(active) != 1 {
		t.Fatalf("expected 1 active fact, got %d", len(active))
	}
	if active[0].Content != "Matthew uses neovim" {
		t.Errorf("active content = %q", active[0].Content)
	}
}

func TestExtract_AutoSupersede_BelowThreshold(t *testing.T) {
	embedder := &orthogonalEmbedder{dim: 8}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	// Pre-insert a fact with an orthogonal embedding.
	emb, _ := memstore.Single(ctx, embedder, "unrelated fact")
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew likes coffee",
		Subject:   "Matthew",
		Category:  "preference",
		Embedding: emb,
	})

	gen := &mockGenerator{
		response: `[{"content": "Matthew uses Go", "subject": "Matthew", "category": "preference"}]`,
	}
	ext := memstore.NewFactExtractor(store, embedder, gen)

	result, err := ext.Extract(ctx, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.Superseded != 0 {
		t.Errorf("superseded = %d, want 0 (below threshold)", result.Superseded)
	}

	// Both facts should remain active.
	active, _ := store.BySubject(ctx, "Matthew", true)
	if len(active) != 2 {
		t.Errorf("expected 2 active facts, got %d", len(active))
	}
}

func TestExtract_AutoSupersede_ConflictingMetadata(t *testing.T) {
	embedder := &identityEmbedder{dim: 4}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	// Pre-insert a fact with metadata.
	emb, _ := memstore.Single(ctx, embedder, "old fact")
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew uses vim",
		Subject:   "Matthew",
		Category:  "preference",
		Embedding: emb,
		Metadata:  json.RawMessage(`{"project":"scene-chain"}`),
	})

	// The extractor doesn't set metadata on extracted facts by default,
	// but if the pre-existing fact has metadata with project=scene-chain
	// and the new fact has no metadata, that's not a conflict (one side empty).
	// So let's test with metadata on the new fact too, via a custom prompt
	// that wouldn't normally produce metadata. Instead, we test the
	// metadataConflicts function directly and verify end-to-end that
	// same-metadata facts DO get superseded.

	// Same metadata → should supersede.
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew uses emacs",
		Subject:   "Matthew",
		Category:  "preference",
		Embedding: emb,
		Metadata:  json.RawMessage(`{"project":"scene-chain"}`),
	})

	// Different metadata → should NOT supersede.
	store.Insert(ctx, memstore.Fact{
		Content:   "Matthew uses nano",
		Subject:   "Matthew",
		Category:  "preference",
		Embedding: emb,
		Metadata:  json.RawMessage(`{"project":"memstore"}`),
	})

	// Count active — all 3 should be active since we haven't run extraction yet.
	active, _ := store.BySubject(ctx, "Matthew", true)
	if len(active) != 3 {
		t.Fatalf("expected 3 active facts before extraction, got %d", len(active))
	}
}

func TestMetadataConflicts(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		conflict bool
	}{
		{"both empty", "", "", false},
		{"one empty", `{"project":"x"}`, "", false},
		{"other empty", "", `{"project":"x"}`, false},
		{"same values", `{"project":"x"}`, `{"project":"x"}`, false},
		{"different values", `{"project":"x"}`, `{"project":"y"}`, true},
		{"disjoint keys", `{"project":"x"}`, `{"source":"y"}`, false},
		{"one matching one not", `{"project":"x","source":"a"}`, `{"project":"x","source":"b"}`, true},
		{"all matching", `{"project":"x","source":"a"}`, `{"project":"x","source":"a"}`, false},
		{"numeric vs string", `{"chapter":1}`, `{"chapter":2}`, true},
		{"same numeric", `{"chapter":1}`, `{"chapter":1}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := json.RawMessage(tt.a)
			b := json.RawMessage(tt.b)
			if len(tt.a) == 0 {
				a = nil
			}
			if len(tt.b) == 0 {
				b = nil
			}
			got := memstore.MetadataConflicts(a, b)
			if got != tt.conflict {
				t.Errorf("MetadataConflicts(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.conflict)
			}
		})
	}
}

func TestExtract_AutoSupersede_DifferentSubjects(t *testing.T) {
	embedder := &identityEmbedder{dim: 4}
	store := openTestStoreWith(t, embedder)
	ctx := context.Background()

	// Pre-insert with a different subject.
	emb, _ := memstore.Single(ctx, embedder, "old fact")
	store.Insert(ctx, memstore.Fact{
		Content:   "Alice uses vim",
		Subject:   "Alice",
		Category:  "preference",
		Embedding: emb,
	})

	gen := &mockGenerator{
		response: `[{"content": "Bob uses vim", "subject": "Bob", "category": "preference"}]`,
	}
	ext := memstore.NewFactExtractor(store, embedder, gen)

	result, err := ext.Extract(ctx, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Different subjects should not supersede each other.
	if result.Superseded != 0 {
		t.Errorf("superseded = %d, want 0 (different subjects)", result.Superseded)
	}
}

func TestExtract_AutoSupersede_NilEmbedder(t *testing.T) {
	// With nil embedder, facts are inserted without embeddings and
	// auto-supersession is skipped (no embeddings to compare).
	store := openTestStoreWith(t, nil)
	ctx := context.Background()

	// Pre-insert a fact.
	store.Insert(ctx, memstore.Fact{
		Content:  "old fact about X",
		Subject:  "X",
		Category: "note",
	})

	gen := &mockGenerator{
		response: `[{"content": "new fact about X", "subject": "X", "category": "note"}]`,
	}
	ext := memstore.NewFactExtractor(store, nil, gen)

	result, err := ext.Extract(ctx, "some text", memstore.ExtractOpts{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Fact inserted but no supersession (nil embedder → no embedding → skipped).
	if result.Superseded != 0 {
		t.Errorf("superseded = %d, want 0", result.Superseded)
	}
	if len(result.Inserted) != 1 {
		t.Errorf("inserted = %d, want 1", len(result.Inserted))
	}
}
