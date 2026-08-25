package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/infodancer/oidclient"
)

// OAuth resource-server authentication.
//
// memstore validates access tokens it did not mint. The protocol work --
// signature, issuer, audience, expiry, key rotation -- lives in
// github.com/infodancer/oidclient, which serves both roles a service can play
// against a provider; this file is the adapter from a verified token to a
// memstore Identity, and it is where memstore's own rules are applied to
// somebody else's assertion.
//
// See docs/mcp-oauth-scope.md. Note in particular that this path is not
// enabled in production until webauth can express a scope grant and an
// audience (sections B1-B3): until then the authorization server has no way to
// say who may use memstore, and autoprovisioning against it would be open
// enrolment wearing the costume of delegation.

// ErrAuthUnavailable means a caller could not be authenticated because
// something on our side failed -- the authorization server's keys could not be
// fetched, or the user could not be resolved. It is deliberately distinct from
// an invalid token: this is a 503, and a bad token is a 401. Telling a client
// its token is bad when the truth is that the issuer is unreachable sends it
// into a reauthentication loop against a server that is already struggling.
var ErrAuthUnavailable = errors.New("httpapi: authentication temporarily unavailable")

// UserResolver maps a verified token subject to a local memstore user id.
//
// Implementations own the provisioning policy. memstore's decision (decision 5
// of the scope doc) is to autoprovision: admission is the authorization
// server's decision to express, not a roster memstore maintains in parallel
// and has to keep in sync. The subject is the key -- never the email, which can
// change and at some providers can be released and re-registered by a
// different person, which would make account takeover a matter of waiting for
// an address to be recycled. Email is passed for storage as a display
// attribute only.
//
// The whole token is passed rather than a subject and an email, because a
// provisioning policy also records email_verified and the display name --
// narrowing the argument list here would silently discard them and pin
// email_verified to false on every row.
type UserResolver interface {
	ResolveUser(ctx context.Context, tok *oidclient.AccessToken) (int64, error)
}

// accessTokenVerifier is the part of *oidclient.ResourceServer this package
// uses. Naming it keeps the mapping rules testable without an authorization
// server, while the end-to-end test exercises the real implementation.
type accessTokenVerifier interface {
	Verify(ctx context.Context, token string) (*oidclient.AccessToken, error)
}

// OAuthVerifier authenticates bearer tokens issued by the configured
// authorization server. It implements TokenVerifier, so it plugs into the same
// seam as the static-token store and every handler and scope check downstream
// is unchanged.
type OAuthVerifier struct {
	tokens accessTokenVerifier
	users  UserResolver
}

// NewOAuthVerifier returns a verifier backed by rs, resolving users through
// users. Both are required.
func NewOAuthVerifier(rs *oidclient.ResourceServer, users UserResolver) (*OAuthVerifier, error) {
	if rs == nil {
		return nil, errors.New("httpapi: a resource server is required")
	}
	if users == nil {
		return nil, errors.New("httpapi: a user resolver is required")
	}
	return newOAuthVerifier(rs, users), nil
}

func newOAuthVerifier(tokens accessTokenVerifier, users UserResolver) *OAuthVerifier {
	return &OAuthVerifier{tokens: tokens, users: users}
}

// VerifyToken implements TokenVerifier.
func (v *OAuthVerifier) VerifyToken(ctx context.Context, token string) (Identity, error) {
	tok, err := v.tokens.Verify(ctx, token)
	if err != nil {
		// An unreachable key set is our outage, not the caller's bad token.
		if errors.Is(err, oidclient.ErrKeysUnavailable) {
			return Identity{}, fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
		}
		return Identity{}, err
	}

	userID, err := v.users.ResolveUser(ctx, tok)
	if err != nil {
		// The token was good; we could not establish whose it is.
		return Identity{}, fmt.Errorf("%w: resolving user: %w", ErrAuthUnavailable, err)
	}
	if userID == 0 {
		// User 0 is the service-scope sentinel elsewhere in the store layer and
		// would silently drop this caller into the default user's namespace --
		// every OAuth caller sharing one memory. A resolver that cannot name a
		// user must fail rather than return it.
		return Identity{}, fmt.Errorf("%w: resolver returned no user for subject %q",
			ErrAuthUnavailable, tok.Subject)
	}

	name := tok.Email
	if name == "" {
		name = tok.Subject
	}

	return Identity{
		Name:   name,
		Scopes: grantableScopes(tok.Scopes),
		Source: "oauth",
		UserID: userID,
	}, nil
}

// grantableScopes filters what the authorization server granted down to what
// memstore will honour on an OAuth credential.
//
// Ingest is removed unconditionally. "Ingest is implied by nothing, including
// admin" is the structural guarantee behind the document corpus: provenance is
// trustworthy because no credential the model holds can reach the ingest path.
// Enforcing that here rather than trusting the authorization server's scope
// policy keeps the guarantee memstore's own, and survives a tenant being
// reconfigured by someone who has never read this file.
//
// The empty result is returned as a non-nil empty slice rather than nil so it
// stays distinguishable in tests; Identity.Allows treats both the same, which
// is why the ingest-only case is called out there -- an ingest-only token must
// not be promoted into the legacy read+write grant by having its one scope
// stripped.
func grantableScopes(granted []string) []string {
	out := make([]string, 0, len(granted))
	for _, s := range granted {
		if s == ScopeIngest {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 && len(granted) > 0 {
		// Everything the token carried was filtered away. Returning an empty
		// set here would hit the legacy "no scopes means read+write" rule and
		// promote the caller. Deny instead, explicitly.
		return []string{scopeNone}
	}
	return out
}

// scopeNone is a scope no route requires and no implication grants. It marks an
// identity whose entire grant was filtered away, so Identity.Allows sees a
// non-empty scope set and refuses everything rather than applying the legacy
// grant.
const scopeNone = "none"

// Compile-time check that the adapter satisfies the seam it plugs into.
var _ TokenVerifier = (*OAuthVerifier)(nil)

// Ensure the concrete resource server satisfies the narrowed interface.
var _ accessTokenVerifier = (*oidclient.ResourceServer)(nil)
