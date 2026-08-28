package memstore

import (
	"fmt"
	"os"
	"strconv"

	"github.com/matthewjhunter/go-embedding"
)

// SimilarityPolicy holds the cosine-similarity gates that decide, without a
// person in the loop, whether two facts are related enough to link and whether
// a new fact is close enough to an existing one to supersede it.
//
// Both gates are on the embedding model's cosine scale, and that scale is not
// portable between models: for the same corpus, pairs that nomic-embed-text
// scores around 0.70 land around 0.53 under embeddinggemma, with the
// background floor moving by the same amount. A constant tuned for one model
// silently stops firing (or fires on noise) under another, so the defaults are
// keyed by model and the deployment can override them.
type SimilarityPolicy struct {
	// LinkMinSim is the minimum cosine between a newly extracted fact and an
	// existing one for the extract queue to record a "related" link.
	LinkMinSim float64
	// SupersedeMinSim is the minimum cosine between a newly extracted fact
	// and an existing same-subject fact for extraction to supersede the old
	// one automatically. Higher than the link gate because supersession
	// hides a live fact: a false positive here loses information, a false
	// negative only leaves a duplicate.
	SupersedeMinSim float64
	// Calibrated reports whether the values came from a measured per-model
	// default or an explicit override, as opposed to the historical constants
	// applied to a model nothing was measured on.
	Calibrated bool
}

// The historical constants, measured against nomic-embed-text on the live
// corpus. They remain the fallback for a model with no entry below.
const (
	DefaultLinkMinSim      = 0.60
	DefaultSupersedeMinSim = 0.85
)

// modelSimilarity is keyed by go-embedding's canonical model name. The
// embeddinggemma figures were measured 2026-08-28 on the production corpus:
// 60 pairs nomic had linked scored median 0.526 (p25 0.496) under gemma
// against a rank-50 floor of 0.484, so 0.50 keeps roughly three quarters of
// them while staying above the floor; 50 recent supersession pairs scored
// median 0.90 with a p10 of 0.73, and 0.80 catches 40 of them without
// reaching into the tail of legitimate rewrites.
var modelSimilarity = map[string]SimilarityPolicy{
	"nomic-embed-text":    {LinkMinSim: 0.60, SupersedeMinSim: 0.85, Calibrated: true},
	"nomic-embed-text-v2": {LinkMinSim: 0.60, SupersedeMinSim: 0.85, Calibrated: true},
	"embeddinggemma":      {LinkMinSim: 0.50, SupersedeMinSim: 0.80, Calibrated: true},
}

// DefaultSimilarityPolicy returns the gates measured for model, resolving tags
// and aliases the way go-embedding does ("embeddinggemma:300m" is
// embeddinggemma). A model with no measurement gets the historical constants
// with Calibrated false, so the daemon can say at startup that the gates are
// a guess.
func DefaultSimilarityPolicy(model string) SimilarityPolicy {
	info, _ := embedding.LookupModel(model)
	if p, ok := modelSimilarity[info.Canonical]; ok {
		return p
	}
	return SimilarityPolicy{LinkMinSim: DefaultLinkMinSim, SupersedeMinSim: DefaultSupersedeMinSim}
}

// SimilarityPolicyFromEnv starts from DefaultSimilarityPolicy(model) and
// applies {prefix}_LINK_MIN_SIM and {prefix}_SUPERSEDE_MIN_SIM where set. An
// override marks the policy calibrated: the operator has measured, or at
// least decided. A value that is not a number in [0,1] is an error.
func SimilarityPolicyFromEnv(prefix, model string) (SimilarityPolicy, error) {
	pol := DefaultSimilarityPolicy(model)
	parse := func(suffix string, dst *float64) error {
		v := os.Getenv(prefix + suffix)
		if v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("memstore: invalid %s%s %q: %w", prefix, suffix, v, err)
		}
		if f < 0 || f > 1 {
			return fmt.Errorf("memstore: %s%s %v out of range [0,1]", prefix, suffix, f)
		}
		*dst = f
		pol.Calibrated = true
		return nil
	}
	if err := parse("_LINK_MIN_SIM", &pol.LinkMinSim); err != nil {
		return SimilarityPolicy{}, err
	}
	if err := parse("_SUPERSEDE_MIN_SIM", &pol.SupersedeMinSim); err != nil {
		return SimilarityPolicy{}, err
	}
	return pol, nil
}
