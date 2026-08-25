package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/infodancer/oidclient"
)

// --- fakes for the mapping tests -------------------------------------------

type fakeTokenVerifier struct {
	tok *oidclient.AccessToken
	err error
}

func (f fakeTokenVerifier) Verify(context.Context, string) (*oidclient.AccessToken, error) {
	return f.tok, f.err
}

type fakeResolver struct {
	id          int64
	err         error
	got         *oidclient.AccessToken
	resolutions int
}

func (f *fakeResolver) ResolveUser(_ context.Context, tok *oidclient.AccessToken) (int64, error) {
	f.resolutions++
	f.got = tok
	return f.id, f.err
}

func TestOAuthVerifierMapsAVerifiedToken(t *testing.T) {
	res := &fakeResolver{id: 42}
	v := newOAuthVerifier(fakeTokenVerifier{tok: &oidclient.AccessToken{
		Subject:       "sub-abc",
		Email:         "someone@example.test",
		EmailVerified: true,
		Scopes:        []string{ScopeRead, ScopeWrite},
		Expiry:        time.Now().Add(time.Hour),
	}}, res)

	id, err := v.VerifyToken(context.Background(), "token")
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.UserID != 42 {
		t.Errorf("UserID = %d, want 42", id.UserID)
	}
	if id.Source != "oauth" {
		t.Errorf("Source = %q, want oauth", id.Source)
	}
	if !slices.Equal(id.Scopes, []string{ScopeRead, ScopeWrite}) {
		t.Errorf("Scopes = %v, want [read write]", id.Scopes)
	}
	// The whole verified token reaches the resolver. Passing only a subject
	// and an email would discard email_verified, which the provisioning policy
	// records -- and would pin it to false on every row it ever wrote.
	if res.got == nil {
		t.Fatal("resolver received no token")
	}
	if res.got.Subject != "sub-abc" {
		t.Errorf("resolver got subject %q, want sub-abc", res.got.Subject)
	}
	if res.got.Email != "someone@example.test" {
		t.Errorf("resolver got email %q, want someone@example.test", res.got.Email)
	}
	if !res.got.EmailVerified {
		t.Error("resolver did not receive email_verified")
	}
}

// "Ingest is implied by nothing" is the guarantee the document corpus rests on.
// Enforcing it here rather than trusting the authorization server not to grant
// it keeps the guarantee memstore's own: a misconfigured tenant, or one whose
// scope policy changes later, must not be able to hand the model a credential
// that reaches the ingest path.
func TestOAuthVerifierNeverGrantsIngest(t *testing.T) {
	for _, granted := range [][]string{
		{ScopeIngest},
		{ScopeRead, ScopeIngest},
		{ScopeAdmin, ScopeIngest, ScopeWrite},
	} {
		v := newOAuthVerifier(fakeTokenVerifier{tok: &oidclient.AccessToken{
			Subject: "sub-abc",
			Scopes:  slices.Clone(granted),
		}}, &fakeResolver{id: 1})

		id, err := v.VerifyToken(context.Background(), "token")
		if err != nil {
			t.Fatalf("VerifyToken(%v): %v", granted, err)
		}
		if slices.Contains(id.Scopes, ScopeIngest) {
			t.Errorf("token granting %v produced scopes %v, which include ingest", granted, id.Scopes)
		}
		// And the derived permission, not merely the string, must be absent.
		if id.Allows(ScopeIngest) {
			t.Errorf("token granting %v produced an identity that allows ingest", granted)
		}
	}
}

// An empty scope set means something specific to Identity.Allows -- the legacy
// read+write grant. An OAuth token that was granted only ingest must not fall
// into that bucket after filtering, because stripping its one scope would
// otherwise promote it.
func TestOAuthVerifierIngestOnlyTokenDoesNotBecomeLegacyGrant(t *testing.T) {
	v := newOAuthVerifier(fakeTokenVerifier{tok: &oidclient.AccessToken{
		Subject: "sub-abc",
		Scopes:  []string{ScopeIngest},
	}}, &fakeResolver{id: 1})

	id, err := v.VerifyToken(context.Background(), "token")
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.Allows(ScopeRead) || id.Allows(ScopeWrite) {
		t.Errorf("ingest-only token was promoted to the legacy read+write grant: scopes=%v", id.Scopes)
	}
}

func TestOAuthVerifierRejectsAnInvalidToken(t *testing.T) {
	v := newOAuthVerifier(fakeTokenVerifier{err: oidclient.ErrInvalidToken}, &fakeResolver{id: 1})

	_, err := v.VerifyToken(context.Background(), "token")
	if err == nil {
		t.Fatal("VerifyToken accepted an invalid token")
	}
	// A bad token is a 401, so it must NOT look like an availability failure.
	if errors.Is(err, ErrAuthUnavailable) {
		t.Errorf("invalid token reported as an availability failure: %v", err)
	}
}

// The distinction this preserves is a 503 against a 401. Answering "the
// authorization server is unreachable" with "your token is bad" sends every
// client into a reauthentication loop against a server already struggling.
func TestOAuthVerifierPropagatesKeyUnavailability(t *testing.T) {
	v := newOAuthVerifier(fakeTokenVerifier{err: oidclient.ErrKeysUnavailable}, &fakeResolver{id: 1})

	_, err := v.VerifyToken(context.Background(), "token")
	if err == nil {
		t.Fatal("VerifyToken succeeded with an unavailable key set")
	}
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Errorf("error %v does not match ErrAuthUnavailable", err)
	}
}

// A resolver failure is our failure, not the caller's: the token was good and
// we could not establish who it belongs to.
func TestOAuthVerifierTreatsResolverFailureAsUnavailable(t *testing.T) {
	v := newOAuthVerifier(fakeTokenVerifier{tok: &oidclient.AccessToken{
		Subject: "sub-abc",
		Scopes:  []string{ScopeRead},
	}}, &fakeResolver{err: errors.New("database is on fire")})

	_, err := v.VerifyToken(context.Background(), "token")
	if err == nil {
		t.Fatal("VerifyToken succeeded despite a resolver failure")
	}
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Errorf("error %v does not match ErrAuthUnavailable", err)
	}
}

// A resolver that returns user 0 would silently scope the caller to the default
// user's namespace -- every OAuth caller sharing one memory. That must fail.
func TestOAuthVerifierRejectsUnresolvedUser(t *testing.T) {
	v := newOAuthVerifier(fakeTokenVerifier{tok: &oidclient.AccessToken{
		Subject: "sub-abc",
		Scopes:  []string{ScopeRead},
	}}, &fakeResolver{id: 0})

	if _, err := v.VerifyToken(context.Background(), "token"); err == nil {
		t.Fatal("VerifyToken accepted a token that resolved to user 0")
	}
}

// --- end-to-end against a real oidclient.ResourceServer ---------------------

// This is the test that proves the library actually works for memstore, rather
// than that the adapter maps a struct correctly.
func TestOAuthVerifierEndToEndAgainstOIDClient(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "memstore-test-kid"
	const resource = "https://memstore.example.test/memstore/mcp"

	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		doc := map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc) //nolint:errcheck // test fixture
	}))
	defer srv.Close()
	issuer = srv.URL

	rs, err := oidclient.NewResourceServer(context.Background(), oidclient.ResourceServerConfig{
		IssuerURL: issuer,
		Resource:  resource,
		JWKSURL:   srv.URL + "/jwks",
	})
	if err != nil {
		t.Fatalf("NewResourceServer: %v", err)
	}

	mint := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}
	good := jwt.MapClaims{
		"iss": issuer, "sub": "sub-e2e", "aud": resource,
		"email": "e2e@example.test", "scope": "read write ingest",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}

	res := &fakeResolver{id: 7}
	v := newOAuthVerifier(rs, res)

	id, err := v.VerifyToken(context.Background(), mint(good))
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.UserID != 7 || id.Source != "oauth" {
		t.Errorf("Identity = %+v, want UserID 7 and Source oauth", id)
	}
	if id.Name != "e2e@example.test" {
		t.Errorf("Name = %q, want e2e@example.test", id.Name)
	}
	// The scope claim asked for ingest; the adapter must have dropped it even
	// though a real authorization server granted it.
	if id.Allows(ScopeIngest) {
		t.Error("an end-to-end token granting ingest produced an identity that allows it")
	}
	if !id.Allows(ScopeRead) || !id.Allows(ScopeWrite) {
		t.Errorf("read/write were not granted: scopes=%v", id.Scopes)
	}

	// A token for a different resource must not authenticate here.
	wrong := jwt.MapClaims{}
	for k, val := range good {
		wrong[k] = val
	}
	wrong["aud"] = "https://herald.example.test/mcp"
	if _, err := v.VerifyToken(context.Background(), mint(wrong)); err == nil {
		t.Error("VerifyToken accepted a token minted for another resource")
	}
}
