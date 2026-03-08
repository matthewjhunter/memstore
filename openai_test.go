package memstore_test

import (
	"testing"

	"github.com/matthewjhunter/memstore"
)

func TestOpenAIEmbedder_ModelName(t *testing.T) {
	emb := memstore.NewOpenAIEmbedder("http://localhost:11434", "", "text-embedding-3-small")
	if emb.Model() != "text-embedding-3-small" {
		t.Errorf("expected model 'text-embedding-3-small', got %q", emb.Model())
	}
}

func TestOpenAIGenerator_ModelName(t *testing.T) {
	gen := memstore.NewOpenAIGenerator("http://localhost:11434", "", "llama3")
	if gen.Model() != "llama3" {
		t.Errorf("expected model 'llama3', got %q", gen.Model())
	}
}
