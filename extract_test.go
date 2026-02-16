package memstore_test

import (
	"context"
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
