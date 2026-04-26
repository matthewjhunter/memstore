package caetl_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore/internal/caetl"
)

func TestNewCA_Invariants(t *testing.T) {
	ca, err := caetl.NewCA("memstore-test-ca", 0)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	if !ca.Cert.IsCA {
		t.Error("CA cert IsCA = false")
	}
	if !ca.Cert.BasicConstraintsValid {
		t.Error("CA cert BasicConstraintsValid = false")
	}
	if ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert missing KeyUsageCertSign")
	}
	if ca.Cert.SerialNumber.Sign() <= 0 {
		t.Error("CA serial must be positive")
	}
	if !ca.Cert.NotAfter.After(time.Now().Add(9 * 365 * 24 * time.Hour)) {
		t.Errorf("CA validity shorter than expected: NotAfter=%s", ca.Cert.NotAfter)
	}
	if ca.Cert.MaxPathLen != 0 || !ca.Cert.MaxPathLenZero {
		t.Error("CA MaxPathLen should be 0 (no intermediates allowed)")
	}
}

func TestNewCA_RejectsEmptyCN(t *testing.T) {
	if _, err := caetl.NewCA("", 0); err == nil {
		t.Fatal("NewCA(\"\") should fail")
	}
}

func TestIssueServer_Invariants(t *testing.T) {
	ca, err := caetl.NewCA("test-ca", 0)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := ca.IssueServer("memstored", []string{"127.0.0.1", "localhost", "memstored.example"}, 0)
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}

	if pair.Cert.IsCA {
		t.Error("server leaf must not be a CA")
	}
	if !pair.Cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid must be true")
	}

	hasServerAuth := false
	for _, eku := range pair.Cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
	}
	if !hasServerAuth {
		t.Error("server cert missing ExtKeyUsageServerAuth")
	}

	if len(pair.Cert.DNSNames) != 2 {
		t.Errorf("DNSNames = %v, want 2 entries", pair.Cert.DNSNames)
	}
	if len(pair.Cert.IPAddresses) != 1 || !pair.Cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("IPAddresses = %v, want [127.0.0.1]", pair.Cert.IPAddresses)
	}
}

func TestIssueServer_RejectsNoHosts(t *testing.T) {
	ca, _ := caetl.NewCA("test-ca", 0)
	if _, err := ca.IssueServer("memstored", nil, 0); err == nil {
		t.Fatal("IssueServer with no hosts should fail")
	}
}

func TestIssueClient_Invariants(t *testing.T) {
	ca, _ := caetl.NewCA("test-ca", 0)
	pair, err := ca.IssueClient("alice", 0)
	if err != nil {
		t.Fatalf("IssueClient: %v", err)
	}
	if pair.Cert.IsCA {
		t.Error("client leaf must not be a CA")
	}
	hasClientAuth := false
	for _, eku := range pair.Cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		t.Error("client cert missing ExtKeyUsageClientAuth")
	}
	if pair.Cert.Subject.CommonName != "alice" {
		t.Errorf("CN = %q, want alice", pair.Cert.Subject.CommonName)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	caCert := filepath.Join(dir, "ca.pem")
	caKey := filepath.Join(dir, "ca.key")
	srvCert := filepath.Join(dir, "server.pem")
	srvKey := filepath.Join(dir, "server.key")

	ca, err := caetl.NewCA("test-ca", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Save(caCert, caKey); err != nil {
		t.Fatalf("CA Save: %v", err)
	}

	pair, err := ca.IssueServer("memstored", []string{"127.0.0.1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := pair.Save(srvCert, srvKey); err != nil {
		t.Fatalf("Pair Save: %v", err)
	}

	// Load back.
	caLoaded, err := caetl.LoadCA(caCert, caKey)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if caLoaded.Cert.SerialNumber.Cmp(ca.Cert.SerialNumber) != 0 {
		t.Error("CA serial mismatch after round-trip")
	}

	pairLoaded, err := caetl.LoadPair(srvCert, srvKey)
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if pairLoaded.Cert.SerialNumber.Cmp(pair.Cert.SerialNumber) != 0 {
		t.Error("leaf serial mismatch after round-trip")
	}

	// Issued cert must verify against the saved CA.
	pool := x509.NewCertPool()
	pool.AddCert(caLoaded.Cert)
	_, err = pairLoaded.Cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Fatalf("server cert failed to verify against CA: %v", err)
	}
}

// TestEndToEnd_TLSHandshake proves the issued cert actually works for a real
// TLS handshake — catches any mismatch between template flags and what the
// stdlib TLS stack accepts.
func TestEndToEnd_TLSHandshake(t *testing.T) {
	ca, err := caetl.NewCA("test-ca", 0)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := ca.IssueServer("memstored", []string{"127.0.0.1"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{pair.Cert.Raw},
		PrivateKey:  pair.Key,
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: "127.0.0.1",
		}},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestFingerprint_Stable(t *testing.T) {
	ca, _ := caetl.NewCA("test-ca", 0)
	fp1 := caetl.Fingerprint(ca.Cert)
	fp2 := caetl.Fingerprint(ca.Cert)
	if fp1 != fp2 {
		t.Errorf("fingerprint not stable: %q vs %q", fp1, fp2)
	}
	// SHA-256 hex with colons: 32 bytes → 64 hex chars + 31 colons = 95 chars.
	if len(fp1) != 95 {
		t.Errorf("fingerprint length = %d, want 95 (got %q)", len(fp1), fp1)
	}
	if !strings.Contains(fp1, ":") {
		t.Errorf("fingerprint missing separators: %q", fp1)
	}
}

func TestLoadCA_RejectsLeafCert(t *testing.T) {
	dir := t.TempDir()
	ca, _ := caetl.NewCA("test-ca", 0)
	pair, _ := ca.IssueServer("memstored", []string{"127.0.0.1"}, 0)

	leafCert := filepath.Join(dir, "leaf.pem")
	leafKey := filepath.Join(dir, "leaf.key")
	if err := pair.Save(leafCert, leafKey); err != nil {
		t.Fatal(err)
	}
	if _, err := caetl.LoadCA(leafCert, leafKey); err == nil {
		t.Fatal("LoadCA should reject a non-CA cert")
	}
}
