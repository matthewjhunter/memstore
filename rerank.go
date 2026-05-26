package memstore

import (
	"fmt"
	"os"
	"strconv"

	"github.com/matthewjhunter/go-embedding"
)

// RerankPolicy is memstore's per-deployment rerank search policy: how rerank
// scores are fused (Mode), the relevance floor (Threshold), and how many
// first-stage candidates are sent to the cross-encoder per pass. It is distinct
// from go-embedding's RerankConfig, which says which backend to call.
//
// Candidates and RecallCandidates are separate because the two paths have very
// different latency budgets: explicit search tolerates a larger pool, while
// recall runs per-prompt under a tight hook timeout and needs a small one. A
// zero value means "use the built-in default" (DefaultRerankCandidates for
// search; DefaultRecallRerankPool for recall).
type RerankPolicy struct {
	Mode             RerankMode
	Threshold        float64
	Candidates       int // search candidate pool
	RecallCandidates int // recall (per-prompt injection) candidate pool
}

// RerankPolicyFromEnv reads the rerank policy from the {prefix}_MODE /
// {prefix}_THRESHOLD / {prefix}_CANDIDATES / {prefix}_RECALL_CANDIDATES env
// vars, cascading to the bare RERANK_* names. These are memstore search policy
// (how rerank scores are fused, filtered, and how big each candidate pass is),
// distinct from go-embedding's RerankConfig (which backend to call). Unset
// values yield zero (use built-in defaults). A malformed mode, an out-of-[0,1]
// threshold, or a non-positive candidate count is an error.
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

	parsePool := func(suffix, label string) (int, error) {
		v := get(suffix)
		if v == "" {
			return 0, nil
		}
		c, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("memstore: invalid rerank %s %q: %w", label, v, err)
		}
		if c <= 0 {
			return 0, fmt.Errorf("memstore: rerank %s %d must be positive", label, c)
		}
		return c, nil
	}

	var err error
	if pol.Candidates, err = parsePool("_CANDIDATES", "candidates"); err != nil {
		return RerankPolicy{}, err
	}
	if pol.RecallCandidates, err = parsePool("_RECALL_CANDIDATES", "recall candidates"); err != nil {
		return RerankPolicy{}, err
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
