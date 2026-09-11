package httpapi

import (
	"math"

	"github.com/matthewjhunter/memstore"
)

// feedbackMultiplier is the factor recall applies to a fact's score from its
// ratings: up to feedbackMaxFactor for consistently useful ones, down to its
// inverse for consistently useless ones, 1 for none.
//
// Confidence grows with the ratings' weight -- their age-decayed sum, see
// memstore.FeedbackHalfLife -- in two parts: a floor of feedbackBaseWeight that
// one fresh rating earns, scaled by min(weight, 1) so it fades with the rating,
// and the remainder, reached at feedbackConfidenceCap. For fresh ratings, where
// weight equals count, this is the curve recall has always used. Decaying the
// count alone would leave every rated fact at least 0.4 of the way to its full
// multiplier forever, which is the frozen demotion #161 describes.
func feedbackMultiplier(stat memstore.FeedbackStat) float64 {
	if stat.Weight <= 0 {
		return 1
	}
	conf := math.Min(stat.Weight, feedbackConfidenceCap) / feedbackConfidenceCap
	exponent := stat.Avg * (feedbackBaseWeight*math.Min(stat.Weight, 1) + (1-feedbackBaseWeight)*conf)
	return math.Pow(feedbackMaxFactor, exponent)
}
