// Package timing records where a request spent its time, phase by phase, so
// the numbers can ride the access log line the request already produces.
//
// The recorder lives on the request context and every entry point is
// nil-safe: a context without one -- the CLI, a test, any in-process caller --
// runs the same code paths and records nothing. That is what lets the store
// and the handlers be instrumented without every caller having to opt in.
package timing

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"
)

// Phase names used across the search path. They are plain strings so a caller
// can add one without touching this package, but the ones that matter are
// named here so a typo in a hot path is caught by the compiler.
const (
	PhaseEmbed    = "embed"    // producing the query vector
	PhaseFTS      = "fts"      // full-text query against Postgres
	PhaseVector   = "vector"   // vector query against Postgres
	PhaseRerank   = "rerank"   // cross-encoder pass
	PhaseTriggers = "triggers" // evaluating CWD/file triggers
	PhaseFeedback = "feedback" // historical feedback lookup
)

type ctxKey struct{}

type phase struct {
	total time.Duration
	calls int64
}

// Recorder accumulates per-phase totals for one request. Its zero value is
// not usable; NewContext makes one.
type Recorder struct {
	mu     sync.Mutex
	phases map[string]*phase
	notes  map[string]slog.Value
}

// NewContext returns a context carrying a fresh recorder. Middleware installs
// one per request; everything below records into it without knowing it is
// there.
func NewContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, &Recorder{phases: make(map[string]*phase)})
}

// FromContext returns the recorder on ctx, or nil when there is none.
func FromContext(ctx context.Context) *Recorder {
	r, _ := ctx.Value(ctxKey{}).(*Recorder)
	return r
}

// Record adds one call of the named phase. A context with no recorder is a
// no-op, which is the common case outside the daemon.
func Record(ctx context.Context, name string, d time.Duration) {
	r := FromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.phases[name]
	if !ok {
		p = &phase{}
		r.phases[name] = p
	}
	p.total += d
	p.calls++
}

// Count attaches a number to the request's access line under name: how many
// candidates a search found, how many it returned. A time says how long the
// work took; a count says what it produced, and an empty result needs both to
// be read. Setting the same name again replaces the value. Like Record, it is
// a no-op on a context with no recorder.
func Count(ctx context.Context, name string, n int) {
	note(ctx, name, slog.Int64Value(int64(n)))
}

// Label attaches a short fixed word to the access line under name, such as the
// reason a result was empty. It is for values drawn from a small set the code
// names -- never request content, which does not belong in an access log.
func Label(ctx context.Context, name, value string) {
	note(ctx, name, slog.StringValue(value))
}

func note(ctx context.Context, name string, v slog.Value) {
	r := FromContext(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notes == nil {
		r.notes = make(map[string]slog.Value)
	}
	r.notes[name] = v
}

// Track starts a phase and returns the function that ends it, for the usual
//
//	defer timing.Track(ctx, timing.PhaseFTS)()
//
// A context with no recorder still returns a callable, so the call site needs
// no branch.
func Track(ctx context.Context, name string) func() {
	if FromContext(ctx) == nil {
		return func() {}
	}
	start := time.Now()
	return func() { Record(ctx, name, time.Since(start)) }
}

// Attrs returns the recorded phases as log attributes -- <phase>_ms and
// <phase>_calls -- sorted by phase name so a line's shape is stable across
// runs of the same work, followed by the counts and labels, sorted by name. It returns nil when nothing was recorded, so a
// request that did no measured work adds no fields.
func Attrs(ctx context.Context) []slog.Attr {
	r := FromContext(ctx)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.phases) == 0 && len(r.notes) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.phases))
	for name := range r.phases {
		names = append(names, name)
	}
	slices.Sort(names)

	attrs := make([]slog.Attr, 0, len(names)*2)
	for _, name := range names {
		p := r.phases[name]
		attrs = append(attrs,
			slog.Float64(name+"_ms", roundMS(p.total)),
			slog.Int64(name+"_calls", p.calls),
		)
	}
	noteNames := make([]string, 0, len(r.notes))
	for name := range r.notes {
		noteNames = append(noteNames, name)
	}
	slices.Sort(noteNames)
	for _, name := range noteNames {
		attrs = append(attrs, slog.Attr{Key: name, Value: r.notes[name]})
	}
	return attrs
}

// roundMS converts to milliseconds at microsecond precision: nanosecond noise
// makes a log line harder to read and compares no better.
func roundMS(d time.Duration) float64 {
	return math.Round(float64(d.Nanoseconds())/1e3) / 1e3
}
