package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

func testResource(t *testing.T) httpapi.ProtectedResource {
	t.Helper()
	return httpapi.ProtectedResource{
		PublicBaseURL:        "https://memstore.example.test",
		Prefix:               httpapi.DefaultPrefix,
		AuthorizationServers: []string{"https://webauth.example.test/t/memstore"},
		ScopesSupported:      []string{httpapi.ScopeRead, httpapi.ScopeWrite},
	}
}

// RFC 9728 derives the metadata location by INSERTING the resource path after
// the well-known segment, not by nesting the well-known document under the
// resource. Getting this backwards produces a document nothing looks for.
func TestProtectedResourceURLsAreTheRFC9728Form(t *testing.T) {
	p := testResource(t)

	if got, want := p.ResourceURL(), "https://memstore.example.test/memstore/mcp"; got != want {
		t.Errorf("ResourceURL() = %q, want %q", got, want)
	}
	if got, want := p.MetadataPath(), "/.well-known/oauth-protected-resource/memstore/mcp"; got != want {
		t.Errorf("MetadataPath() = %q, want %q", got, want)
	}
	if got, want := p.MetadataURL(), "https://memstore.example.test/.well-known/oauth-protected-resource/memstore/mcp"; got != want {
		t.Errorf("MetadataURL() = %q, want %q", got, want)
	}
	// The nested form is the plausible mistake; make sure we did not build it.
	if strings.Contains(p.MetadataPath(), "/memstore/.well-known") {
		t.Errorf("MetadataPath() = %q nests the well-known document under the prefix", p.MetadataPath())
	}
}

func TestProtectedResourceMetadataDeclaresTheResource(t *testing.T) {
	p := testResource(t)
	meta := p.Metadata()

	if meta.Resource != p.ResourceURL() {
		t.Errorf("metadata resource = %q, want %q", meta.Resource, p.ResourceURL())
	}
	if !slices.Equal(meta.AuthorizationServers, p.AuthorizationServers) {
		t.Errorf("authorization_servers = %v, want %v", meta.AuthorizationServers, p.AuthorizationServers)
	}
	if !slices.Contains(meta.BearerMethodsSupported, "header") {
		t.Errorf("bearer_methods_supported = %v, want it to include header", meta.BearerMethodsSupported)
	}
}

// A client picks the scopes to request from here, and the OAuth path strips
// ingest on the way in regardless -- so advertising it would only invite a
// grant memstore refuses to honour. The configured value deliberately includes
// ingest: asserting its absence against a fixture that never contained it
// proves nothing.
func TestProtectedResourceMetadataNeverAdvertisesIngest(t *testing.T) {
	p := testResource(t)
	p.ScopesSupported = []string{httpapi.ScopeRead, httpapi.ScopeIngest, httpapi.ScopeWrite}

	meta := p.Metadata()
	if slices.Contains(meta.ScopesSupported, httpapi.ScopeIngest) {
		t.Errorf("scopes_supported = %v, which advertises ingest", meta.ScopesSupported)
	}
	// The rest of the grant must survive the filtering.
	if !slices.Contains(meta.ScopesSupported, httpapi.ScopeRead) ||
		!slices.Contains(meta.ScopesSupported, httpapi.ScopeWrite) {
		t.Errorf("scopes_supported = %v, want read and write retained", meta.ScopesSupported)
	}
}

func TestProtectedResourceValidateRejectsIncomplete(t *testing.T) {
	full := testResource(t)
	tests := []struct {
		name   string
		break_ func(p *httpapi.ProtectedResource)
	}{
		{"no public base URL", func(p *httpapi.ProtectedResource) { p.PublicBaseURL = "" }},
		{"no authorization servers", func(p *httpapi.ProtectedResource) { p.AuthorizationServers = nil }},
		{"non-absolute public base URL", func(p *httpapi.ProtectedResource) { p.PublicBaseURL = "memstore.example.test" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := full
			tt.break_(&p)
			if err := p.Validate(); err == nil {
				t.Error("Validate accepted an incomplete resource")
			}
		})
	}
	if err := full.Validate(); err != nil {
		t.Errorf("Validate rejected a complete resource: %v", err)
	}
}

// The metadata document is public discovery data and must be reachable without
// a credential -- a client fetches it precisely because it does not have one
// yet. It also must be served by the host rather than the API module, because
// well-known URIs are root-scoped and StripPrefix hides the prefix from the
// module.
func TestMountServesMetadataUnauthenticated(t *testing.T) {
	p := testResource(t)
	api := newTestHandlerWith(t, httpapi.WithTokenVerifier(failingVerifier{err: errNope}))
	top := httpapi.Mount(httpapi.DefaultPrefix, api, httpapi.WithProtectedResourceMetadata(p))

	req := httptest.NewRequest(http.MethodGet, p.MetadataPath(), nil)
	rec := httptest.NewRecorder()
	top.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decoding metadata: %v", err)
	}
	if doc.Resource != p.ResourceURL() {
		t.Errorf("resource = %q, want %q", doc.Resource, p.ResourceURL())
	}
	if len(doc.AuthorizationServers) == 0 {
		t.Error("authorization_servers is empty")
	}
}

// Without the option, the reserved well-known space stays reserved: the host
// answers 404 there rather than anything under the API.
func TestMountWithoutMetadataStillReservesWellKnown(t *testing.T) {
	api := newTestHandlerWith(t)
	top := httpapi.Mount(httpapi.DefaultPrefix, api)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/memstore/mcp", nil)
	rec := httptest.NewRecorder()
	top.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- A2: the challenge -----------------------------------------------------

// A 401 with no WWW-Authenticate tells a client it is unauthorized and nothing
// about how to fix that. The header is what makes the flow discoverable, and
// without it an MCP client cannot start authenticating no matter how correct
// the rest of the deployment is.
func TestUnauthorizedMCPRequestCarriesTheChallenge(t *testing.T) {
	p := testResource(t)
	h := newTestHandlerWith(t,
		httpapi.WithTokenVerifier(failingVerifier{err: errNope}),
		httpapi.WithProtectedResource(p),
	)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	if got == "" {
		t.Fatal("no WWW-Authenticate header on a 401")
	}
	if !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("WWW-Authenticate = %q, want it to start with Bearer", got)
	}
	if !strings.Contains(got, `resource_metadata="`+p.MetadataURL()+`"`) {
		t.Errorf("WWW-Authenticate = %q, want it to carry resource_metadata=%q", got, p.MetadataURL())
	}
}

// A request with no Authorization header at all is the first thing a client
// sends. It is the most important case for the challenge to appear on.
func TestUnauthenticatedMCPRequestCarriesTheChallenge(t *testing.T) {
	p := testResource(t)
	h := newTestHandlerWith(t,
		httpapi.WithTokenVerifier(failingVerifier{err: errNope}),
		httpapi.WithProtectedResource(p),
	)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("no WWW-Authenticate header on an unauthenticated MCP request")
	}
}

// A 503 is not a challenge. Advertising where to authenticate when the problem
// is that we cannot check anything would send a client into the flow it is
// least able to complete.
func TestUnavailableAuthDoesNotCarryTheChallenge(t *testing.T) {
	p := testResource(t)
	h := newTestHandlerWith(t,
		httpapi.WithTokenVerifier(failingVerifier{err: httpapi.ErrAuthUnavailable}),
		httpapi.WithProtectedResource(p),
	)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("503 carried a challenge: %q", got)
	}
}

// The metadata describes the MCP endpoint. The REST surface has its own callers
// and its own credentials, and whether it moves to OAuth is still open -- so it
// must not advertise a resource document that does not describe it.
func TestNonMCPUnauthorizedRequestHasNoChallenge(t *testing.T) {
	p := testResource(t)
	h := newTestHandlerWith(t,
		httpapi.WithTokenVerifier(failingVerifier{err: errNope}),
		httpapi.WithProtectedResource(p),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("REST 401 advertised the MCP resource document: %q", got)
	}
}

// Deployments that have not configured OAuth must behave exactly as before.
func TestNoChallengeWhenOAuthIsNotConfigured(t *testing.T) {
	h := newTestHandlerWith(t, httpapi.WithTokenVerifier(failingVerifier{err: errNope}))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("unconfigured deployment emitted a challenge: %q", got)
	}
}

var errNope = errorString("no such token")

type errorString string

func (e errorString) Error() string { return string(e) }

// What the document advertises must be what the verifier accepts. A client
// reads scopes_supported to build its authorization request, so advertising
// bare names while the verifier expects namespaced ones would send every client
// to ask for scopes memstore then ignores.
func TestProtectedResourceAdvertisesPrefixedScopes(t *testing.T) {
	p := testResource(t)
	p.ScopePrefix = "memstore:"
	p.ScopesSupported = []string{httpapi.ScopeRead, httpapi.ScopeIngest, httpapi.ScopeWrite}

	meta := p.Metadata()
	if !slices.Contains(meta.ScopesSupported, "memstore:read") {
		t.Errorf("scopes_supported = %v, want it to include memstore:read", meta.ScopesSupported)
	}
	// Ingest stays out in either form.
	for _, bad := range []string{"ingest", "memstore:ingest"} {
		if slices.Contains(meta.ScopesSupported, bad) {
			t.Errorf("scopes_supported = %v, which advertises %q", meta.ScopesSupported, bad)
		}
	}
}
