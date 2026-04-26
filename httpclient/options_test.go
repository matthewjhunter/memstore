package httpclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/internal/caetl"
)

// startTLSServer mints a self-signed CA + server cert valid for 127.0.0.1
// and returns an HTTPS test server using them, plus the path to the CA cert
// (PEM) the client should trust. If clientCAPath is non-empty the server
// requires mTLS against that CA.
func startTLSServer(t *testing.T, clientCAPath string) (server *httptest.Server, caCertFile string) {
	t.Helper()
	dir := t.TempDir()

	ca, err := caetl.NewCA("test-ca", 0)
	if err != nil {
		t.Fatal(err)
	}
	caCertFile = filepath.Join(dir, "ca.pem")
	caKeyFile := filepath.Join(dir, "ca.key")
	if err := ca.Save(caCertFile, caKeyFile); err != nil {
		t.Fatal(err)
	}

	srvPair, err := ca.IssueServer("localhost", []string{"127.0.0.1", "localhost"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	tlsCert := tls.Certificate{
		Certificate: [][]byte{srvPair.Cert.Raw},
		PrivateKey:  srvPair.Key,
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAPath != "" {
		caPEM, err := os.ReadFile(clientCAPath)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			t.Fatal("no certs in client CA bundle")
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stub the count endpoint — enough to exercise the transport.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"count":0}`))
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, caCertFile
}

// dialOnce issues a single GET that exercises the configured transport.
func dialOnce(t *testing.T, c *httpclient.Client) error {
	t.Helper()
	_, err := c.ActiveCount(t.Context())
	return err
}

// dialOnceFast is dialOnce with a short context — used for negative cases so
// the client's retry/backoff doesn't dominate the test runtime.
func dialOnceFast(t *testing.T, c *httpclient.Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := c.ActiveCount(ctx)
	return err
}

func TestClient_TLSCAFile_TrustsSelfSignedServer(t *testing.T) {
	srv, caFile := startTLSServer(t, "")

	// Without TLSCAFile, the system trust store will reject the self-signed cert.
	plain := httpclient.New(srv.URL, "")
	if err := dialOnceFast(t, plain); err == nil {
		t.Fatal("expected handshake failure without TLSCAFile, got nil")
	}

	// With TLSCAFile pointed at the issuing CA, the same call succeeds.
	c, err := httpclient.NewWithOptions(srv.URL, "", httpclient.ClientOptions{TLSCAFile: caFile})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if err := dialOnce(t, c); err != nil {
		t.Fatalf("health with trusted CA: %v", err)
	}
}

func TestClient_MTLS_PresentsClientCert(t *testing.T) {
	// Server CA is issued first via startTLSServer (which builds a CA, server
	// cert, etc.); we then issue a client cert against that same CA by loading
	// it back. Simulates running `memstore tls init` then `tls issue-client`.
	dir := t.TempDir()
	ca, err := caetl.NewCA("test-ca", 0)
	if err != nil {
		t.Fatal(err)
	}
	caCertFile := filepath.Join(dir, "ca.pem")
	caKeyFile := filepath.Join(dir, "ca.key")
	if err := ca.Save(caCertFile, caKeyFile); err != nil {
		t.Fatal(err)
	}

	// Server uses the CA we just made.
	srvPair, err := ca.IssueServer("localhost", []string{"127.0.0.1", "localhost"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	tlsCert := tls.Certificate{
		Certificate: [][]byte{srvPair.Cert.Raw},
		PrivateKey:  srvPair.Key,
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","facts":0}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// Client cert against the same CA.
	clientPair, err := ca.IssueClient("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	clientCertFile := filepath.Join(dir, "alice.pem")
	clientKeyFile := filepath.Join(dir, "alice.key")
	if err := clientPair.Save(clientCertFile, clientKeyFile); err != nil {
		t.Fatal(err)
	}

	// Without a client cert, mTLS rejects the connection.
	noCert, err := httpclient.NewWithOptions(srv.URL, "", httpclient.ClientOptions{TLSCAFile: caCertFile})
	if err != nil {
		t.Fatal(err)
	}
	if err := dialOnceFast(t, noCert); err == nil {
		t.Fatal("expected mTLS handshake failure without client cert")
	}

	// With the client cert, success.
	withCert, err := httpclient.NewWithOptions(srv.URL, "", httpclient.ClientOptions{
		TLSCAFile:         caCertFile,
		TLSClientCertFile: clientCertFile,
		TLSClientKeyFile:  clientKeyFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dialOnce(t, withCert); err != nil {
		t.Fatalf("mTLS request with client cert: %v", err)
	}
}

func TestClient_MTLS_RejectsHalfConfig(t *testing.T) {
	_, err := httpclient.NewWithOptions("https://example.invalid", "",
		httpclient.ClientOptions{TLSClientCertFile: "/dev/null"})
	if err == nil {
		t.Fatal("expected error when cert is set without key")
	}
	_, err = httpclient.NewWithOptions("https://example.invalid", "",
		httpclient.ClientOptions{TLSClientKeyFile: "/dev/null"})
	if err == nil {
		t.Fatal("expected error when key is set without cert")
	}
}

func TestClient_TLSCAFile_BadPath(t *testing.T) {
	_, err := httpclient.NewWithOptions("https://example.invalid", "",
		httpclient.ClientOptions{TLSCAFile: "/nonexistent/ca.pem"})
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestClientOptionsFromConfig(t *testing.T) {
	cfg := memstore.AppConfig{
		TLSCAFile:         "/etc/memstore/ca.pem",
		TLSClientCertFile: "/etc/memstore/alice.pem",
		TLSClientKeyFile:  "/etc/memstore/alice.key",
	}
	opts := httpclient.ClientOptionsFromConfig(cfg)
	if opts.TLSCAFile != cfg.TLSCAFile {
		t.Errorf("TLSCAFile = %q", opts.TLSCAFile)
	}
	if !opts.HasTLSConfig() {
		t.Error("HasTLSConfig should be true")
	}
	if !httpclient.ClientOptionsFromConfig(memstore.AppConfig{}).HasTLSConfig() == false {
		t.Error("empty config should not have TLS config")
	}
}
