package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// OAuth protected-resource discovery (RFC 9728).
//
// This is how a client that arrives with no credential finds out where to get
// one. Two pieces, and both are required for the flow to start: a metadata
// document describing this resource and naming its authorization server, and a
// WWW-Authenticate header on the 401 pointing at that document. Without the
// header the document is unreachable in practice, because nothing tells the
// client to look for it.
//
// Note where each is served from. The metadata document is the HOST's, mounted
// by [Mount], because well-known URIs are root-scoped and a module behind
// StripPrefix cannot see the prefix it lives under. The challenge is the API's,
// because that is where the 401 is produced -- but the API still has to be told
// the absolute URL to advertise, for the same reason.

// wellKnownProtectedResource is the RFC 9728 well-known segment. The resource's
// path is INSERTED after it, so a resource at /memstore/mcp is described at
// /.well-known/oauth-protected-resource/memstore/mcp. It is not a document
// nested under the resource, which is the intuitive and wrong reading.
const wellKnownProtectedResource = "/.well-known/oauth-protected-resource"

// mcpRoutePath is the API-relative path of the MCP endpoint, as registered in
// registerRoutes. The challenge is scoped to it: the metadata document
// describes the MCP endpoint, and the REST surface has its own callers and its
// own credentials.
const mcpRoutePath = "/mcp"

// ProtectedResource describes this deployment's MCP endpoint as an OAuth 2.1
// protected resource. The zero value means OAuth is not configured, which is
// the current default -- see docs/mcp-oauth-scope.md for why the verifier is
// not enabled in production until webauth can express a scope grant.
type ProtectedResource struct {
	// PublicBaseURL is the scheme and authority this daemon is reached at from
	// outside, with no path -- "https://memstore.example.net". It cannot be
	// derived from a request: the daemon may sit behind a proxy, and a module
	// behind StripPrefix cannot see its own prefix. The host must supply it.
	PublicBaseURL string

	// Prefix is the mount prefix, matching what is passed to Mount. Empty means
	// the daemon is mounted at the root.
	Prefix string

	// AuthorizationServers are the issuer identifiers a client may authenticate
	// against. At least one is required; there is no useful metadata document
	// without one.
	AuthorizationServers []string

	// ScopesSupported is advertised so a client knows what to ask for, in bare
	// form; ScopePrefix is applied on the way out. Ingest is never included:
	// memstore refuses it on an OAuth credential regardless, and advertising it
	// would invite a request that cannot be honoured.
	ScopesSupported []string

	// ScopePrefix namespaces this resource's scopes at the authorization
	// server -- "memstore:" where one server serves several resources, empty
	// where it does not. It must match what the verifier is configured with,
	// or memstore would advertise scopes it then refuses to recognise.
	ScopePrefix string
}

// Configured reports whether OAuth discovery is set up.
func (p ProtectedResource) Configured() bool {
	return p.PublicBaseURL != "" && len(p.AuthorizationServers) > 0
}

// Validate reports whether p can produce a usable metadata document.
func (p ProtectedResource) Validate() error {
	if p.PublicBaseURL == "" {
		return errors.New("httpapi: PublicBaseURL is required")
	}
	u, err := url.Parse(p.PublicBaseURL)
	if err != nil {
		return fmt.Errorf("httpapi: parsing PublicBaseURL: %w", err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("httpapi: PublicBaseURL %q must be absolute, with a scheme and host", p.PublicBaseURL)
	}
	if len(p.AuthorizationServers) == 0 {
		return errors.New("httpapi: at least one authorization server is required")
	}
	return nil
}

// resourcePath is the MCP endpoint's path, prefix included.
func (p ProtectedResource) resourcePath() string {
	prefix := strings.TrimSuffix(p.Prefix, "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix + mcpRoutePath
}

// ResourceURL is the canonical identifier of this protected resource: the
// absolute URL of the MCP endpoint. It is what a token's aud claim must name,
// and what a client sends as the RFC 8707 resource indicator.
func (p ProtectedResource) ResourceURL() string {
	return strings.TrimSuffix(p.PublicBaseURL, "/") + p.resourcePath()
}

// MetadataPath is where the metadata document is served, rooted at the host.
func (p ProtectedResource) MetadataPath() string {
	return wellKnownProtectedResource + p.resourcePath()
}

// MetadataURL is the absolute form of MetadataPath, as advertised in the
// WWW-Authenticate challenge.
func (p ProtectedResource) MetadataURL() string {
	return strings.TrimSuffix(p.PublicBaseURL, "/") + p.MetadataPath()
}

// Metadata builds the RFC 9728 document.
func (p ProtectedResource) Metadata() *oauthex.ProtectedResourceMetadata {
	scopes := make([]string, 0, len(p.ScopesSupported))
	for _, s := range p.ScopesSupported {
		if s == ScopeIngest {
			// Never advertised. The OAuth path strips it on the way in
			// (grantableScopes), so offering it here would only produce a
			// grant memstore refuses to honour.
			continue
		}
		scopes = append(scopes, p.ScopePrefix+s)
	}
	return &oauthex.ProtectedResourceMetadata{
		Resource:               p.ResourceURL(),
		AuthorizationServers:   p.AuthorizationServers,
		ScopesSupported:        scopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "memstore",
	}
}

// challenge is the WWW-Authenticate value for a 401 on the MCP endpoint.
func (p ProtectedResource) challenge() string {
	return fmt.Sprintf("Bearer resource_metadata=%q", p.MetadataURL())
}

// MetadataHandler serves the document, unauthenticated and CORS-open. Both are
// required by RFC 9728 §3.1: this is public configuration data, fetched by
// clients that by definition do not have a credential yet.
func (p ProtectedResource) MetadataHandler() http.Handler {
	return auth.ProtectedResourceMetadataHandler(p.Metadata())
}

// writeUnauthorized answers 401, attaching the RFC 9728 challenge when this
// request is one the metadata document actually describes.
//
// The challenge is scoped to the MCP endpoint deliberately. It advertises a
// document describing /mcp as a protected resource, and the REST surface is
// not that resource: it has its own callers, its own credentials, and whether
// it moves to OAuth at all is still open. Advertising there would point a
// client at a document that does not describe what it just failed to reach.
func (h *Handler) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if h.resource.Configured() && r.URL.Path == mcpRoutePath {
		w.Header().Set("WWW-Authenticate", h.resource.challenge())
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing API key")
}

// MountOpt configures Mount.
type MountOpt func(*mountConfig)

type mountConfig struct {
	resource ProtectedResource
}

// WithProtectedResourceMetadata serves p's RFC 9728 document from the host's
// reserved well-known space. Without it that space stays reserved and empty.
func WithProtectedResourceMetadata(p ProtectedResource) MountOpt {
	return func(c *mountConfig) { c.resource = p }
}
