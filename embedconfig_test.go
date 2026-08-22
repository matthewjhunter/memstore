package memstore_test

import (
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// The registry lookup is what supplies task prefixes and the input budget, and
// a miss supplies neither without erroring. These tests pin the startup
// behaviour that makes a miss loud.

func TestEmbedConfigFromEnv_DefaultsToStrictModel(t *testing.T) {
	t.Setenv("MEMSTORE_EMBED_MODEL", "nomic-embed-text")

	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StrictModel {
		t.Error("StrictModel is off by default; an unrecognised model would embed with no prefixes and no budget")
	}
}

// An operator who says "I know it is unregistered, run anyway" must be obeyed,
// or there is no escape hatch for a model the registry has never heard of.
func TestEmbedConfigFromEnv_OperatorCanDisableStrict(t *testing.T) {
	t.Setenv("MEMSTORE_EMBED_MODEL", "nomic-embed-text")
	t.Setenv("MEMSTORE_EMBED_STRICT_MODEL", "false")

	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StrictModel {
		t.Error("MEMSTORE_EMBED_STRICT_MODEL=false was overridden by the default")
	}
}

// The bare EMBEDDING_* name is the cascade go-embedding documents; an operator
// setting it deployment-wide must not be silently overridden either.
func TestEmbedConfigFromEnv_BareStrictNameIsHonoured(t *testing.T) {
	t.Setenv("MEMSTORE_EMBED_MODEL", "nomic-embed-text")
	t.Setenv("EMBEDDING_STRICT_MODEL", "false")

	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StrictModel {
		t.Error("EMBEDDING_STRICT_MODEL=false was overridden by the default")
	}
}

// The deployed model name carries an Ollama tag. Strict mode must resolve it
// through canonicalisation rather than refusing to start in production.
func TestEmbedConfigFromEnv_TaggedDeployedModelStillStarts(t *testing.T) {
	t.Setenv("MEMSTORE_EMBED_MODEL", "nomic-embed-text:latest")

	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedding.New(cfg); err != nil {
		t.Fatalf("strict mode rejected the deployed model name %q: %v", cfg.Model, err)
	}
}

// The point of the default: a model the registry cannot resolve must stop
// startup, not embed unprefixed and unbudgeted.
func TestEmbedConfigFromEnv_UnrecognisedModelRefusesToStart(t *testing.T) {
	t.Setenv("MEMSTORE_EMBED_MODEL", "not-a-real-embedding-model")

	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedding.New(cfg); err == nil {
		t.Error("an unrecognised model was accepted; no prefixes and no budget would apply silently")
	}
}
