package httpapi

import (
	"net/http"
	"slices"

	"github.com/matthewjhunter/memstore"
)

// Scopes carried by API tokens, re-exported from the root package so handlers
// and route registration read naturally. The canonical definitions live in
// memstore.Scope* because pgstore validates against the same set at issuance
// and must not import this package.
//
// Every route declares the scope it requires at registration; see
// registerRoutes. What each implies is documented on Allows.
const (
	ScopeRead   = memstore.ScopeRead
	ScopeWrite  = memstore.ScopeWrite
	ScopeAdmin  = memstore.ScopeAdmin
	ScopeIngest = memstore.ScopeIngest
)

// Allows reports whether the identity may exercise the given scope.
//
// Two implications exist, and one deliberate non-implication:
//
//   - ScopeAdmin implies ScopeRead and ScopeWrite. An administrator can already
//     do both by other means; requiring the grant to be spelled out adds
//     friction without adding a boundary.
//
//   - An identity with NO scopes gets ScopeRead and ScopeWrite. Tokens issued
//     before scope enforcement existed carry an empty scope set, as does the
//     legacy single-key auth path, and revoking their access on upgrade would
//     break running deployments for no security gain -- they already had it.
//
//   - ScopeIngest is implied by NOTHING. Not by admin, not by the legacy grant.
//     It must be granted explicitly on the token.
//
// That last rule is the one worth defending. The document corpus derives its
// trustworthiness from provenance the model cannot author, and the structural
// guarantee behind that is simply that no credential the model holds can reach
// the ingest path. In practice the MCP server's token is often an admin token,
// so if admin implied ingest the guarantee would be void exactly where it
// matters. Admin is authority over the store; ingest is the authority to assert
// where bytes came from. They are different powers and this keeps them apart.
func (id Identity) Allows(scope string) bool {
	if slices.Contains(id.Scopes, scope) {
		return true
	}
	// Ingest is never implied.
	if scope == ScopeIngest {
		return false
	}
	if scope == ScopeRead || scope == ScopeWrite {
		if len(id.Scopes) == 0 || slices.Contains(id.Scopes, ScopeAdmin) {
			return true
		}
	}
	return false
}

// requireScope wraps a handler so it only runs when the caller's identity
// allows the given scope.
//
// When no identity is present the request passes through: that is the
// unauthenticated configuration (neither a token verifier nor an API key is
// wired up), which is a deployment choice, not a caller-controlled one. If auth
// is configured, ServeHTTP has already rejected anything without a valid
// credential before this runs.
func (h *Handler) requireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFromContext(r.Context())
		if ok && !id.Allows(scope) {
			writeError(w, http.StatusForbidden, "token lacks the "+scope+" scope")
			return
		}
		next(w, r)
	}
}
