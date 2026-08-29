package main

import (
	"strings"
	"testing"
)

func TestResolveSearchMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantHybrid bool
		wantErr    bool
	}{
		{name: "auto is hybrid", mode: modeAuto, wantHybrid: true},
		{name: "explicit hybrid", mode: modeHybrid, wantHybrid: true},
		{name: "fts is keyword-only", mode: modeFTS, wantHybrid: false},
		{name: "unknown mode errors", mode: "sideways", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hybrid, err := resolveSearchMode(tt.mode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if hybrid != tt.wantHybrid {
				t.Errorf("hybrid = %v, want %v", hybrid, tt.wantHybrid)
			}
		})
	}
}

// The usage string must name every mode the resolver accepts, or the help
// text and the behaviour drift apart.
func TestSearchModesAreDocumented(t *testing.T) {
	for _, m := range []string{modeAuto, modeHybrid, modeFTS} {
		if !strings.Contains(searchModeUsage, m) {
			t.Errorf("searchModeUsage does not mention %q", m)
		}
	}
}
