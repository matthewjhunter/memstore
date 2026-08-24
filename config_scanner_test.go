package memstore

import (
	"bufio"
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig points ConfigPath at a temp dir and writes body to the config
// file inside it.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "memstore", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })
	fn()
	return buf.String()
}

// A line past bufio.Scanner's 64KB buffer stops the scan. Everything after it
// is silently unread, so a config whose `remote` sits below such a line loads
// as if remote were unset -- and an unset remote makes memstore-mcp fall back
// to an empty local SQLite database rather than the daemon. That failure is
// indistinguishable from an empty corpus, so the read must at least say so.
func TestLoadConfigWarnsOnUnreadableFile(t *testing.T) {
	writeConfig(t, strings.Repeat("x", bufio.MaxScanTokenSize+1)+"\nremote = \"http://example:8230\"\n")

	var cfg AppConfig
	out := captureLog(t, func() { cfg = LoadConfig() })

	if out == "" {
		t.Error("LoadConfig read a truncated config silently; expected a warning on the log")
	}
	if !strings.Contains(out, "config") {
		t.Errorf("warning does not name the config file: %q", out)
	}
	// The contract stays "defaults on unreadable" -- this test pins the
	// warning, not a behaviour change.
	if cfg.Remote != "" {
		t.Errorf("remote = %q; the line after the overlong one is genuinely unread", cfg.Remote)
	}
}

func TestLoadIngestTokenWarnsOnUnreadableFile(t *testing.T) {
	writeConfig(t, strings.Repeat("x", bufio.MaxScanTokenSize+1)+"\ningest_token = \"mst_example\"\n")

	var tok string
	out := captureLog(t, func() { tok = LoadIngestToken() })

	if out == "" {
		t.Error("LoadIngestToken read a truncated config silently; expected a warning on the log")
	}
	if tok != "" {
		t.Errorf("token = %q; the line after the overlong one is genuinely unread", tok)
	}
}

// A normal config must stay quiet, or the warning becomes noise nobody reads.
func TestLoadConfigSilentOnGoodFile(t *testing.T) {
	writeConfig(t, "# comment\nremote = \"http://example:8230\"\nnamespace = \"probe\"\n")

	var cfg AppConfig
	out := captureLog(t, func() { cfg = LoadConfig() })

	if out != "" {
		t.Errorf("LoadConfig logged on a well-formed config: %q", out)
	}
	if cfg.Remote != "http://example:8230" {
		t.Errorf("remote = %q, want the configured value", cfg.Remote)
	}
	if cfg.Namespace != "probe" {
		t.Errorf("namespace = %q, want probe", cfg.Namespace)
	}
}

// A missing config file is the ordinary first-run case, not an error.
func TestLoadConfigSilentWhenMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	out := captureLog(t, func() { LoadConfig() })
	if out != "" {
		t.Errorf("LoadConfig logged when no config file exists: %q", out)
	}
}
