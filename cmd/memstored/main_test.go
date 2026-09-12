package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/testpg"
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
// testDSN hands each daemon test a private database; see internal/testpg.
func testDSN(t *testing.T) string {
	t.Helper()
	return testpg.DSN(t)
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

// Disabling TLS is not enough to serve plaintext. The operator has to affirm
// that the listener is only reachable over a trusted path, because memstored
// cannot tell that for itself: in Docker a proxy-fronted deployment binds
// 0.0.0.0 inside a private network, which looks exactly like 0.0.0.0 on a LAN.
// Sniffing the interface would refuse the safe case and get switched off
// reflexively, so the affirmation is asked for once, explicitly.
//
// What it protects: every bearer token and every recalled fact crosses that
// listener in the clear. Matthew's own LAN is trusted enough for it; nobody
// else's network is memstore's to assume.
func TestRun_TLSDisabledAloneIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flag only", []string{"--tls-disabled"}},
		{"with a database configured", []string{"--tls-disabled", "--pg", "postgres://nobody@127.0.0.1:1/none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertTransportRefusal(t, tc.args, "--insecure-plaintext", "trusted")
		})
	}
}

// assertTransportRefusal runs the daemon and expects it to refuse before
// touching anything external. No database is configured or reachable in these
// cases, which is deliberate: a transport misconfiguration must surface as
// itself, not as a connection failure behind it.
func assertTransportRefusal(t *testing.T, args []string, want ...string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	err := run(context.Background(), args, io.Discard, nil)
	if err == nil {
		t.Fatal("started with no affirmation; expected a refusal")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal does not mention %q, so the operator is not told how to proceed: %v", w, err)
		}
	}
}

// TLS stays the default: neither flag, no certs, still a refusal. The
// affirmation is about plaintext, not a way to skip configuring TLS.
func TestRun_InsecurePlaintextDoesNotBypassTLS(t *testing.T) {
	assertTransportRefusal(t, []string{"--insecure-plaintext"}, "tls-cert-file")
}

func TestRun_TLSDisabled_PlaintextHealth(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addr, stop := startDaemon(t, args)
	defer func() {
		_ = stop()
	}()

	resp, err := http.Get("http://" + addr + "/memstore/v1/health")
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
	resp, err := client.Get("https://" + addr + "/memstore/v1/health")
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
	plain, err := http.Get("http://" + addr + "/memstore/v1/health")
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
	resp, err := withCert.Get("https://" + addr + "/memstore/v1/health")
	if err != nil {
		t.Fatalf("mTLS GET with valid client cert: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Without a client cert: handshake must fail.
	noCert := httpsClient(t, caPath, nil)
	if _, err := noCert.Get("https://" + addr + "/memstore/v1/health"); err == nil {
		t.Fatal("mTLS request without client cert unexpectedly succeeded")
	}
}

// freshArgs is commonArgs without the identity seed: the database has a schema
// and nothing else, which is what a first `docker compose up` sees.
func freshArgs(t *testing.T) []string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	return []string{
		"--addr", "127.0.0.1:0",
		"--pg", testDSN(t),
		"--namespace", "test-" + t.Name(),
		"--vec-dim", "768",
		"--ollama", "http://127.0.0.1:1",
		"--tls-disabled", "--insecure-plaintext",
	}
}

func whoAmI(t *testing.T, addr, token string) (int, memstore.WhoAmIResponse) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+"/memstore/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET whoami: %v", err)
	}
	defer resp.Body.Close()
	var out memstore.WhoAmIResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// On an empty database, --default-user records the identity the legacy token
// binds to, so MEMSTORE_API_KEY authenticates from the first start with no
// out-of-band tier3-init. This is what lets a compose file come up cold.
func TestRun_DefaultUserBootstrapsIdentityForTheAPIKey(t *testing.T) {
	args := append(freshArgs(t), "--default-user", "trial", "--api-key", "trial-secret")
	addr, stop := startDaemon(t, args)
	defer func() { _ = stop() }()

	status, who := whoAmI(t, addr, "trial-secret")
	if status != http.StatusOK {
		t.Fatalf("whoami status = %d, want 200", status)
	}
	if !who.Authenticated || who.Name != "legacy" {
		t.Errorf("whoami = %+v, want authenticated as the legacy token", who)
	}
}

// Without --default-user the empty database has nobody to own anything, and
// the daemon must not invent someone: it refuses to start and names the fix.
func TestRun_EmptyDatabaseWithoutDefaultUserRefusesToStart(t *testing.T) {
	args := append(freshArgs(t), "--api-key", "trial-secret")
	err := run(context.Background(), args, io.Discard, func(net.Addr) {
		t.Fatal("daemon bound a listener on an empty database with no default user")
	})
	if err == nil || !strings.Contains(err.Error(), "default-user") {
		t.Fatalf("err = %v, want a refusal that names --default-user", err)
	}
}

// A second start with the same --default-user is a no-op, not a conflict.
func TestRun_DefaultUserIsIdempotentAcrossRestarts(t *testing.T) {
	args := append(freshArgs(t), "--default-user", "trial", "--api-key", "trial-secret")
	addr, stop := startDaemon(t, args)
	if s, _ := whoAmI(t, addr, "trial-secret"); s != http.StatusOK {
		t.Fatalf("first start: whoami = %d", s)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	addr, stop = startDaemon(t, args)
	defer func() { _ = stop() }()
	if s, _ := whoAmI(t, addr, "trial-secret"); s != http.StatusOK {
		t.Fatalf("second start: whoami = %d", s)
	}
}

// syncBuffer is a bytes.Buffer safe for the daemon's several logging
// goroutines to share with the test reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRun_LogsLeveledLogfmt pins the reason for the conversion: memstored's
// output has to carry a lowercase level= on every line, because that is what
// Loki reads as detected_level and what the error alert matches on.
func TestRun_LogsLeveledLogfmt(t *testing.T) {
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addrCh := make(chan net.Addr, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, args, &out, func(a net.Addr) { addrCh <- a }) }()
	select {
	case <-addrCh:
	case err := <-errCh:
		t.Fatalf("daemon exited before binding: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not bind within 5s")
	}
	cancel()
	<-errCh

	got := out.String()
	if got == "" {
		t.Fatal("no log output reached the writer run() was given")
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if !strings.Contains(line, "level=") {
			t.Errorf("line without a level: %s", line)
		}
		if strings.Contains(line, "level=INFO") || strings.Contains(line, "level=WARN") ||
			strings.Contains(line, "level=ERROR") || strings.Contains(line, "level=DEBUG") {
			t.Errorf("level must be lowercase for Loki's logfmt parser: %s", line)
		}
		if !strings.Contains(line, "time=") {
			t.Errorf("line without a timestamp: %s", line)
		}
	}
	// The plaintext listener is the one warning this configuration must produce.
	if !strings.Contains(got, "level=warn") {
		t.Errorf("expected the WITHOUT TLS warning at warn level, got:\n%s", got)
	}
}

// TestRun_LogLevelFilters proves the level knob reaches the handler.
func TestRun_LogLevelFilters(t *testing.T) {
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext", "--log-level", "error")
	addrCh := make(chan net.Addr, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, args, &out, func(a net.Addr) { addrCh <- a }) }()
	select {
	case <-addrCh:
	case err := <-errCh:
		t.Fatalf("daemon exited before binding: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not bind within 5s")
	}
	cancel()
	<-errCh

	if got := out.String(); strings.Contains(got, "level=info") || strings.Contains(got, "level=warn") {
		t.Errorf("--log-level=error should suppress info and warn, got:\n%s", got)
	}
}

// startDaemonLogging is startDaemon with the daemon's log output captured.
func startDaemonLogging(t *testing.T, args []string) (addr string, out *syncBuffer, stop func() error) {
	t.Helper()

	out = &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, args, out, func(a net.Addr) { addrCh <- a }) }()

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

	return addr, out, func() error {
		cancel()
		select {
		case err := <-errCh:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("daemon did not exit within 5s")
		}
	}
}

// TestRun_AccessLog is the point of wiring httplog in: every request the
// daemon serves leaves one line, with the fields a conventional access log
// carries, whether or not a proxy in front keeps its own.
func TestRun_AccessLog(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	// Query string included on purpose: it must not reach the log.
	resp, err := http.Get("http://" + addr + "/memstore/v1/whoami?token=sekrit")
	if err != nil {
		t.Fatalf("GET whoami: %v", err)
	}
	resp.Body.Close()
	_ = stop()

	var line string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "msg=http_access") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no access log line, got:\n%s", out.String())
	}
	for _, want := range []string{
		"level=info", "method=GET", "path=/memstore/v1/whoami",
		"proto=HTTP/1.1", "status=", "bytes=", "duration_ms=", "remote_addr=",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("access line missing %q: %s", want, line)
		}
	}
	if strings.Contains(out.String(), "sekrit") {
		t.Errorf("the query string reached the log: %s", line)
	}
}

// Health is what a monitor hits every few seconds and it says nothing when it
// succeeds; logging it buries everything else.
func TestRun_AccessLogSkipsHealth(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	for _, path := range []string{"/memstore/v1/health", "/v1/health"} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
	}
	_ = stop()

	if strings.Contains(out.String(), "msg=http_access") {
		t.Errorf("health should not be logged, got:\n%s", out.String())
	}
}

// The access log names the caller: on a daemon several machines share, a line
// that cannot say who made the request answers half the question.
func TestRun_AccessLogRecordsIdentity(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext", "--api-key", "test-key")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/memstore/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET whoami: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the access log needs an authenticated request)", resp.StatusCode)
	}
	_ = stop()

	if !strings.Contains(out.String(), "identity=legacy") {
		t.Errorf("expected identity on the access line, got:\n%s", out.String())
	}
}

// Without http.Server.ErrorLog, net/http writes handshake and protocol faults
// to stderr unstructured -- outside the logger, and outside every level-based
// alert.
func TestRun_ServerErrorsAreLogged(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeServerCert(t, dir)
	args := append(commonArgs(t), "--tls-cert-file", certFile, "--tls-key-file", keyFile)
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	// Plaintext at a TLS listener: a handshake failure the server reports
	// through ErrorLog and nowhere else.
	resp, err := http.Get("http://" + addr + "/memstore/v1/health")
	if err == nil {
		resp.Body.Close()
	}
	_ = stop()

	got := out.String()
	if !strings.Contains(got, "TLS handshake error") {
		t.Errorf("expected the handshake failure in the log, got:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.Contains(line, "TLS handshake error") && !strings.Contains(line, "level=error") {
			t.Errorf("handshake failure should be error level: %s", line)
		}
	}
}

// TestRun_AccessLogCarriesPhaseTimings is the whole point of #59 part 3: the
// breakdown sits on the line that already carries the status and the total,
// so "is it the embedder?" is one query rather than a correlation exercise.
func TestRun_AccessLogCarriesPhaseTimings(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext", "--api-key", "test-key")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	body := strings.NewReader(`{"query":"anything","limit":3}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/memstore/v1/search", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST search: %v", err)
	}
	resp.Body.Close()
	_ = stop()

	var line string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "msg=http_access") && strings.Contains(l, "/v1/search") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no access line for the search, got:\n%s", out.String())
	}
	// Embed is asserted rather than FTS because it is the one phase that runs
	// either way: a hybrid search embeds the query first and gives up if that
	// fails, and CI has no embedder to reach. Recording a failed phase is
	// deliberate -- a phase that logged nothing when it failed would hide the
	// cost of the failure, which is exactly the case worth seeing.
	for _, want := range []string{"embed_ms=", "embed_calls=", "request_id="} {
		if !strings.Contains(line, want) {
			t.Errorf("access line missing %q: %s", want, line)
		}
	}
}

// A request that measures nothing must not sprout empty phase fields.
func TestRun_AccessLogOmitsUnusedPhases(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	resp, err := http.Get("http://" + addr + "/memstore/v1/whoami")
	if err != nil {
		t.Fatalf("GET whoami: %v", err)
	}
	resp.Body.Close()
	_ = stop()

	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "msg=http_access") {
			continue
		}
		for _, unwanted := range []string{"fts_ms=", "embed_ms=", "vector_ms=", "rerank_ms="} {
			if strings.Contains(line, unwanted) {
				t.Errorf("whoami did no searching but reported %s: %s", unwanted, line)
			}
		}
	}
}

// Every request gets its own id, and the id is on the line.
func TestRun_RequestIDsArePerRequest(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	for range 2 {
		resp, err := http.Get("http://" + addr + "/memstore/v1/whoami")
		if err != nil {
			t.Fatalf("GET whoami: %v", err)
		}
		resp.Body.Close()
	}
	_ = stop()

	var ids []string
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "msg=http_access") {
			continue
		}
		_, rest, ok := strings.Cut(line, "request_id=")
		if !ok {
			t.Fatalf("access line without a request id: %s", line)
		}
		id, _, _ := strings.Cut(rest, " ")
		ids = append(ids, id)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 access lines, got %d:\n%s", len(ids), out.String())
	}
	if ids[0] == ids[1] {
		t.Errorf("both requests logged the same id %q", ids[0])
	}
}

// An inbound X-Request-Id is client-supplied, so it is not believed: a caller
// could otherwise collide ids with another caller, or reuse one across
// thousands of requests, and the correlation stops meaning anything.
func TestRun_InboundRequestIDIsIgnored(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/memstore/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "forged-by-the-client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET whoami: %v", err)
	}
	resp.Body.Close()
	_ = stop()

	if strings.Contains(out.String(), "forged-by-the-client") {
		t.Errorf("the client's request id reached the log:\n%s", out.String())
	}
}

// TestRun_NothingUsesTheStandardLog is what the conversion bought: with every
// package logging through slog, the daemon's output has no unleveled lines in
// it, and the bridge that used to guess a level from each line's text is gone.
// A new log.Printf anywhere in the daemon's reach fails here rather than
// arriving in Loki as detected_level=unknown.
func TestRun_NothingUsesTheStandardLog(t *testing.T) {
	var stdlog bytes.Buffer
	prevFlags := log.Flags()
	log.SetOutput(&stdlog)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(prevFlags)
	})

	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext", "--api-key", "test-key")
	addr, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()

	// Exercise the paths that used to log through the standard package: a
	// search (store and search-path lines) and an authenticated request.
	body := strings.NewReader(`{"query":"anything","limit":3}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/memstore/v1/search", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST search: %v", err)
	}
	resp.Body.Close()
	_ = stop()

	if stdlog.Len() != 0 {
		t.Errorf("something logged through the standard log package:\n%s", stdlog.String())
	}
	if out.String() == "" {
		t.Error("the daemon logged nothing at all, so this proves nothing")
	}
}

// The component tag is how a line is attributed once several subsystems share
// one logger.
func TestRun_LinesCarryTheirComponent(t *testing.T) {
	args := append(commonArgs(t), "--tls-disabled", "--insecure-plaintext")
	_, out, stop := startDaemonLogging(t, args)
	defer func() { _ = stop() }()
	_ = stop()

	// The embed queue runs on a timer from startup and the store logs on
	// demand, so the reliable assertion is that whatever components did log
	// named themselves; component= must never appear empty.
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "component=\"\"") || strings.Contains(line, "component= ") {
			t.Errorf("line with an empty component: %s", line)
		}
	}
}
