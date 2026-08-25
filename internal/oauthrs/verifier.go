// Package oauthrs verifies OAuth 2.1 access tokens for memstore acting as a
// resource server.
//
// It validates; it never obtains. There is no authorization-code flow here, no
// client secret, and no call to an upstream identity provider: a token arrives,
// its signature is checked against the authorization server's published keys,
// and its claims are checked against what this resource is. The client runs the
// flow. See docs/mcp-oauth-scope.md for the roles and why they are split this
// way.
//
// Everything this package rejects, it rejects with ErrInvalidToken. The one
// exception is deliberate and load-bearing: an authorization server we cannot
// reach yields an error that does NOT match ErrInvalidToken, because "we could
// not check this" is a 500 and "this token is bad" is a 401, and answering an
// outage with a 401 sends every client into a reauthentication loop against a
// server that is already struggling.
package oauthrs

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// ErrInvalidToken is the sentinel for every "this token does not authenticate"
// outcome. Callers map it to 401; anything else is a failure to check.
var ErrInvalidToken = errors.New("oauthrs: invalid token")

// defaultMinRefresh bounds how often an unknown key id may force an outbound
// fetch. Rotation is rare and unauthenticated callers choose the key id, so the
// window trades a few minutes of staleness after a rotation for not handing
// anyone an amplifier aimed at the authorization server.
const defaultMinRefresh = 5 * time.Minute

// maxJWKSBytes caps what we will read from the JWKS endpoint. A key set is a
// few kilobytes; anything approaching this is a compromised or malfunctioning
// server, and we should not exhaust memory parsing it.
const maxJWKSBytes = 1 << 20

// Config describes the one authorization server this verifier trusts and the
// one resource identifier it will accept tokens for. All three fields are
// required: a verifier missing any of them cannot perform a check it is being
// asked to perform, and defaulting one would mean silently not checking it.
type Config struct {
	// Issuer is the expected iss claim, compared exactly.
	Issuer string

	// Resource is this resource server's identifier -- for memstore, the
	// canonical MCP endpoint URL. A token must name it in aud.
	Resource string

	// JWKSURL is where the authorization server publishes its signing keys.
	JWKSURL string

	// HTTPClient fetches the key set. Defaults to a client with a short
	// timeout; a fetch is on the request path and must not hang there.
	HTTPClient *http.Client

	// MinRefresh is the floor between key-set refetches triggered by an
	// unrecognised key id. Defaults to defaultMinRefresh.
	MinRefresh time.Duration
}

// Claims is what a verified token asserts, reduced to the parts memstore acts
// on. It is deliberately not the raw claim set: a caller that needs something
// else should have it added here, so every consumed claim is one this package
// validated.
type Claims struct {
	// Subject is the authorization server's stable identifier for the user.
	// It is the key memstore provisions and looks up users by -- never the
	// email address, which can change and at some providers can be released
	// and re-registered by a different person.
	Subject string

	// Email is a display attribute. Never a lookup key.
	Email string

	// Scopes is the scope claim, already split. Whether a scope means anything
	// is the caller's decision; this package only reports what was granted.
	Scopes []string

	// Expiry is the validated expiration, for callers that surface it.
	Expiry time.Time
}

// Verifier checks tokens against one authorization server. It is safe for
// concurrent use, and caches the key set across requests.
type Verifier struct {
	cfg    Config
	client *http.Client
	parser *jwt.Parser

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	loaded      bool
	lastRefresh time.Time // last refetch forced by an unknown kid, not the initial load
}

// New returns a Verifier for cfg, or an error if cfg is incomplete.
func New(cfg Config) (*Verifier, error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("oauthrs: Issuer is required")
	case cfg.Resource == "":
		return nil, errors.New("oauthrs: Resource is required")
	case cfg.JWKSURL == "":
		return nil, errors.New("oauthrs: JWKSURL is required")
	}
	if cfg.MinRefresh == 0 {
		cfg.MinRefresh = defaultMinRefresh
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Verifier{
		cfg:    cfg,
		client: client,
		// The algorithm is policy, not something the token gets to choose.
		// Pinning it here is what defeats algorithm confusion: an HS256 token
		// signed with the RSA public key as its secret never reaches the
		// key function, and neither does alg "none".
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{"RS256"}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithExpirationRequired(),
		),
		keys: map[string]*rsa.PublicKey{},
	}, nil
}

// tokenClaims is the wire shape. Only fields this package validates or returns
// appear here.
type tokenClaims struct {
	jwt.RegisteredClaims
	Email string `json:"email,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// Verify checks token and returns what it asserts.
//
// A returned error matching ErrInvalidToken means the token does not
// authenticate. Any other error means the check could not be completed.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	// The key function cannot return a typed error through the parser without
	// relying on how it wraps, so a lookup failure is captured here and
	// consulted first. Signature verification and "we could not fetch the
	// keys" must not collapse into the same outcome.
	var keyErr error

	var claims tokenClaims
	_, err := v.parser.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("%w: no kid in token header", ErrInvalidToken)
		}
		key, err := v.publicKey(ctx, kid)
		if err != nil {
			keyErr = err
			return nil, err
		}
		return key, nil
	})
	if keyErr != nil {
		return nil, keyErr
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	// Audience is checked here rather than through the parser because the
	// comparison is not string equality: RFC 9728 canonicalises the resource
	// identifier with a trailing slash, RFC 8707 resource indicators routinely
	// omit it, and issuers echo back whichever they were given.
	if !oauthex.MatchesResource(claims.Audience, v.cfg.Resource) {
		return nil, fmt.Errorf("%w: audience %v does not name %s",
			ErrInvalidToken, []string(claims.Audience), v.cfg.Resource)
	}

	// A token with no subject names nobody. Rejecting it here keeps callers
	// from provisioning or attributing against an empty identifier.
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}

	out := &Claims{
		Subject: claims.Subject,
		Email:   claims.Email,
		Scopes:  strings.Fields(claims.Scope),
	}
	if claims.ExpiresAt != nil {
		out.Expiry = claims.ExpiresAt.Time
	}
	return out, nil
}

// publicKey resolves kid, loading the key set on first use and refetching once
// per MinRefresh window when kid is unrecognised.
func (v *Verifier) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !v.loaded {
		if err := v.fetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}

	// Unknown kid. Either the server rotated its keys, or the caller made the
	// kid up. Refetching serves the first case; the throttle bounds the second.
	if time.Since(v.lastRefresh) < v.cfg.MinRefresh {
		return nil, fmt.Errorf("%w: unknown key id %q", ErrInvalidToken, kid)
	}
	v.lastRefresh = time.Now()
	if err := v.fetchLocked(ctx); err != nil {
		return nil, err
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown key id %q", ErrInvalidToken, kid)
}

// jwksDoc and jwkKey are the subset of RFC 7517 we consume. Keys that are not
// RSA signing keys are skipped rather than rejected: a key set may legitimately
// carry encryption keys or algorithms we do not accept, and one of those
// appearing is not a reason to fail every verification.
type jwksDoc struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// fetchLocked replaces the cached key set. The caller holds v.mu.
func (v *Verifier) fetchLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return fmt.Errorf("oauthrs: building JWKS request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("oauthrs: fetching JWKS: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauthrs: fetching JWKS: status %d", resp.StatusCode)
	}

	var doc jwksDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return fmt.Errorf("oauthrs: decoding JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.Alg != "" && k.Alg != "RS256" {
			continue
		}
		pub, err := k.rsaPublicKey()
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("oauthrs: JWKS contained no usable RSA signing keys")
	}

	v.keys = keys
	v.loaded = true
	return nil
}

// rsaPublicKey rebuilds the key from its modulus and exponent. Both are
// base64url without padding per RFC 7517.
func (k jwkKey) rsaPublicKey() (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decoding modulus: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decoding exponent: %w", err)
	}
	if len(nb) == 0 || len(eb) == 0 {
		return nil, errors.New("empty modulus or exponent")
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() <= 0 || e.Int64() > 1<<31-1 {
		return nil, errors.New("exponent out of range")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
}
