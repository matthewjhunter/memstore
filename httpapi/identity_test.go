package httpapi_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

func TestIdentityContext_RoundTrip(t *testing.T) {
	id := httpapi.Identity{Name: "matthew-laptop", Scopes: []string{"read", "write"}, Source: "bearer"}
	ctx := httpapi.WithIdentity(context.Background(), id)

	got, ok := httpapi.IdentityFromContext(ctx)
	if !ok {
		t.Fatal("IdentityFromContext returned ok=false")
	}
	if got.Name != id.Name || got.Source != id.Source {
		t.Errorf("got %+v, want %+v", got, id)
	}
	if !got.HasScope("read") || got.HasScope("admin") {
		t.Errorf("HasScope wrong: %+v", got.Scopes)
	}
}

func TestIdentityFromContext_Empty(t *testing.T) {
	if _, ok := httpapi.IdentityFromContext(context.Background()); ok {
		t.Error("expected ok=false on empty context")
	}
}

func TestPeerIdentity_NoTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := httpapi.PeerIdentity(r); ok {
		t.Error("expected ok=false for non-TLS request")
	}
}

func TestPeerIdentity_FromCertCN(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: "alice-laptop"}},
		},
	}
	id, ok := httpapi.PeerIdentity(r)
	if !ok {
		t.Fatal("ok=false despite peer cert")
	}
	if id.Name != "alice-laptop" {
		t.Errorf("Name = %q, want alice-laptop", id.Name)
	}
	if id.Source != "mtls" {
		t.Errorf("Source = %q, want mtls", id.Source)
	}
}
