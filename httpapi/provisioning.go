package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/rpuser"
)

// ProvisioningResolver resolves an OAuth caller to a local memstore user,
// creating one on first sight of a subject.
//
// It is a thin adapter: the policy -- key on (issuer, subject), never match by
// email, sync mutable claims onto the existing row -- lives in
// github.com/infodancer/oidclient/rpuser, shared with the websites, and the SQL
// lives in pgstore. What is here is the conversion from rpuser's string key to
// the int64 the store layer scopes by.
//
// This is memstore's implementation of decision 5 in docs/mcp-oauth-scope.md:
// admission is the authorization server's decision to express, so a validated
// token for an unknown subject provisions rather than being refused. That
// delegation is only real once webauth can express the grant (sections B1-B3),
// which is why the OAuth path is not enabled in production before then.
type ProvisioningResolver struct {
	prov *rpuser.Provisioner
}

// NewProvisioningResolver returns a resolver over store, provisioning users for
// tokens issued by issuer. The issuer is the configured one the resource server
// validates against, not a value read out of a token.
func NewProvisioningResolver(store rpuser.UserStore, issuer string, logf func(string, ...any)) (*ProvisioningResolver, error) {
	if store == nil {
		return nil, errors.New("httpapi: a user store is required")
	}
	if issuer == "" {
		return nil, errors.New("httpapi: an issuer is required")
	}
	return &ProvisioningResolver{prov: &rpuser.Provisioner{
		Store:  store,
		Issuer: issuer,
		Logf:   logf,
	}}, nil
}

var _ UserResolver = (*ProvisioningResolver)(nil)

// ResolveUser implements UserResolver.
func (r *ProvisioningResolver) ResolveUser(ctx context.Context, tok *oidclient.AccessToken) (int64, error) {
	u, _, err := r.prov.ProvisionAccessToken(ctx, tok)
	if err != nil {
		return 0, err
	}
	// The store puts the numeric id in Host, which is rpuser's documented slot
	// for the host's own representation. Preferring it over re-parsing Key
	// keeps the string form an implementation detail of the store.
	id, ok := u.Host.(int64)
	if !ok {
		return 0, fmt.Errorf("httpapi: user store returned no numeric id for subject %q", tok.Subject)
	}
	return id, nil
}
