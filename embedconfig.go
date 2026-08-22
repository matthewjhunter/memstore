package memstore

import (
	"log"
	"os"

	embedding "github.com/matthewjhunter/go-embedding"
)

// EmbedEnvPrefix is the namespace memstore reads its embedder config from.
// go-embedding cascades MEMSTORE_EMBED_* -> EMBEDDING_* -> defaults.
const EmbedEnvPrefix = "MEMSTORE_EMBED"

// EmbedConfigFromEnv reads the embedder configuration every memstore binary
// shares, defaulting StrictModel on.
//
// Model identity decides two things that fail silently when the registry lookup
// misses: the task prefixes text is wrapped in, and the input budget. A miss
// yields neither and no error, so the symptom is not a failure but quietly worse
// recall -- or, against a backend that rejects oversize input hard rather than
// truncating, a per-fact failure with nothing naming the cause.
//
// Serving runtimes rename models freely: Ollama appends a tag, Lemonade appends
// -GGUF and takes a user. prefix. A rename on the backend is enough to turn a
// resolved model into an unresolved one, which is why this is strict by default
// rather than trusting the deployment to keep the name in registry form.
//
// Set MEMSTORE_EMBED_STRICT_MODEL=false (or EMBEDDING_STRICT_MODEL=false) to run
// a model the registry does not know, accepting no prefixes and no budget.
// Setting EMBEDDING_MODEL_ALIAS to map the served name onto a known one is
// almost always the better answer.
func EmbedConfigFromEnv() (embedding.Config, error) {
	cfg, err := embedding.ConfigFromEnvPrefix(EmbedEnvPrefix)
	if err != nil {
		return cfg, err
	}
	if !strictModelSetByOperator() {
		cfg.StrictModel = true
	}
	return cfg, nil
}

// strictModelSetByOperator reports whether the strict-model choice was made
// explicitly, so the default is applied only when nobody stated a preference.
func strictModelSetByOperator() bool {
	for _, k := range []string{EmbedEnvPrefix + "_STRICT_MODEL", "EMBEDDING_STRICT_MODEL"} {
		if _, ok := os.LookupEnv(k); ok {
			return true
		}
	}
	return false
}

// LogEmbedModel records what the configured model name actually resolved to, so
// the prefixes and budget in force are visible at startup rather than inferred
// from bad results later.
func LogEmbedModel(cfg embedding.Config) {
	info, known := embedding.LookupModel(cfg.Model)
	if !known {
		log.Printf("memstore: embedding model %q is unrecognised -- no task prefixes and no input budget", cfg.Model)
		return
	}
	log.Printf("memstore: embedding model %q resolved as %q (task prefixes: %v, budget: %d bytes)",
		cfg.Model, info.Canonical, info.HasPrompts, cfg.Limits().MaxBytes)
}
