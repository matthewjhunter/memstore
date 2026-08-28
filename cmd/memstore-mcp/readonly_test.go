package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

type stubWhoAmI struct {
	res memstore.WhoAmIResponse
	err error
}

func (s stubWhoAmI) WhoAmI(context.Context) (memstore.WhoAmIResponse, error) {
	return s.res, s.err
}

func allows(scopes ...string) memstore.WhoAmIResponse {
	return memstore.WhoAmIResponse{Name: "tok", Authenticated: true, Allows: scopes}
}

func TestApplyTokenScopes(t *testing.T) {
	tests := []struct {
		name string
		stub stubWhoAmI
		flag bool
		want bool
	}{
		{"writable token", stubWhoAmI{res: allows(memstore.ScopeRead, memstore.ScopeWrite)}, false, false},
		{"read-only token", stubWhoAmI{res: allows(memstore.ScopeRead)}, false, true},
		{"daemon without the endpoint", stubWhoAmI{err: errors.New("404")}, false, false},
		{"flag forces read-only regardless", stubWhoAmI{res: allows(memstore.ScopeWrite)}, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyTokenScopes(context.Background(), tt.stub, tt.flag); got != tt.want {
				t.Errorf("applyTokenScopes = %v, want %v", got, tt.want)
			}
		})
	}
}

// The notice has to tell someone what to run, not only that they should stop.
func TestDeprecationNoticeNamesTheReplacement(t *testing.T) {
	for _, want := range []string{"DEPRECATED", "memstore setup", "MIGRATING.md"} {
		if !strings.Contains(deprecationNotice, want) {
			t.Errorf("deprecation notice lacks %q: %s", want, deprecationNotice)
		}
	}
}
