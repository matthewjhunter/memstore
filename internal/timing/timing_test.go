package timing_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore/internal/timing"
)

// attrMap flattens the recorder's attributes for assertion.
func attrMap(attrs []slog.Attr) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value.Any()
	}
	return m
}

// Every recording call has to work on a context that never went near a
// recorder: the store is used by the CLI, by tests and in-process, and none of
// those should have to install one or change a call site.
func TestNoRecorderIsInert(t *testing.T) {
	ctx := context.Background()
	timing.Track(ctx, "embed")()
	timing.Record(ctx, "fts", 5*time.Millisecond)
	if got := timing.Attrs(ctx); got != nil {
		t.Errorf("Attrs on a bare context = %v, want nil", got)
	}
}

func TestRecordsPhaseTotalsAndCalls(t *testing.T) {
	ctx := timing.NewContext(context.Background())
	timing.Record(ctx, "fts", 10*time.Millisecond)
	timing.Record(ctx, "fts", 30*time.Millisecond)
	timing.Record(ctx, "embed", 310*time.Millisecond)

	m := attrMap(timing.Attrs(ctx))
	if got := m["fts_ms"]; got != 40.0 {
		t.Errorf("fts_ms = %v, want 40 (the sum across calls)", got)
	}
	if got := m["fts_calls"]; got != int64(2) {
		t.Errorf("fts_calls = %v, want 2", got)
	}
	if got := m["embed_ms"]; got != 310.0 {
		t.Errorf("embed_ms = %v, want 310", got)
	}
	if got := m["embed_calls"]; got != int64(1) {
		t.Errorf("embed_calls = %v, want 1", got)
	}
	if _, ok := m["vector_ms"]; ok {
		t.Error("a phase that never ran should contribute no attribute")
	}
}

func TestTrackMeasuresElapsed(t *testing.T) {
	ctx := timing.NewContext(context.Background())
	done := timing.Track(ctx, "vector")
	time.Sleep(2 * time.Millisecond)
	done()

	m := attrMap(timing.Attrs(ctx))
	ms, ok := m["vector_ms"].(float64)
	if !ok {
		t.Fatalf("vector_ms missing or not a float: %v", m)
	}
	if ms < 1 {
		t.Errorf("vector_ms = %v, want at least the 2ms that elapsed", ms)
	}
}

// Attribute order is fixed so a log line's shape does not change run to run
// for the same work -- map iteration would otherwise shuffle the fields.
func TestAttrsAreOrdered(t *testing.T) {
	ctx := timing.NewContext(context.Background())
	for _, phase := range []string{"vector", "embed", "rerank", "fts"} {
		timing.Record(ctx, phase, time.Millisecond)
	}
	var keys []string
	for _, a := range timing.Attrs(ctx) {
		keys = append(keys, a.Key)
	}
	want := []string{"embed_ms", "embed_calls", "fts_ms", "fts_calls",
		"rerank_ms", "rerank_calls", "vector_ms", "vector_calls"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("keys = %v, want %v", keys, want)
			break
		}
	}
}

// A batch search embeds several queries at once and recall reads the recorder
// while later stages may still be finishing; -race proves the lock.
func TestConcurrentRecording(t *testing.T) {
	ctx := timing.NewContext(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timing.Record(ctx, "fts", time.Millisecond)
			_ = timing.Attrs(ctx)
		}()
	}
	wg.Wait()

	if got := attrMap(timing.Attrs(ctx))["fts_calls"]; got != int64(50) {
		t.Errorf("fts_calls = %v, want 50", got)
	}
}

// Milliseconds are rounded to microsecond precision: a full float64 of
// nanosecond noise makes a log line hard to read and compares no better.
func TestMillisecondsAreRounded(t *testing.T) {
	ctx := timing.NewContext(context.Background())
	timing.Record(ctx, "embed", 1234567*time.Nanosecond)
	if got := attrMap(timing.Attrs(ctx))["embed_ms"]; got != 1.235 {
		t.Errorf("embed_ms = %v, want 1.235", got)
	}
}
