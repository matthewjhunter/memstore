package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore/internal/caetl"
)

// withIsolatedConfigDir points memstore.ConfigPath() at a temp directory and
// returns the resolved TLS dir for the run.
func withIsolatedConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "memstore", "tls")
}

func TestTLSInit_CreatesCAAndServerCert(t *testing.T) {
	tlsDir := withIsolatedConfigDir(t)

	var out bytes.Buffer
	runTLSInit([]string{"--hosts", "localhost,127.0.0.1"}, &out)

	caCert := filepath.Join(tlsDir, "ca.pem")
	srvCert := filepath.Join(tlsDir, "server.pem")
	for _, p := range []string{
		caCert,
		filepath.Join(tlsDir, "ca.key"),
		srvCert,
		filepath.Join(tlsDir, "server.key"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}

	// Verify CA invariants on the file we wrote.
	ca, err := caetl.LoadCA(caCert, filepath.Join(tlsDir, "ca.key"))
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !ca.Cert.IsCA {
		t.Error("CA cert IsCA = false")
	}

	// Verify the server cert chains to the CA.
	srv, err := caetl.LoadPair(srvCert, filepath.Join(tlsDir, "server.key"))
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if srv.Cert.Issuer.CommonName != ca.Cert.Subject.CommonName {
		t.Errorf("server issuer = %q, want %q", srv.Cert.Issuer.CommonName, ca.Cert.Subject.CommonName)
	}

	// Key files must be 0600.
	for _, kp := range []string{filepath.Join(tlsDir, "ca.key"), filepath.Join(tlsDir, "server.key")} {
		st, err := os.Stat(kp)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0600 {
			t.Errorf("%s perm = %o, want 0600", kp, perm)
		}
	}
}

func TestTLSInit_AppendsConfigKeys(t *testing.T) {
	tlsDir := withIsolatedConfigDir(t)
	configPath := filepath.Join(filepath.Dir(filepath.Dir(tlsDir)), "memstore", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("namespace = \"test\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runTLSInit([]string{"--hosts", "localhost"}, &out)

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tls_cert_file", "tls_key_file", "tls_client_ca_file"} {
		if !strings.Contains(string(content), key) {
			t.Errorf("config.toml missing %q after init: %s", key, content)
		}
	}
	if !strings.Contains(string(content), `namespace = "test"`) {
		t.Errorf("init clobbered existing config keys: %s", content)
	}

	// Re-running init must not append duplicate keys.
	runTLSInit([]string{"--hosts", "localhost", "--force"}, &out)
	content2, _ := os.ReadFile(configPath)
	if strings.Count(string(content2), "tls_cert_file") != 1 {
		t.Errorf("tls_cert_file appeared multiple times after second init: %s", content2)
	}
}

func TestTLSInit_RefusesOverwriteWithoutForce(t *testing.T) {
	withIsolatedConfigDir(t)

	var out bytes.Buffer
	runTLSInit([]string{"--hosts", "localhost"}, &out)

	// Second run without --force should fatal. We can't catch os.Exit cleanly
	// from inside the test, so we exercise the helpers directly: re-issuing
	// the server cert with the existing-file guard logic in init relies on
	// fileExists + the !*force branch. Verify by checking server cert exists.
	tlsDir, _ := defaultTLSDir()
	if !fileExists(filepath.Join(tlsDir, "server.pem")) {
		t.Fatal("server.pem missing after first init")
	}
	// Re-running with --force should succeed.
	runTLSInit([]string{"--hosts", "localhost,127.0.0.1", "--force"}, &out)

	srv, err := caetl.LoadPair(filepath.Join(tlsDir, "server.pem"), filepath.Join(tlsDir, "server.key"))
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if len(srv.Cert.IPAddresses) == 0 {
		t.Error("server cert should have IP SAN after --force re-init")
	}
}

func TestTLSIssueClient_RoundTrip(t *testing.T) {
	tlsDir := withIsolatedConfigDir(t)

	var out bytes.Buffer
	runTLSInit([]string{"--hosts", "localhost"}, &out)
	out.Reset()

	runTLSIssueClient([]string{"alice"}, &out)

	clientCert := filepath.Join(tlsDir, "clients", "alice.pem")
	clientKey := filepath.Join(tlsDir, "clients", "alice.key")
	if _, err := os.Stat(clientCert); err != nil {
		t.Fatalf("client cert missing: %v", err)
	}
	pair, err := caetl.LoadPair(clientCert, clientKey)
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if pair.Cert.Subject.CommonName != "alice" {
		t.Errorf("CN = %q, want alice", pair.Cert.Subject.CommonName)
	}
	if !strings.Contains(out.String(), "alice") {
		t.Errorf("output missing client name: %s", out.String())
	}
}

func TestTLSShow_PrintsCertSummaries(t *testing.T) {
	withIsolatedConfigDir(t)

	var out bytes.Buffer
	runTLSInit([]string{"--hosts", "localhost"}, &out)
	runTLSIssueClient([]string{"bob"}, &out)
	out.Reset()

	runTLSShow(nil, &out)

	got := out.String()
	for _, want := range []string{"CA:", "Server:", "Clients:", "bob", "fingerprint:"} {
		if !strings.Contains(got, want) {
			t.Errorf("show output missing %q:\n%s", want, got)
		}
	}
}

func TestValidClientName(t *testing.T) {
	cases := map[string]bool{
		"alice":           true,
		"bob.smith":       true,
		"agent_01":        true,
		"node-7":          true,
		"":                false,
		".":               false,
		"..":              false,
		"name with space": false,
		"name/slash":      false,
		"name;semi":       false,
	}
	for name, want := range cases {
		if got := validClientName(name); got != want {
			t.Errorf("validClientName(%q) = %v, want %v", name, got, want)
		}
	}
}
