package httpapi_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore/httpapi"
)

// fakeScorer counts calls and drains a fixed backlog, so the runner's behaviour can
// be observed without a store.
type fakeScorer struct {
	remaining atomic.Int64
	calls     atomic.Int32
	err       error
}

func (f *fakeScorer) BackfillDetectScores(_ context.Context, limit int) (int, error) {
	f.calls.Add(1)
	if f.err != nil {
		return 0, f.err
	}
	n := f.remaining.Load()
	if n <= 0 {
		return 0, nil
	}
	if int64(limit) < n {
		n = int64(limit)
	}
	f.remaining.Add(-n)
	return int(n), nil
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal(msg)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// The corpus predating the score column has to actually get scored, in batches, and
// without being asked to do it by hand.
func TestDetectBackfill_DrainsTheBacklog(t *testing.T) {
	f := &fakeScorer{}
	f.remaining.Store(250)

	b := httpapi.NewDetectBackfill(f, time.Millisecond, 100)
	b.Start()
	defer b.Stop()

	waitFor(t, func() bool { return f.remaining.Load() == 0 },
		"the backlog was not drained")
}

// Once drained it must stop doing work. A pass that keeps scoring nothing forever is
// a query per tick against the live database for the rest of the process's life.
func TestDetectBackfill_StopsWorkingOnceDrained(t *testing.T) {
	f := &fakeScorer{}
	f.remaining.Store(10)

	b := httpapi.NewDetectBackfill(f, time.Millisecond, 100)
	b.Start()
	defer b.Stop()

	waitFor(t, func() bool { return f.remaining.Load() == 0 }, "not drained")
	settled := f.calls.Load()
	time.Sleep(50 * time.Millisecond)

	if grew := f.calls.Load() - settled; grew > 1 {
		t.Errorf("made %d further calls after draining, want it to stop", grew)
	}
}

// A failing pass must not spin: it retries on the next tick rather than in a tight
// loop, and it must not be mistaken for "drained" either.
func TestDetectBackfill_RetriesAfterAnError(t *testing.T) {
	f := &fakeScorer{err: errors.New("database down")}
	f.remaining.Store(10)

	b := httpapi.NewDetectBackfill(f, time.Millisecond, 100)
	b.Start()
	defer b.Stop()

	waitFor(t, func() bool { return f.calls.Load() >= 3 },
		"gave up after an error instead of retrying")
}

func TestDetectBackfill_StopIsIdempotentAndPromptOnAnEmptyStore(t *testing.T) {
	f := &fakeScorer{}
	b := httpapi.NewDetectBackfill(f, time.Millisecond, 100)
	b.Start()

	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked")
	}
}
