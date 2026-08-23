package memstore

import (
	"log"

	embedding "github.com/matthewjhunter/go-embedding"
)

// EmbedEnvPrefix is the namespace memstore reads its embedder config from.
// go-embedding cascades MEMSTORE_EMBED_* -> EMBEDDING_* -> defaults.
const EmbedEnvPrefix = "MEMSTORE_EMBED"

// EmbedConfigFromEnv reads the embedder configuration every memstore binary
// shares, with StrictModel defaulted on.
//
// The default matters because model identity decides the task prefixes and the
// input budget, and both fail open: a registry miss yields a passthrough
// prompter and a zero budget with no error, so the symptom is quietly worse
// recall rather than a failure. Setting MEMSTORE_EMBED_STRICT_MODEL or the bare
// EMBEDDING_STRICT_MODEL opts out, and is never overridden.
func EmbedConfigFromEnv() (embedding.Config, error) {
	return embedding.ConfigFromEnvPrefixStrict(EmbedEnvPrefix)
}

// LogEmbedModel records what the configured model name actually resolved to, so
// the prefixes and budget in force are visible at startup rather than inferred
// from bad results later.
func LogEmbedModel(cfg embedding.Config) {
	log.Printf("memstore: %s", embedding.Describe(cfg))
}
