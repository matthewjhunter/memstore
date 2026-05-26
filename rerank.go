package memstore

import (
	"fmt"
	"os"
	"strconv"

	"github.com/matthewjhunter/go-embedding"
)

// RerankPolicy is memstore's per-deployment rerank search policy: how rerank
// scores are fused (Mode), the relevance floor (Threshold), and how many
// first-stage candidates are sent to the cross-encoder per pass (Candidates).
// It is distinct from go-embedding's RerankConfig, which says which backend to
// call. Candidates == 0 means "use the built-in default" (DefaultRerankCandidates
// for search; the recall pipeline's own cap for recall).
type RerankPolicy struct {
	Mode       RerankMode
	Threshold  float64
	Candidates int
}

// RerankPolicyFromEnv reads the rerank policy from the {prefix}_MODE /
// {prefix}_THRESHOLD / {prefix}_CANDIDATES env vars, cascading to RERANK_MODE /
// RERANK_THRESHOLD / RERANK_CANDIDATES. These are memstore search policy (how
// rerank scores are fused, filtered, and how big the candidate pass is),
// distinct from go-embedding's RerankConfig (which backend to call). Unset
// values yield RerankOff / 0 / 0. A malformed mode, an out-of-[0,1] threshold,
// or a non-positive candidate count is an error.
func RerankPolicyFromEnv(prefix string) (RerankPolicy, error) {
	get := func(suffix string) string {
		if v := os.Getenv(prefix + suffix); v != "" {
			return v
		}
		return os.Getenv("RERANK" + suffix)
	}

	var pol RerankPolicy
	if v := get("_MODE"); v != "" {
		m, err := ParseRerankMode(v)
		if err != nil {
			return RerankPolicy{}, err
		}
		pol.Mode = m
	}

	if v := get("_THRESHOLD"); v != "" {
		t, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return RerankPolicy{}, fmt.Errorf("memstore: invalid rerank threshold %q: %w", v, err)
		}
		if t < 0 || t > 1 {
			return RerankPolicy{}, fmt.Errorf("memstore: rerank threshold %v out of range [0,1]", t)
		}
		pol.Threshold = t
	}

	if v := get("_CANDIDATES"); v != "" {
		c, err := strconv.Atoi(v)
		if err != nil {
			return RerankPolicy{}, fmt.Errorf("memstore: invalid rerank candidates %q: %w", v, err)
		}
		if c <= 0 {
			return RerankPolicy{}, fmt.Errorf("memstore: rerank candidates %d must be positive", c)
		}
		pol.Candidates = c
	}
	return pol, nil
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
