package oauthrs

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://webauth.example.test/t/memstore"
	testResource = "https://memstore.example.test/memstore/mcp"
)

// jwksServer serves a JWKS document for the given keys and counts how many
// times it was fetched, so the refresh-throttle test can assert on it.
type jwksServer struct {
	*httptest.Server
	fetches atomic.Int64
}

func newJWKSServer(t *testing.T, keys map[string]*rsa.PublicKey) *jwksServer {
	t.Helper()
	s := &jwksServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fetches.Add(1)
		type jwk struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		}
		doc := struct {
			Keys []jwk `json:"keys"`
		}{}
		for kid, pub := range keys {
			doc.Keys = append(doc.Keys, jwk{
				Kty: "RSA", Kid: kid, Alg: "RS256", Use: "sig",
				N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc) //nolint:errcheck // test server
	}))
	t.Cleanup(s.Close)
	return s
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

// claims is the mutable shape the tests build tokens from. Each test starts
// from validClaims and breaks exactly one thing, so a failure names the field.
type claims struct {
	jwt.RegisteredClaims
	Email string `json:"email,omitempty"`
	Scope string `json:"scope,omitempty"`
}

func validClaims() claims {
	now := time.Now()
	return claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   "user-abc123",
			Audience:  jwt.ClaimStrings{testResource},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Email: "someone@example.test",
		Scope: "read write",
	}
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, c claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func newVerifier(t *testing.T, jwksURL string) *Verifier {
	t.Helper()
	v, err := New(Config{
		Issuer:   testIssuer,
		Resource: testResource,
		JWKSURL:  jwksURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestVerifyAcceptsAValidToken(t *testing.T) {
	key := newKey(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := newVerifier(t, srv.URL)

	got, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "user-abc123" {
		t.Errorf("Subject = %q, want user-abc123", got.Subject)
	}
	if got.Email != "someone@example.test" {
		t.Errorf("Email = %q, want someone@example.test", got.Email)
	}
	// The scope claim is a space-delimited string per RFC 6749; it reaches the
	// caller already split, because every consumer wants the list.
	if len(got.Scopes) != 2 || got.Scopes[0] != "read" || got.Scopes[1] != "write" {
		t.Errorf("Scopes = %v, want [read write]", got.Scopes)
	}
}

// The rejection table. Every case here is a token that must not authenticate,
// and each one is a distinct way an attacker or a misconfiguration produces a
// token that would otherwise parse cleanly.
func TestVerifyRejects(t *testing.T) {
	key := newKey(t)
	otherKey := newKey(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})

	tests := []struct {
		name  string
		token func(t *testing.T) string
	}{
		{
			// The confused-deputy case, and the reason audience binding is
			// mandatory: a token webauth minted for another application on the
			// same tenant must not open memstore.
			name: "audience naming a different resource",
			token: func(t *testing.T) string {
				c := validClaims()
				c.Audience = jwt.ClaimStrings{"https://herald.example.test/mcp"}
				return sign(t, key, "k1", c)
			},
		},
		{
			// An unaudienced token is every resource's token. webauth emits
			// exactly this shape today, which is why B2 gates deployment.
			name: "no audience at all",
			token: func(t *testing.T) string {
				c := validClaims()
				c.Audience = nil
				return sign(t, key, "k1", c)
			},
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				c := validClaims()
				c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
				return sign(t, key, "k1", c)
			},
		},
		{
			// A token with no expiry never stops being valid. Absence must be
			// a rejection rather than "nothing to check, therefore fine".
			name: "no expiry claim",
			token: func(t *testing.T) string {
				c := validClaims()
				c.ExpiresAt = nil
				return sign(t, key, "k1", c)
			},
		},
		{
			name: "issued by a different authorization server",
			token: func(t *testing.T) string {
				c := validClaims()
				c.Issuer = "https://evil.example.test/t/memstore"
				return sign(t, key, "k1", c)
			},
		},
		{
			name: "signed by a key that is not in the JWKS",
			token: func(t *testing.T) string {
				return sign(t, otherKey, "k1", validClaims())
			},
		},
		{
			name: "kid naming a key the JWKS does not have",
			token: func(t *testing.T) string {
				return sign(t, key, "nonexistent-kid", validClaims())
			},
		},
		{
			// Algorithm confusion: sign with HMAC using the RSA public key as
			// the shared secret. A verifier that picks its algorithm from the
			// token header rather than from policy accepts this.
			name: "HMAC-signed using the public key as the secret",
			token: func(t *testing.T) string {
				pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
				if err != nil {
					t.Fatalf("marshal public key: %v", err)
				}
				tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
				tok.Header["kid"] = "k1"
				s, err := tok.SignedString(pub)
				if err != nil {
					t.Fatalf("sign HS256: %v", err)
				}
				return s
			},
		},
		{
			name: "alg none",
			token: func(t *testing.T) string {
				tok := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims())
				tok.Header["kid"] = "k1"
				s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
				if err != nil {
					t.Fatalf("sign none: %v", err)
				}
				return s
			},
		},
		{
			// Without a subject there is nobody to provision or attribute the
			// call to, and autoprovisioning would key a row on "".
			name: "no subject",
			token: func(t *testing.T) string {
				c := validClaims()
				c.Subject = ""
				return sign(t, key, "k1", c)
			},
		},
		{
			name:  "not a JWT at all",
			token: func(t *testing.T) string { return "not.a.token" },
		},
		{
			name:  "empty string",
			token: func(t *testing.T) string { return "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newVerifier(t, srv.URL)
			got, err := v.Verify(context.Background(), tt.token(t))
			if err == nil {
				t.Fatalf("Verify accepted the token, claims = %+v", got)
			}
			// Callers distinguish "this token is bad" (401) from "we could not
			// check" (500), so the sentinel has to survive the wrapping.
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error %v does not match ErrInvalidToken", err)
			}
		})
	}
}

// The rejection table above proves an HS256 or "none" token is refused, but not
// WHY: golang-jwt would refuse them anyway because the key function hands back
// an *rsa.PublicKey, which is not usable as an HMAC secret. That is an accident
// of the key type, not a policy, and it would evaporate if the key function
// ever returned something more permissive.
//
// This pins the policy instead. A token whose algorithm we do not accept must
// be rejected during parsing, before the key function runs at all -- observable
// here because reaching the key function is what triggers the first JWKS fetch.
// Deleting jwt.WithValidMethods makes this test fail; deleting it makes nothing
// else fail.
func TestVerifyRejectsBadAlgorithmBeforeConsultingKeys(t *testing.T) {
	key := newKey(t)

	tests := []struct {
		name  string
		token func(t *testing.T) string
	}{
		{
			name: "HS256",
			token: func(t *testing.T) string {
				pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
				if err != nil {
					t.Fatalf("marshal public key: %v", err)
				}
				tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
				tok.Header["kid"] = "k1"
				s, err := tok.SignedString(pub)
				if err != nil {
					t.Fatalf("sign HS256: %v", err)
				}
				return s
			},
		},
		{
			name: "none",
			token: func(t *testing.T) string {
				tok := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims())
				tok.Header["kid"] = "k1"
				s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
				if err != nil {
					t.Fatalf("sign none: %v", err)
				}
				return s
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh server and verifier per case, so the key set is still
			// unfetched when the token arrives.
			srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
			v := newVerifier(t, srv.URL)

			if _, err := v.Verify(context.Background(), tt.token(t)); err == nil {
				t.Fatal("Verify accepted the token")
			}
			if got := srv.fetches.Load(); got != 0 {
				t.Errorf("key set was fetched %d times; the algorithm should have "+
					"been rejected before the key function ran", got)
			}
		})
	}
}

// RFC 9728 canonicalises the resource identifier with a trailing slash while
// RFC 8707 resource indicators often omit it, and issuers differ in which form
// they echo into aud. Both forms name the same resource.
func TestVerifyAcceptsTrailingSlashVariance(t *testing.T) {
	key := newKey(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := newVerifier(t, srv.URL)

	c := validClaims()
	c.Audience = jwt.ClaimStrings{testResource + "/"}
	if _, err := v.Verify(context.Background(), sign(t, key, "k1", c)); err != nil {
		t.Fatalf("Verify rejected a trailing-slash audience: %v", err)
	}
}

// A token may carry several audiences; ours being among them is enough.
func TestVerifyAcceptsMultipleAudiencesIncludingOurs(t *testing.T) {
	key := newKey(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := newVerifier(t, srv.URL)

	c := validClaims()
	c.Audience = jwt.ClaimStrings{"https://herald.example.test/mcp", testResource}
	if _, err := v.Verify(context.Background(), sign(t, key, "k1", c)); err != nil {
		t.Fatalf("Verify rejected a token listing our resource: %v", err)
	}
}

// Key rotation must not need a restart: a kid the cache has never seen forces
// one refetch, and the token signed by the new key then verifies.
func TestVerifyRefetchesJWKSOnUnknownKid(t *testing.T) {
	oldKey, newKeyPair := newKey(t), newKey(t)
	keys := map[string]*rsa.PublicKey{"k1": &oldKey.PublicKey}
	srv := newJWKSServer(t, keys)
	v := newVerifier(t, srv.URL)

	if _, err := v.Verify(context.Background(), sign(t, oldKey, "k1", validClaims())); err != nil {
		t.Fatalf("Verify with the original key: %v", err)
	}

	// webauth rotates: a second key appears under a new kid.
	keys["k2"] = &newKeyPair.PublicKey
	if _, err := v.Verify(context.Background(), sign(t, newKeyPair, "k2", validClaims())); err != nil {
		t.Fatalf("Verify after rotation: %v", err)
	}
}

// The refetch above is an unauthenticated caller's way of making us issue an
// outbound request, so it has to be throttled: a flood of unknown kids must not
// become a flood of JWKS fetches against webauth.
func TestVerifyThrottlesRefetch(t *testing.T) {
	key := newKey(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v, err := New(Config{
		Issuer:     testIssuer,
		Resource:   testResource,
		JWKSURL:    srv.URL,
		MinRefresh: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Prime the cache, then hammer it with unknown kids.
	if _, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims())); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	primed := srv.fetches.Load()
	for range 20 {
		_, _ = v.Verify(context.Background(), sign(t, key, "unknown", validClaims()))
	}
	if got := srv.fetches.Load() - primed; got > 1 {
		t.Errorf("20 unknown-kid tokens caused %d JWKS fetches, want at most 1", got)
	}
}

// A verifier that cannot reach the JWKS must report that as a failure to check
// rather than as a bad token: the difference is a 500 and a 401, and treating
// an outage as "your token is invalid" sends clients into a reauth loop.
func TestVerifyDistinguishesUnreachableJWKS(t *testing.T) {
	key := newKey(t)
	srv := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	url := srv.URL
	srv.Close()

	v := newVerifier(t, url)
	_, err := v.Verify(context.Background(), sign(t, key, "k1", validClaims()))
	if err == nil {
		t.Fatal("Verify succeeded against an unreachable JWKS")
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Errorf("unreachable JWKS reported as an invalid token: %v", err)
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no issuer", Config{Resource: testResource, JWKSURL: "https://x.test/jwks"}},
		{"no resource", Config{Issuer: testIssuer, JWKSURL: "https://x.test/jwks"}},
		{"no JWKS URL", Config{Issuer: testIssuer, Resource: testResource}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Error("New accepted an incomplete config")
			}
		})
	}
}
