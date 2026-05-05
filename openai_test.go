package memstore_test

import (
	"testing"

	"github.com/matthewjhunter/memstore"
)

func TestOpenAIGenerator_ModelName(t *testing.T) {
	gen := memstore.NewOpenAIGenerator("http://localhost:11434", "", "llama3")
	if gen.Model() != "llama3" {
		t.Errorf("expected model 'llama3', got %q", gen.Model())
	}
}
