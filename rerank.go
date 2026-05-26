package memstore

import (
	"fmt"

	"github.com/matthewjhunter/go-embedding"
)

// RerankerFromEnv builds a second-stage Reranker from the {prefix}_* env
// namespace, cascading to RERANK_* (see embedding.RerankConfigFromEnvPrefix).
// It returns (nil, cfg, nil) when no endpoint and model are configured, meaning
// rerank is disabled — the common case, so callers can attach the result
// unconditionally via Store.SetReranker.
//
// The returned cfg lets the caller log what was configured (and warn if
// NormalizeScores is off for a raw-logit backend, which would break score
// fusion). A parse error in the env namespace is returned; a missing
// endpoint/model is not an error, just disabled.
func RerankerFromEnv(prefix string) (embedding.Reranker, embedding.RerankConfig, error) {
	cfg, err := embedding.RerankConfigFromEnvPrefix(prefix)
	if err != nil {
		return nil, cfg, fmt.Errorf("memstore: reranker config: %w", err)
	}
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, cfg, nil // not configured → rerank disabled
	}
	rr, err := embedding.NewReranker(cfg)
	if err != nil {
		return nil, cfg, fmt.Errorf("memstore: create reranker: %w", err)
	}
	return rr, cfg, nil
}
