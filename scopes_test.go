package memstore_test

import (
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

func TestValidateScopes(t *testing.T) {
	tests := []struct {
		name    string
		scopes  []string
		wantErr bool
		// errMentions, when set, must appear in the error text -- the point of
		// validating is a usable message, not just a rejection.
		errMentions string
	}{
		{
			// The common case, and it must stay legal: an unscoped token gets
			// the legacy read+write grant.
			name:   "empty is valid",
			scopes: nil,
		},
		{name: "read", scopes: []string{memstore.ScopeRead}},
		{name: "write", scopes: []string{memstore.ScopeWrite}},
		{name: "admin", scopes: []string{memstore.ScopeAdmin}},
		{name: "ingest", scopes: []string{memstore.ScopeIngest}},
		{
			name:   "combination",
			scopes: []string{memstore.ScopeRead, memstore.ScopeIngest},
		},
		{
			// The motivating bug: a typo produces a token that authenticates
			// and can do nothing, with no error at issue time saying why.
			name:        "typo is rejected and named",
			scopes:      []string{"ingset"},
			wantErr:     true,
			errMentions: "ingset",
		},
		{
			// slices.Contains is case-sensitive, so this would silently fail
			// closed at request time.
			name:        "wrong case is rejected",
			scopes:      []string{"Ingest"},
			wantErr:     true,
			errMentions: "Ingest",
		},
		{
			name:        "one bad scope among good ones is rejected",
			scopes:      []string{memstore.ScopeRead, "supseruser"},
			wantErr:     true,
			errMentions: "supseruser",
		},
		{
			name:        "error lists the valid scopes",
			scopes:      []string{"bogus"},
			wantErr:     true,
			errMentions: memstore.ScopeIngest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := memstore.ValidateScopes(tc.scopes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateScopes(%v) error = %v, wantErr = %v",
					tc.scopes, err, tc.wantErr)
			}
			if tc.errMentions != "" && !strings.Contains(err.Error(), tc.errMentions) {
				t.Errorf("error %q does not mention %q", err, tc.errMentions)
			}
		})
	}
}
