package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/matthewjhunter/memstore"
)

// ClientOptions configures TLS behavior for outbound requests to memstored.
//
// TLSCAFile is added to the system trust store (not a replacement). Use it
// to trust a self-signed memstored cert without modifying the OS root store.
//
// TLSClientCertFile / TLSClientKeyFile are presented during the TLS handshake
// when memstored is configured with --tls-client-ca-file (mTLS). Both must
// be set together or neither.
type ClientOptions struct {
	TLSCAFile         string
	TLSClientCertFile string
	TLSClientKeyFile  string
}

// HasTLSConfig reports whether any TLS-related field is set.
func (o ClientOptions) HasTLSConfig() bool {
	return o.TLSCAFile != "" || o.TLSClientCertFile != "" || o.TLSClientKeyFile != ""
}

// ClientOptionsFromConfig pulls client-side TLS knobs out of an AppConfig.
func ClientOptionsFromConfig(cfg memstore.AppConfig) ClientOptions {
	return ClientOptions{
		TLSCAFile:         cfg.TLSCAFile,
		TLSClientCertFile: cfg.TLSClientCertFile,
		TLSClientKeyFile:  cfg.TLSClientKeyFile,
	}
}

// transportFor builds an http.RoundTripper configured per opts. Returns nil
// (signaling http.DefaultTransport) when no TLS config is set.
func transportFor(opts ClientOptions) (http.RoundTripper, error) {
	if !opts.HasTLSConfig() {
		return nil, nil
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if opts.TLSCAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pemBytes, err := os.ReadFile(opts.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA file %s: %w", opts.TLSCAFile, err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no certificates in TLS CA file %s", opts.TLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}

	if opts.TLSClientCertFile != "" || opts.TLSClientKeyFile != "" {
		if opts.TLSClientCertFile == "" || opts.TLSClientKeyFile == "" {
			return nil, fmt.Errorf("mTLS requires both TLSClientCertFile and TLSClientKeyFile")
		}
		cert, err := tls.LoadX509KeyPair(opts.TLSClientCertFile, opts.TLSClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load mTLS client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return &http.Transport{TLSClientConfig: tlsCfg}, nil
}
