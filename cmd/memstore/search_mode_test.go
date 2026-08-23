package main

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveSearchMode(t *testing.T) {
	embFailed := errors.New("embedder config: unknown provider")

	tests := []struct {
		name       string
		mode       string
		remote     bool
		embErr     error
		wantHybrid bool
		wantNote   bool
		wantErr    bool
	}{
		// Remote: the daemon owns the embedder, so hybrid costs the CLI
		// nothing and is what "as configured" means here. A local embedder is
		// never built, so its config cannot matter.
		{name: "auto remote is hybrid", mode: modeAuto, remote: true, wantHybrid: true},
		{name: "auto remote ignores local embedder trouble", mode: modeAuto, remote: true, embErr: embFailed, wantHybrid: true},
		{name: "explicit hybrid remote", mode: modeHybrid, remote: true, wantHybrid: true},

		// Local with a usable embedder behaves the same.
		{name: "auto local with embedder is hybrid", mode: modeAuto, wantHybrid: true},
		{name: "explicit hybrid local with embedder", mode: modeHybrid, wantHybrid: true},

		// Local with no usable embedder: auto degrades and says so, an
		// explicit request fails rather than quietly returning less.
		{name: "auto local without embedder degrades", mode: modeAuto, embErr: embFailed, wantHybrid: false, wantNote: true},
		{name: "explicit hybrid local without embedder errors", mode: modeHybrid, embErr: embFailed, wantErr: true},

		// fts is a forced choice and never errors or warns.
		{name: "fts remote", mode: modeFTS, remote: true, wantHybrid: false},
		{name: "fts local", mode: modeFTS, wantHybrid: false},
		{name: "fts local ignores embedder trouble", mode: modeFTS, embErr: embFailed, wantHybrid: false},

		{name: "unknown mode is rejected", mode: "semantic", wantErr: true},
		{name: "empty mode is rejected", mode: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hybrid, note, err := resolveSearchMode(tt.mode, tt.remote, tt.embErr)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got hybrid=%v note=%q", hybrid, note)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hybrid != tt.wantHybrid {
				t.Errorf("hybrid = %v, want %v", hybrid, tt.wantHybrid)
			}
			if gotNote := note != ""; gotNote != tt.wantNote {
				t.Errorf("note = %q, want a note: %v", note, tt.wantNote)
			}
		})
	}
}

// The degrade note has to name the cause, or a user seeing keyword-only
// results has nothing to act on.
func TestResolveSearchModeNoteExplainsItself(t *testing.T) {
	_, note, err := resolveSearchMode(modeAuto, false, errors.New("boom: bad provider"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(note, "boom: bad provider") {
		t.Errorf("note %q does not carry the underlying reason", note)
	}
	if !strings.Contains(note, modeFTS) {
		t.Errorf("note %q does not say which mode it fell back to", note)
	}
}

// An explicit hybrid request that cannot be honoured must say what to do about
// it, not just report the failure.
func TestResolveSearchModeErrorSuggestsFallback(t *testing.T) {
	_, _, err := resolveSearchMode(modeHybrid, false, errors.New("bad provider"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--search "+modeFTS) {
		t.Errorf("error %q does not point at --search %s", err, modeFTS)
	}
}

func TestSearchModesAreDocumented(t *testing.T) {
	// The usage string is how anyone discovers these; it must list every mode
	// the resolver accepts.
	for _, mode := range []string{modeAuto, modeHybrid, modeFTS} {
		if !strings.Contains(searchModeUsage, mode) {
			t.Errorf("usage string does not mention %q: %s", mode, searchModeUsage)
		}
	}
}
