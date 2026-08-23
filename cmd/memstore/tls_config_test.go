package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendTLSConfigKeysSkipsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# comment\n\ntls_ca_file = \"/old/ca.pem\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	written, err := appendTLSConfigKeys(path, map[string]string{
		"tls_ca_file":   "/new/ca.pem",
		"tls_cert_file": "/new/client.pem",
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	if len(written) != 1 || written[0] != "tls_cert_file" {
		t.Errorf("written = %v, want [tls_cert_file] only", written)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "tls_ca_file"); got != 1 {
		t.Errorf("tls_ca_file appears %d times, want 1; the existing key was duplicated", got)
	}
}

// A scanner that stops early leaves the existing-keys map partial, and a
// partial map makes the caller append a key that is already in the file --
// silently duplicating it. bufio.Scanner reports a line over its 64KB buffer
// as an error rather than a short read, so the error has to be checked before
// the map is trusted.
func TestAppendTLSConfigKeysFailsOnUnreadableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	// One line past the scanner's limit, then a key that a partial read
	// would never reach.
	var b strings.Builder
	b.WriteString(strings.Repeat("x", bufio.MaxScanTokenSize+1))
	b.WriteString("\ntls_ca_file = \"/existing/ca.pem\"\n")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := appendTLSConfigKeys(path, map[string]string{"tls_ca_file": "/new/ca.pem"})
	if err == nil {
		t.Fatal("append succeeded on a config it could not fully read; tls_ca_file would be duplicated")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "tls_ca_file"); got != 1 {
		t.Errorf("tls_ca_file appears %d times, want 1; the file was modified despite the read failing", got)
	}
}
