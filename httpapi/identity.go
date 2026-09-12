package httpapi

import (
	"context"
	"net/http"
	"slices"
	"sync"
)

// Identity is the resolved caller for an authenticated request. It is set by
// the auth middleware (bearer-token verification, mTLS handshake, or any
// future source) and read by handlers that need to know who is calling.
type Identity struct {
	// Name is the human-readable identity, e.g. "matthew-laptop" for a bearer
	// token row or the CN of an mTLS client cert.
	Name string

	// Scopes are coarse-grained permission strings attached to the identity,
	// e.g. "read", "write", "admin". Consumed by Allows, which every route
	// consults through requireScope; GET /v1/whoami reports the effective set
	// so callers need not reimplement the implication rules.
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
// to propagate the resolved caller to handlers. If the context carries an
// identity sink (see WithIdentitySink), the name is recorded there too.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	if sink, ok := ctx.Value(identitySinkKey{}).(*identitySink); ok {
		sink.set(id.Name)
	}
	return context.WithValue(ctx, identityCtxKey{}, id)
}

type identitySinkKey struct{}

// identitySink is the slot an outer middleware leaves on the request context
// for the auth layer to fill in.
type identitySink struct {
	mu   sync.Mutex
	name string
}

func (s *identitySink) set(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

func (s *identitySink) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// WithIdentitySink returns a context carrying a slot for the authenticated
// name, and a function reading it. It exists for middleware that wraps the
// handler from outside -- the access log -- which cannot otherwise learn who
// the caller was: WithIdentity returns a derived context that only the inner
// chain holds, and a request's own context is immutable from out there.
//
// The returned function is safe to call while the handler is still running,
// and reports the empty string until the request authenticates (or forever, if
// it never does).
func WithIdentitySink(ctx context.Context) (context.Context, func() string) {
	sink := &identitySink{}
	return context.WithValue(ctx, identitySinkKey{}, sink), sink.get
}

// SinkIdentityName returns the authenticated name recorded on ctx's identity
// sink, or the empty string if the request carries no sink or never
// authenticated. It is the context-keyed form of the accessor
// WithIdentitySink returns, for callers handed only a context -- an access log
// asked for the identity of the request it just served, say.
func SinkIdentityName(ctx context.Context) string {
	sink, ok := ctx.Value(identitySinkKey{}).(*identitySink)
	if !ok {
		return ""
	}
	return sink.get()
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
