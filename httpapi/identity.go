package httpapi

import (
	"context"
	"net/http"
	"slices"
)

// Identity is the resolved caller for an authenticated request. It is set by
// the auth middleware (bearer-token verification, mTLS handshake, or any
// future source) and read by handlers that need to know who is calling.
type Identity struct {
	// Name is the human-readable identity, e.g. "matthew-laptop" for a bearer
	// token row or the CN of an mTLS client cert.
	Name string

	// Scopes are coarse-grained permission strings attached to the identity,
	// e.g. "read", "write", "admin". The current handlers don't consume them
	// yet; the field is here so the multi-tenant work has a place to land.
	Scopes []string

	// Source records how the identity was established: "bearer", "mtls", or
	// "legacy" for the single-key MEMSTORE_API_KEY fallback. Useful for audit
	// and for handlers that only trust one source.
	Source string

	// UserID is the database ID of the owning user row. Set from the token
	// store's VerifyResult.UserID on the bearer-token path; 0 on the legacy
	// single-key path (maps to the default user via the store fallback).
	UserID int64
}

// HasScope reports whether the identity carries the given scope.
func (id Identity) HasScope(scope string) bool {
	return slices.Contains(id.Scopes, scope)
}

type identityCtxKey struct{}

// WithIdentity returns a context that carries id. Auth middleware uses this
// to propagate the resolved caller to handlers.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the Identity stored on ctx, or zero+false if
// the request was unauthenticated (or the middleware didn't run).
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}

// PeerIdentity returns the Identity established by the TLS layer (mTLS
// client cert), or zero+false if the request did not present a verified
// client cert. Independent of bearer-token auth: a request can carry both,
// and handlers may consult either.
func PeerIdentity(r *http.Request) (Identity, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Identity{}, false
	}
	return Identity{
		Name:   r.TLS.PeerCertificates[0].Subject.CommonName,
		Source: "mtls",
	}, true
}
