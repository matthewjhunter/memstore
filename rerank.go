package memstore

import (
	"fmt"
	"os"
	"strconv"

	"github.com/matthewjhunter/go-embedding"
)

// RerankPolicyFromEnv reads the rerank fusion mode and relevance threshold from
// the {prefix}_MODE / {prefix}_THRESHOLD env vars, cascading to RERANK_MODE /
// RERANK_THRESHOLD. These are memstore search policy (how rerank scores are
// fused and filtered), distinct from go-embedding's RerankConfig (which backend
// to call). Unset values yield RerankOff / 0. A malformed mode or an
// out-of-[0,1] threshold is an error.
func RerankPolicyFromEnv(prefix string) (RerankMode, float64, error) {
	get := func(suffix string) string {
		if v := os.Getenv(prefix + suffix); v != "" {
			return v
		}
		return os.Getenv("RERANK" + suffix)
	}

	mode := RerankOff
	if v := get("_MODE"); v != "" {
		m, err := ParseRerankMode(v)
		if err != nil {
			return RerankOff, 0, err
		}
		mode = m
	}

	var threshold float64
	if v := get("_THRESHOLD"); v != "" {
		t, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return RerankOff, 0, fmt.Errorf("memstore: invalid rerank threshold %q: %w", v, err)
		}
		if t < 0 || t > 1 {
			return RerankOff, 0, fmt.Errorf("memstore: rerank threshold %v out of range [0,1]", t)
		}
		threshold = t
	}
	return mode, threshold, nil
}

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
