package memstore

import "testing"

// poolVectors is what deduplication compares facts on, so its properties
// matter: every chunk has to count, and magnitude must not decide the outcome.

func TestPoolVectors_SingleVectorPoolsToItself(t *testing.T) {
	v := []float32{1, 2, 3}
	if got := poolVectors([][]float32{v}); !sameVec(got, v) {
		t.Errorf("poolVectors(one) = %v, want %v -- an unsplit fact keeps its own vector", got, v)
	}
}

// Dropping a chunk must change the result. If it does not, the tail of a long
// fact is invisible to deduplication -- the loss chunking removed, reintroduced.
func TestPoolVectors_EveryVectorCounts(t *testing.T) {
	all := [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

	if sameVec(poolVectors(all), poolVectors(all[:2])) {
		t.Error("dropping the last vector did not change the centroid")
	}
}

// A chunk with a larger magnitude must not dominate: cosine reads direction,
// so the centroid is over unit vectors.
func TestPoolVectors_MagnitudeDoesNotDominate(t *testing.T) {
	balanced := poolVectors([][]float32{{1, 0}, {0, 1}})
	lopsided := poolVectors([][]float32{{100, 0}, {0, 1}})

	if !sameVec(balanced, lopsided) {
		t.Errorf("centroid moved from %v to %v when one vector was scaled up; "+
			"vectors must be normalised before averaging", balanced, lopsided)
	}
}

func TestPoolVectors_RaggedDimensionsYieldNothing(t *testing.T) {
	if got := poolVectors([][]float32{{1, 0}, {0, 1, 0}}); got != nil {
		t.Errorf("got %v for mismatched dimensions, want nil", got)
	}
}

func sameVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	const eps = 1e-6
	for i := range a {
		d := a[i] - b[i]
		if d > eps || d < -eps {
			return false
		}
	}
	return true
}
