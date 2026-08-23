package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

func whoami(t *testing.T, h http.Handler, token string) (int, httpapi.WhoAmIResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	res := w.Result()
	var out httpapi.WhoAmIResponse
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("decode whoami: %v", err)
		}
	}
	return res.StatusCode, out
}

// The point of the endpoint: a caller learns what it may do without having to
// probe a write route and get a 403. The effective set is computed server-side
// by Identity.Allows, so there is exactly one implementation of the implication
// rules and a client cannot drift from them.
func TestWhoAmIReportsEffectiveScopes(t *testing.T) {
	h := newScopeHandler(t)

	tests := []struct {
		name       string
		token      string
		wantAllows []string
	}{
		{"read token", "tok-read", []string{httpapi.ScopeRead}},
		// Read is not implied by write: Allows only special-cases the empty
		// set and admin, so a write-only token really cannot read.
		{"write token", "tok-write", []string{httpapi.ScopeWrite}},
		{"admin token implies read and write", "tok-admin", []string{httpapi.ScopeRead, httpapi.ScopeWrite, httpapi.ScopeAdmin}},
		{"ingest token gets only ingest", "tok-ingest", []string{httpapi.ScopeIngest}},
		// Pre-enforcement tokens carry no scopes and keep read+write.
		{"legacy token", "tok-legacy", []string{httpapi.ScopeRead, httpapi.ScopeWrite}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, got := whoami(t, h, tt.token)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			slices.Sort(got.Allows)
			want := slices.Clone(tt.wantAllows)
			slices.Sort(want)
			if !slices.Equal(got.Allows, want) {
				t.Errorf("allows = %v, want %v", got.Allows, want)
			}
			if !got.Authenticated {
				t.Error("authenticated = false, want true")
			}
		})
	}
}

// Ingest is implied by nothing, including admin. Asserted on its own because
// it is the rule the document corpus's provenance guarantee rests on, and a
// client shaping itself from this response must not be told otherwise.
func TestWhoAmINeverImpliesIngest(t *testing.T) {
	h := newScopeHandler(t)
	for _, token := range []string{"tok-read", "tok-write", "tok-admin", "tok-legacy"} {
		_, got := whoami(t, h, token)
		if slices.Contains(got.Allows, httpapi.ScopeIngest) {
			t.Errorf("token %s reports ingest; ingest must be granted explicitly", token)
		}
	}
}

// Whoami carries no scope requirement of its own: any valid token may ask what
// it can do, including one that cannot read facts.
func TestWhoAmINeedsNoScope(t *testing.T) {
	h := newScopeHandler(t)
	if code, _ := whoami(t, h, "tok-ingest"); code != http.StatusOK {
		t.Errorf("ingest-only token got %d asking whoami; the route must not require a scope", code)
	}
}

func TestWhoAmIRejectsBadToken(t *testing.T) {
	h := newScopeHandler(t)
	if code, _ := whoami(t, h, "nonsense"); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// In an unauthenticated deployment requireScope passes everything through, so
// the honest answer is that everything is allowed. Reporting read-only here
// would make a client hide tools that in fact work.
func TestWhoAmIUnauthenticatedDeployment(t *testing.T) {
	h, _ := newTestHandler(t)

	code, got := whoami(t, h, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Authenticated {
		t.Error("authenticated = true on a handler with no auth configured")
	}
	for _, scope := range []string{httpapi.ScopeRead, httpapi.ScopeWrite, httpapi.ScopeAdmin, httpapi.ScopeIngest} {
		if !slices.Contains(got.Allows, scope) {
			t.Errorf("allows lacks %s; unauthenticated deployments permit every route", scope)
		}
	}
}
