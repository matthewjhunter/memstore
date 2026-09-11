package httpapi

import (
	"math"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// Fresh ratings -- weight equal to count -- keep the curve recall has always
// used, so decay changes nothing for ratings made today.
func TestFeedbackMultiplierFreshMatchesTheOldCurve(t *testing.T) {
	for _, c := range []struct {
		stat memstore.FeedbackStat
		want float64
	}{
		{memstore.FeedbackStat{Avg: 1, Count: 1, Weight: 1}, math.Pow(2, 0.52)},
		{memstore.FeedbackStat{Avg: -1, Count: 1, Weight: 1}, math.Pow(2, -0.52)},
		{memstore.FeedbackStat{Avg: -1, Count: 5, Weight: 5}, 0.5},
		{memstore.FeedbackStat{Avg: 1, Count: 9, Weight: 9}, 2},
	} {
		if got := feedbackMultiplier(c.stat); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%+v: multiplier %.4f, want %.4f", c.stat, got, c.want)
		}
	}
}

// Old ratings fade toward neutral. The confidence floor has to shrink with the
// weight too: decaying only the count would leave every rated fact at least
// 0.4 of the way to its full multiplier forever, which is the frozen demotion
// #161 describes.
func TestFeedbackMultiplierFadesWithWeight(t *testing.T) {
	if got := feedbackMultiplier(memstore.FeedbackStat{Avg: -1, Count: 5, Weight: 0}); got != 1 {
		t.Errorf("zero weight: multiplier %.4f, want exactly 1", got)
	}
	if got := feedbackMultiplier(memstore.FeedbackStat{Avg: -1, Count: 5, Weight: 0.05}); got < 0.98 {
		t.Errorf("five ratings from months ago (weight 0.05): multiplier %.4f, want at least 0.98", got)
	}
	prev := 1.0
	for _, w := range []float64{0.1, 0.5, 1, 2, 5} {
		got := feedbackMultiplier(memstore.FeedbackStat{Avg: -1, Count: 5, Weight: w})
		if got >= prev {
			t.Errorf("weight %.1f: multiplier %.4f, want below %.4f -- more weight must demote more", w, got, prev)
		}
		prev = got
	}
}
