package main

import (
	"bufio"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/caetl"
)

// tlsPaths bundles the default file layout under a TLS directory.
type tlsPaths struct {
	dir       string
	caCert    string
	caKey     string
	srvCert   string
	srvKey    string
	clientDir string
}

func defaultTLSPaths(dir string) tlsPaths {
	return tlsPaths{
		dir:       dir,
		caCert:    filepath.Join(dir, "ca.pem"),
		caKey:     filepath.Join(dir, "ca.key"),
		srvCert:   filepath.Join(dir, "server.pem"),
		srvKey:    filepath.Join(dir, "server.key"),
		clientDir: filepath.Join(dir, "clients"),
	}
}

// defaultTLSDir returns ~/.config/memstore/tls (or under XDG_CONFIG_HOME).
func defaultTLSDir() (string, error) {
	cfgPath := memstore.ConfigPath()
	if cfgPath == "" {
		return "", errors.New("could not determine config path")
	}
	return filepath.Join(filepath.Dir(cfgPath), "tls"), nil
}

func runTLS(args []string) {
	if len(args) == 0 {
		printTLSUsage(os.Stderr)
		os.Exit(1)
	}
	switch args[0] {
	case "init":
		runTLSInit(args[1:], os.Stdout)
	case "issue-client":
		runTLSIssueClient(args[1:], os.Stdout)
	case "show":
		runTLSShow(args[1:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "tls: unknown subcommand %q\n", args[0])
		printTLSUsage(os.Stderr)
		os.Exit(1)
	}
}

func printTLSUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: memstore tls <subcommand> [flags]

Subcommands:
  init                    Generate a self-signed CA and server cert for memstored.
  issue-client <name>     Mint a client cert (CN=<name>) signed by the local CA.
  show                    Print fingerprints, expiries, and paths for the local CA + certs.

Default paths live under ~/.config/memstore/tls/. Override with --dir.`)
}

// --- init ---

func runTLSInit(args []string, out io.Writer) {
	fs := flag.NewFlagSet("tls init", flag.ExitOnError)
	dir := fs.String("dir", "", "TLS directory (default: ~/.config/memstore/tls)")
	caCN := fs.String("ca-cn", "memstore CA", "Subject CN for the CA cert")
	srvCN := fs.String("server-cn", "", "Subject CN for the server cert (default: hostname)")
	hosts := fs.String("hosts", "", "comma-separated SANs for the server cert (default: hostname,localhost,127.0.0.1)")
	caValidity := fs.Duration("ca-validity", caetl.CADefaultValidity, "CA validity")
	srvValidity := fs.Duration("server-validity", caetl.LeafDefaultValidity, "server cert validity")
	force := fs.Bool("force", false, "overwrite existing CA / server cert")
	skipConfig := fs.Bool("skip-config-update", false, "do not append cert paths to config.toml")
	fs.Parse(args)

	paths, err := resolvePaths(*dir)
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(paths.dir, 0700); err != nil {
		fail(fmt.Errorf("create %s: %w", paths.dir, err))
	}

	// Hosts default: hostname + localhost + 127.0.0.1.
	hostList := splitHosts(*hosts)
	if len(hostList) == 0 {
		hn, _ := os.Hostname()
		hostList = []string{"localhost", "127.0.0.1"}
		if hn != "" && hn != "localhost" {
			hostList = append([]string{hn}, hostList...)
		}
	}

	// Server CN default: first host (which is usually the machine hostname).
	serverCN := *srvCN
	if serverCN == "" {
		serverCN = hostList[0]
	}

	// CA: load or create.
	var ca *caetl.CA
	if fileExists(paths.caCert) && fileExists(paths.caKey) {
		if !*force {
			ca, err = caetl.LoadCA(paths.caCert, paths.caKey)
			if err != nil {
				fail(fmt.Errorf("load existing CA: %w", err))
			}
			fmt.Fprintf(out, "CA: reusing existing %s\n", paths.caCert)
		} else {
			ca, err = caetl.NewCA(*caCN, *caValidity)
			if err != nil {
				fail(err)
			}
			if err := ca.Save(paths.caCert, paths.caKey); err != nil {
				fail(err)
			}
			fmt.Fprintf(out, "CA: regenerated %s (--force)\n", paths.caCert)
		}
	} else {
		ca, err = caetl.NewCA(*caCN, *caValidity)
		if err != nil {
			fail(err)
		}
		if err := ca.Save(paths.caCert, paths.caKey); err != nil {
			fail(err)
		}
		fmt.Fprintf(out, "CA: created %s\n", paths.caCert)
	}

	// Server cert: refuse to overwrite without --force.
	if fileExists(paths.srvCert) && !*force {
		fail(fmt.Errorf("%s already exists (use --force to overwrite)", paths.srvCert))
	}
	srv, err := ca.IssueServer(serverCN, hostList, *srvValidity)
	if err != nil {
		fail(err)
	}
	if err := srv.Save(paths.srvCert, paths.srvKey); err != nil {
		fail(err)
	}
	fmt.Fprintf(out, "Server cert: %s (CN=%s, hosts=%s)\n", paths.srvCert, serverCN, strings.Join(hostList, ","))

	// Update config.toml unless told not to.
	if !*skipConfig {
		written, err := appendTLSConfigKeys(memstore.ConfigPath(), map[string]string{
			"tls_cert_file":      paths.srvCert,
			"tls_key_file":       paths.srvKey,
			"tls_client_ca_file": paths.caCert,
		})
		if err != nil {
			fmt.Fprintf(out, "WARNING: could not update config.toml: %v\n", err)
		} else if len(written) > 0 {
			fmt.Fprintf(out, "Config: appended %s to %s\n", strings.Join(written, ", "), memstore.ConfigPath())
		} else {
			fmt.Fprintln(out, "Config: TLS keys already present, leaving config.toml untouched")
		}
	}

	// Closing instructions — third parties land here on first run.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  1. Restart memstored. It will pick up the cert paths from config.toml.")
	fmt.Fprintln(out, "  2. Until httpclient learns to read tls_ca_file, point the system trust at this CA")
	fmt.Fprintf(out, "     when running the memstore CLI / MCP, e.g.:\n")
	fmt.Fprintf(out, "       export SSL_CERT_FILE=%s\n", paths.caCert)
	fmt.Fprintln(out, "  3. To issue a client cert (for mTLS, when wired up):")
	fmt.Fprintln(out, "       memstore tls issue-client <name>")
}

// --- issue-client ---

func runTLSIssueClient(args []string, out io.Writer) {
	fs := flag.NewFlagSet("tls issue-client", flag.ExitOnError)
	dir := fs.String("dir", "", "TLS directory (default: ~/.config/memstore/tls)")
	outDir := fs.String("out", "", "output directory for the client bundle (default: <dir>/clients)")
	validity := fs.Duration("validity", caetl.LeafDefaultValidity, "client cert validity")
	force := fs.Bool("force", false, "overwrite existing client cert")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "tls issue-client: expected exactly one positional argument <name>")
		os.Exit(1)
	}
	name := fs.Arg(0)
	if !validClientName(name) {
		fail(fmt.Errorf("invalid client name %q (allowed: letters, digits, '-', '_', '.')", name))
	}

	paths, err := resolvePaths(*dir)
	if err != nil {
		fail(err)
	}
	ca, err := caetl.LoadCA(paths.caCert, paths.caKey)
	if err != nil {
		fail(fmt.Errorf("load CA: %w (run 'memstore tls init' first)", err))
	}

	bundleDir := *outDir
	if bundleDir == "" {
		bundleDir = paths.clientDir
	}
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		fail(fmt.Errorf("create %s: %w", bundleDir, err))
	}

	certPath := filepath.Join(bundleDir, name+".pem")
	keyPath := filepath.Join(bundleDir, name+".key")
	if (fileExists(certPath) || fileExists(keyPath)) && !*force {
		fail(fmt.Errorf("%s or %s exists (use --force to overwrite)", certPath, keyPath))
	}

	pair, err := ca.IssueClient(name, *validity)
	if err != nil {
		fail(err)
	}
	if err := pair.Save(certPath, keyPath); err != nil {
		fail(err)
	}

	fmt.Fprintf(out, "Issued client cert for %q\n", name)
	fmt.Fprintf(out, "  cert:    %s\n", certPath)
	fmt.Fprintf(out, "  key:     %s\n", keyPath)
	fmt.Fprintf(out, "  expires: %s\n", pair.Cert.NotAfter.Format(time.RFC3339))
	fmt.Fprintf(out, "  fingerprint (SHA-256): %s\n", caetl.Fingerprint(pair.Cert))
}

// --- show ---

func runTLSShow(args []string, out io.Writer) {
	fs := flag.NewFlagSet("tls show", flag.ExitOnError)
	dir := fs.String("dir", "", "TLS directory (default: ~/.config/memstore/tls)")
	fs.Parse(args)

	paths, err := resolvePaths(*dir)
	if err != nil {
		fail(err)
	}

	fmt.Fprintf(out, "TLS directory: %s\n", paths.dir)

	if !fileExists(paths.caCert) {
		fmt.Fprintln(out, "  no CA cert found — run 'memstore tls init'")
		os.Exit(1)
	}
	ca, err := caetl.LoadCA(paths.caCert, paths.caKey)
	if err != nil {
		fail(err)
	}
	printCertSummary(out, "CA", paths.caCert, ca.Cert)

	if fileExists(paths.srvCert) {
		srv, err := caetl.LoadPair(paths.srvCert, paths.srvKey)
		if err != nil {
			fmt.Fprintf(out, "WARNING: server cert load failed: %v\n", err)
		} else {
			printCertSummary(out, "Server", paths.srvCert, srv.Cert)
		}
	}

	// Client bundles, if any.
	if entries, err := os.ReadDir(paths.clientDir); err == nil && len(entries) > 0 {
		fmt.Fprintln(out, "Clients:")
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".pem") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".pem")
			certPath := filepath.Join(paths.clientDir, e.Name())
			keyPath := filepath.Join(paths.clientDir, name+".key")
			pair, err := caetl.LoadPair(certPath, keyPath)
			if err != nil {
				fmt.Fprintf(out, "  %s: load failed (%v)\n", name, err)
				continue
			}
			fmt.Fprintf(out, "  %-20s expires=%s fp=%s\n",
				name, pair.Cert.NotAfter.Format("2006-01-02"), caetl.Fingerprint(pair.Cert))
		}
	}
}

func printCertSummary(w io.Writer, label, path string, cert *x509.Certificate) {
	fmt.Fprintf(w, "%s: %s\n", label, path)
	fmt.Fprintf(w, "  CN:          %s\n", cert.Subject.CommonName)
	fmt.Fprintf(w, "  expires:     %s\n", cert.NotAfter.Format(time.RFC3339))
	fmt.Fprintf(w, "  fingerprint: %s\n", caetl.Fingerprint(cert))
	if len(cert.DNSNames) > 0 || len(cert.IPAddresses) > 0 {
		var sans []string
		sans = append(sans, cert.DNSNames...)
		for _, ip := range cert.IPAddresses {
			sans = append(sans, ip.String())
		}
		fmt.Fprintf(w, "  SANs:        %s\n", strings.Join(sans, ", "))
	}
}

// --- helpers ---

func resolvePaths(dirFlag string) (tlsPaths, error) {
	dir := dirFlag
	if dir == "" {
		d, err := defaultTLSDir()
		if err != nil {
			return tlsPaths{}, err
		}
		dir = d
	}
	return defaultTLSPaths(dir), nil
}

func splitHosts(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// validClientName limits client cert CNs to a conservative character set so
// they're safe to use as filenames and shell arguments.
func validClientName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// appendTLSConfigKeys appends key=value lines to config.toml for keys not
// already present. Returns the keys that were written.
func appendTLSConfigKeys(path string, kv map[string]string) ([]string, error) {
	if path == "" {
		return nil, errors.New("config path unknown")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	existing := map[string]bool{}
	f, err := os.Open(path)
	switch {
	case err == nil:
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if eq := strings.IndexByte(line, '='); eq > 0 {
				key := strings.TrimSpace(line[:eq])
				existing[key] = true
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// fine — we'll create it
	default:
		return nil, err
	}

	var written []string
	var b strings.Builder
	// Stable order for predictable output.
	for _, key := range []string{"tls_cert_file", "tls_key_file", "tls_client_ca_file"} {
		val, ok := kv[key]
		if !ok || existing[key] {
			continue
		}
		fmt.Fprintf(&b, "%s = %q\n", key, val)
		written = append(written, key)
	}
	if b.Len() == 0 {
		return nil, nil
	}

	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	// Make sure we start on a fresh line.
	if st, _ := out.Stat(); st != nil && st.Size() > 0 {
		if _, err := out.Write([]byte("\n# Generated by 'memstore tls init'\n")); err != nil {
			return nil, err
		}
	} else {
		if _, err := out.Write([]byte("# Generated by 'memstore tls init'\n")); err != nil {
			return nil, err
		}
	}
	if _, err := out.WriteString(b.String()); err != nil {
		return nil, err
	}
	return written, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
