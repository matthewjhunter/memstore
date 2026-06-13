package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/matthewjhunter/memstore/pgstore"
)

// startDaemon launches run() in a goroutine and returns the bound address plus
// a stop function that cancels the daemon and waits for it to exit.
func startDaemon(t *testing.T, args []string) (addr string, stop func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	addrCh := make(chan net.Addr, 1)
	errCh := make(chan error, 1)

	go func() {
		errCh <- run(ctx, args, io.Discard, func(a net.Addr) { addrCh <- a })
	}()

	select {
	case a := <-addrCh:
		addr = a.String()
	case err := <-errCh:
		cancel()
		t.Fatalf("daemon exited before binding: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("daemon did not bind within 5s")
	}

	stop = func() error {
		cancel()
		select {
		case err := <-errCh:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("daemon did not exit within 5s")
		}
	}
	return addr, stop
}

// writeServerCert writes a self-signed ECDSA cert valid for 127.0.0.1 to
// dir/cert.pem and dir/key.pem, returning the file paths.
func writeServerCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	cert, key := mintCert(t, "memstored-test", []string{"127.0.0.1", "localhost"}, nil, false)
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writePEM(t, certPath, "CERTIFICATE", cert)
	writeKeyPEM(t, keyPath, key)
	return certPath, keyPath
}

// mintCA returns a CA cert and its private key, writes the CA cert to dir/ca.pem.
func mintCA(t *testing.T, dir string) (caPath string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) {
	t.Helper()
	der, key := mintCert(t, "memstored-test-ca", nil, nil, true)
	caPath = filepath.Join(dir, "ca.pem")
	writePEM(t, caPath, "CERTIFICATE", der)
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return caPath, parsed, key
}

// mintCert generates an ECDSA cert. If isCA, it's self-signed and CA-marked.
// Otherwise it's a leaf cert; pass parent + parentKey to chain to a CA.
func mintCert(t *testing.T, cn string, sans []string, parentChain *signed, isCA bool) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
		tmpl.BasicConstraintsValid = true
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	parent, parentKey := tmpl, any(key)
	if parentChain != nil {
		parent = parentChain.cert
		parentKey = parentChain.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der, key
}

type signed struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, path, "EC PRIVATE KEY", der)
}

// httpsClient builds a client that trusts the given CA file (or the server
// cert directly, if it's a self-signed leaf).
func httpsClient(t *testing.T, caFile string, clientCert *tls.Certificate) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("no certs in trust file")
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg},
		Timeout:   5 * time.Second,
	}
}

// memstoredDBCounter makes each ephemeral database name unique within this
// process. Combined with the PID it is collision-free across concurrent package
// binaries sharing one Postgres server, without relying on time or RNG.
var memstoredDBCounter atomic.Int64

// testDSN creates a fresh, private database on the server that MEMSTORE_TEST_PG
// points at and returns a DSN targeting it. Each daemon test thus migrates and
// runs against its own database, never sharing schema state (the session
// migration's UNIQUE constraints, the pgvector extension, the default_user row)
// with the pgstore tests. Under `go test ./...` default parallelism on one
// shared Postgres service those concurrent migrations otherwise collide -- e.g.
// a UNIQUE-index creation racing on its name (SQLSTATE 42P07), which the 012a
// migration's duplicate_object guard does not catch.
//
// Cleanup registered on t DROPs the database with FORCE from a fresh admin
// connection (best-effort; logged on failure).
func testDSN(t *testing.T) string {
	t.Helper()
	adminDSN := os.Getenv("MEMSTORE_TEST_PG")
	if adminDSN == "" {
		t.Skip("MEMSTORE_TEST_PG not set; skipping memstored tests (requires PostgreSQL)")
	}

	ctx := context.Background()

	adminCfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse MEMSTORE_TEST_PG: %v", err)
	}

	// Lowercase, valid identifier; unique per process + call (no time/RNG).
	dbName := fmt.Sprintf("memstored_test_%d_%d", os.Getpid(), memstoredDBCounter.Add(1))

	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	// Drop any stale leftover (a crashed prior run could have reused this PID),
	// then create fresh. CREATE DATABASE cannot be parameterized; the identifier
	// is process-derived, not user input.
	admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		admin.Close(ctx)
		t.Fatalf("create database %s: %v", dbName, err)
	}
	admin.Close(ctx)

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		a, err := pgx.ConnectConfig(cleanupCtx, adminCfg)
		if err != nil {
			t.Logf("memstored cleanup: connect to drop %s: %v", dbName, err)
			return
		}
		defer a.Close(cleanupCtx)
		if _, err := a.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName)); err != nil {
			t.Logf("memstored cleanup: drop %s: %v", dbName, err)
		}
	})

	return dsnForDatabase(adminCfg, dbName)
}

// dsnForDatabase builds a keyword DSN from the parsed admin config with the
// database name swapped. Reconstructing from fields (rather than pgx's
// ConnString(), which returns the original parsed string and would ignore the
// swap) keeps the daemon's --pg flag and seedIdentity's pool pointed at the new
// database. sslmode follows whether the admin config negotiated TLS.
func dsnForDatabase(cfg *pgx.ConnConfig, dbName string) string {
	sslmode := "disable"
	if cfg.TLSConfig != nil {
		sslmode = "require"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "host=%s port=%d dbname=%s sslmode=%s", cfg.Host, cfg.Port, dbName, sslmode)
	if cfg.User != "" {
		fmt.Fprintf(&b, " user=%s", cfg.User)
	}
	if cfg.Password != "" {
		fmt.Fprintf(&b, " password=%s", cfg.Password)
	}
	return b.String()
}

// seedIdentity prepares the database so the daemon can start: store
// construction requires a recorded default user (see pgstore.InitIdentity).
// The first pgstore.New on a virgin database migrates the schema and then
// fails at user resolution; that failure is expected and the migration is
// committed before it.
func seedIdentity(t *testing.T, dsn, namespace string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	if _, err := pgstore.New(ctx, pool, nil, namespace, 768, 512); err != nil && !strings.Contains(err.Error(), "tier3-init") {
		t.Fatalf("pgstore.New (schema init): %v", err)
	}
	if err := pgstore.InitIdentity(ctx, pool, namespace, "testuser"); err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
}

// commonArgs returns the minimal flag set needed to boot the daemon against
// the test PostgreSQL on an ephemeral port. Each test uses a unique namespace
// so concurrent runs don't see each other's data.
func commonArgs(t *testing.T) []string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // ignore any user config
	t.Setenv("XDG_DATA_HOME", t.TempDir())   // isolate any defaults
	dsn := testDSN(t)
	ns := "test-" + t.Name()
	seedIdentity(t, dsn, ns)
	return []string{
		"--addr", "127.0.0.1:0",
		"--pg", dsn,
		"--namespace", ns,
		"--vec-dim", "768",
		"--ollama", "http://127.0.0.1:1", // never actually called in these tests
	}
}

func TestRun_RejectsPositionalArgs(t *testing.T) {
	// Regression: `memstored admin` (or any unknown subcommand) used to silently
	// boot the daemon, then fail noisily when the backfill goroutine raced the
	// closed pool on shutdown. Validate args before touching the DB.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := run(ctx, []string{"admin"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("expected 'unexpected argument' error, got %v", err)
	}
}

func TestRun_TLSRequiredWithoutCerts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := run(ctx, commonArgs(t), io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "TLS required") {
		t.Fatalf("expected 'TLS required' error, got %v", err)
	}
}

func TestRun_TLSDisabled_PlaintextHealth(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled")
	addr, stop := startDaemon(t, args)
	defer func() {
		_ = stop()
	}()

	resp, err := http.Get("http://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRun_TLSEnabled_HTTPSHealth(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeServerCert(t, dir)

	args := append(commonArgs(t),
		"--tls-cert-file", certFile,
		"--tls-key-file", keyFile,
	)
	addr, stop := startDaemon(t, args)
	defer func() {
		_ = stop()
	}()

	client := httpsClient(t, certFile, nil)
	resp, err := client.Get("https://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Plaintext against a TLS listener: Go's stdlib answers with HTTP 400
	// ("client sent an HTTP request to an HTTPS server"), so we assert the
	// request never reaches the handler rather than expecting a transport
	// error.
	plain, err := http.Get("http://" + addr + "/v1/health")
	if err == nil {
		plain.Body.Close()
		if plain.StatusCode == http.StatusOK {
			t.Fatal("plaintext GET against TLS listener got 200 OK")
		}
	}
}

func TestRun_MTLS_ClientCertRequired(t *testing.T) {
	dir := t.TempDir()
	caPath, caCert, caKey := mintCA(t, dir)

	// Server cert signed by the CA, valid for 127.0.0.1.
	serverDER, serverKey := mintCert(t, "memstored", []string{"127.0.0.1", "localhost"},
		&signed{cert: caCert, key: caKey}, false)
	serverCertFile := filepath.Join(dir, "server.pem")
	serverKeyFile := filepath.Join(dir, "server-key.pem")
	writePEM(t, serverCertFile, "CERTIFICATE", serverDER)
	writeKeyPEM(t, serverKeyFile, serverKey)

	// Client cert signed by the same CA.
	clientDER, clientKey := mintCert(t, "test-user", nil, &signed{cert: caCert, key: caKey}, false)
	clientKeyDER, _ := x509.MarshalECPrivateKey(clientKey)
	clientCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}),
	)
	if err != nil {
		t.Fatalf("build client cert: %v", err)
	}

	args := append(commonArgs(t),
		"--tls-cert-file", serverCertFile,
		"--tls-key-file", serverKeyFile,
		"--tls-client-ca-file", caPath,
	)
	addr, stop := startDaemon(t, args)
	defer func() {
		_ = stop()
	}()

	// With the right client cert: success.
	withCert := httpsClient(t, caPath, &clientCert)
	resp, err := withCert.Get("https://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("mTLS GET with valid client cert: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Without a client cert: handshake must fail.
	noCert := httpsClient(t, caPath, nil)
	if _, err := noCert.Get("https://" + addr + "/v1/health"); err == nil {
		t.Fatal("mTLS request without client cert unexpectedly succeeded")
	}
}
