package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

// scopeVerifier maps a bearer token straight to a scope set, so enforcement can
// be exercised without a Postgres token store. Distinct from handler_test.go's
// stubVerifier, which binds a single token to a fixed Identity.
type scopeVerifier map[string][]string

func (v scopeVerifier) VerifyToken(_ context.Context, token string) (httpapi.Identity, error) {
	scopes, ok := v[token]
	if !ok {
		return httpapi.Identity{}, errors.New("invalid")
	}
	return httpapi.Identity{Name: token, Scopes: scopes, Source: "bearer"}, nil
}

func newScopeHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWith(t, httpapi.WithTokenVerifier(scopeVerifier{
		"tok-read":   {httpapi.ScopeRead},
		"tok-write":  {httpapi.ScopeWrite},
		"tok-admin":  {httpapi.ScopeAdmin},
		"tok-ingest": {httpapi.ScopeIngest},
		"tok-legacy": nil,
	}))
}

func scopeRequest(t *testing.T, h http.Handler, token, method, path string) int {
	t.Helper()
	body := strings.NewReader(`{"content":"x","subject":"y"}`)
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result().StatusCode
}

func TestScopeEnforcement(t *testing.T) {
	h := newScopeHandler(t)

	tests := []struct {
		name     string
		token    string
		method   string
		path     string
		wantDeny bool
	}{
		{"read token can read", "tok-read", "GET", "/v1/facts", false},
		{"read token cannot write", "tok-read", "POST", "/v1/facts", true},
		{"write token can write", "tok-write", "POST", "/v1/facts", false},
		{"write token cannot read", "tok-write", "GET", "/v1/facts", true},
		{"admin can read", "tok-admin", "GET", "/v1/facts", false},
		{"admin can write", "tok-admin", "POST", "/v1/facts", false},
		{"legacy unscoped token can read", "tok-legacy", "GET", "/v1/facts", false},
		{"legacy unscoped token can write", "tok-legacy", "POST", "/v1/facts", false},
		{"ingest-only token cannot read facts", "tok-ingest", "GET", "/v1/facts", true},
		{"ingest-only token cannot write facts", "tok-ingest", "POST", "/v1/facts", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code := scopeRequest(t, h, tc.token, tc.method, tc.path)
			denied := code == http.StatusForbidden
			if denied != tc.wantDeny {
				t.Errorf("%s %s as %s: status %d (denied=%v), want denied=%v",
					tc.method, tc.path, tc.token, code, denied, tc.wantDeny)
			}
		})
	}
}

// An invalid credential is still a 401, not a 403 -- scope checks run after
// authentication, and the two failures must stay distinguishable.
func TestUnknownTokenIs401(t *testing.T) {
	h := newScopeHandler(t)
	if code := scopeRequest(t, h, "nope", "GET", "/v1/facts"); code != http.StatusUnauthorized {
		t.Errorf("unknown token: status %d, want 401", code)
	}
}

// Health stays reachable without any credential; monitoring depends on it.
func TestHealthNeedsNoScope(t *testing.T) {
	h := newScopeHandler(t)
	req := httptest.NewRequest("GET", "/v1/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode == http.StatusForbidden {
		t.Error("health endpoint should not require a scope")
	}
}

// With no auth configured at all there is no Identity, and every route must
// stay reachable -- that is the unauthenticated deployment mode, and the many
// existing tests that build a handler without a verifier depend on it.
func TestNoAuthConfiguredSkipsScopeChecks(t *testing.T) {
	h := newTestHandlerWith(t)
	req := httptest.NewRequest("GET", "/v1/facts", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode == http.StatusForbidden {
		t.Error("unauthenticated mode should not enforce scopes")
	}
}
