package memstore

import (
	"fmt"
	"slices"
	"strings"
)

// Token scopes. These name the authority a credential carries, and are the
// canonical spelling for both ends: pgstore stores them on api_tokens rows and
// validates them at issuance, httpapi enforces them per route. They live here
// because the root package is the one both of those already depend on.
//
// What each scope implies -- and, for ingest, what it deliberately does not --
// is documented on httpapi.Identity.Allows, which is where the decision is made.
const (
	// ScopeRead permits reads: fetching, listing, searching, recall.
	ScopeRead = "read"
	// ScopeWrite permits mutating facts, links, sessions, and context.
	ScopeWrite = "write"
	// ScopeAdmin permits administrative operations, and implies read and write.
	ScopeAdmin = "admin"
	// ScopeIngest permits writing to the document corpus. It is implied by no
	// other scope, including admin, and must always be granted explicitly.
	ScopeIngest = "ingest"
)

// ValidScopes returns every recognised scope, in a stable order suitable for
// error messages and help text.
func ValidScopes() []string {
	return []string{ScopeRead, ScopeWrite, ScopeAdmin, ScopeIngest}
}

// ValidateScopes reports whether every scope in the set is recognised. An empty
// set is valid: it is how an unscoped token is represented, and such a token
// receives the legacy read+write grant.
//
// This exists because an unrecognised scope fails closed and silently. Scope
// matching is exact and case-sensitive, so "Ingest" or a typo like "ingset"
// produces a credential that authenticates successfully and is refused by every
// route, with nothing at issue time to say why. Worse, a non-empty scope set
// forfeits the empty-set grant, so a misspelled scope is strictly worse than
// passing no scopes at all. Rejecting it at the point of issue is the only
// place the mistake is still cheap.
func ValidateScopes(scopes []string) error {
	valid := ValidScopes()
	for _, s := range scopes {
		if !slices.Contains(valid, s) {
			return fmt.Errorf("unknown scope %q: valid scopes are %s (exact, lowercase)",
				s, strings.Join(valid, ", "))
		}
	}
	return nil
}
