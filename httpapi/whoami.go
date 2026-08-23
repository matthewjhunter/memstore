package httpapi

import (
	"net/http"

	"github.com/matthewjhunter/memstore"
)

// WhoAmIResponse is the wire shape of GET /v1/whoami. It is an alias for the
// root-package type so httpclient can decode it without importing this
// package (and with it the whole server) into every client binary.
type WhoAmIResponse = memstore.WhoAmIResponse

// handleWhoAmI reports the caller's effective permissions.
//
// Registered without requireScope, deliberately. Asking what you may do is not
// itself a privileged act, and gating it on read would leave an ingest-only
// token -- the one credential that holds no read grant -- unable to discover
// its own capabilities. ServeHTTP has already rejected an invalid credential
// before this runs, so reaching here means the caller is either authenticated
// or the deployment is unauthenticated.
func (h *Handler) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	id, authenticated := IdentityFromContext(r.Context())

	out := WhoAmIResponse{
		Name:          id.Name,
		Source:        id.Source,
		Authenticated: authenticated,
		Scopes:        id.Scopes,
		Allows:        make([]string, 0, len(ValidScopes())),
	}
	if out.Scopes == nil {
		out.Scopes = []string{}
	}

	for _, scope := range ValidScopes() {
		// With no identity the deployment is unauthenticated and requireScope
		// passes every route through, so every scope is genuinely available.
		if !authenticated || id.Allows(scope) {
			out.Allows = append(out.Allows, scope)
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// ValidScopes re-exports the canonical scope list so whoami and its tests read
// against the same source as issuance.
func ValidScopes() []string { return memstore.ValidScopes() }
