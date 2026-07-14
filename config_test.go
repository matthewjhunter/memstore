package memstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", cfg.Namespace, "default")
	}
	if cfg.Ollama != "http://localhost:11434" {
		t.Errorf("Ollama = %q, want %q", cfg.Ollama, "http://localhost:11434")
	}
	if cfg.GenModel != "" {
		t.Errorf("GenModel = %q, want empty", cfg.GenModel)
	}
	if cfg.DB == "" {
		t.Error("DB should not be empty")
	}
}

// clearMemstoreEnv unsets every MEMSTORE_* variable for the duration of a test.
// LoadConfig lets the environment override both file and defaults, so a developer
// who exports MEMSTORE_REMOTE or MEMSTORE_API_KEY in their shell would otherwise
// see these tests fail against their own environment rather than the fixture.
func clearMemstoreEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "MEMSTORE_") {
			t.Setenv(k, "")
		}
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearMemstoreEnv(t)
	cfg := LoadConfig()
	want := DefaultConfig()
	if cfg != want {
		t.Errorf("LoadConfig with missing file = %+v, want defaults %+v", cfg, want)
	}
}

func TestLoadConfig_ParsesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `# Memstore configuration
db = "/tmp/test.db"
namespace = "prod"
ollama = "http://remote:11434"
gen_model = "llama3"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()

	if cfg.DB != "/tmp/test.db" {
		t.Errorf("DB = %q, want %q", cfg.DB, "/tmp/test.db")
	}
	if cfg.Namespace != "prod" {
		t.Errorf("Namespace = %q, want %q", cfg.Namespace, "prod")
	}
	if cfg.Ollama != "http://remote:11434" {
		t.Errorf("Ollama = %q, want %q", cfg.Ollama, "http://remote:11434")
	}
	if cfg.GenModel != "llama3" {
		t.Errorf("GenModel = %q, want %q", cfg.GenModel, "llama3")
	}
}

func TestLoadConfig_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `gen_model = "qwen2.5:7b"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	defaults := DefaultConfig()

	if cfg.GenModel != "qwen2.5:7b" {
		t.Errorf("GenModel = %q, want %q", cfg.GenModel, "qwen2.5:7b")
	}
	if cfg.DB != defaults.DB {
		t.Errorf("DB = %q, want default %q", cfg.DB, defaults.DB)
	}
	if cfg.Namespace != defaults.Namespace {
		t.Errorf("Namespace = %q, want default %q", cfg.Namespace, defaults.Namespace)
	}
}

func TestLoadConfig_QuotedValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `namespace = "staging"
ollama = 'http://gpu:11434'
gen_model = unquoted
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()

	if cfg.Namespace != "staging" {
		t.Errorf("Namespace = %q, want %q", cfg.Namespace, "staging")
	}
	if cfg.Ollama != "http://gpu:11434" {
		t.Errorf("Ollama = %q, want %q", cfg.Ollama, "http://gpu:11434")
	}
	if cfg.GenModel != "unquoted" {
		t.Errorf("GenModel = %q, want %q", cfg.GenModel, "unquoted")
	}
}

func TestLoadConfig_TildeExpansion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `db = "~/data/memstore.db"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "data/memstore.db")
	if cfg.DB != want {
		t.Errorf("DB = %q, want %q", cfg.DB, want)
	}
}

func TestLoadConfig_CommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `
# This is a comment
   # Indented comment

namespace = "test"

# Another comment
gen_model = "test-gen-model"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()

	if cfg.Namespace != "test" {
		t.Errorf("Namespace = %q, want %q", cfg.Namespace, "test")
	}
	if cfg.GenModel != "test-gen-model" {
		t.Errorf("GenModel = %q, want %q", cfg.GenModel, "test-gen-model")
	}
}

func TestConfigPath_XDGOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	got := ConfigPath()
	want := "/custom/config/memstore/config.toml"
	if got != want {
		t.Errorf("ConfigPath = %q, want %q", got, want)
	}
}

func TestConfigPath_Default(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	got := ConfigPath()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "memstore", "config.toml")
	if got != want {
		t.Errorf("ConfigPath = %q, want %q", got, want)
	}
}

func TestParseConfigLine(t *testing.T) {
	tests := []struct {
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{`key = value`, "key", "value", true},
		{`key="value"`, "key", "value", true},
		{`key = "value with = sign"`, "key", "value with = sign", true},
		{`  key  =  value  `, "key", "value", true},
		{`no_equals`, "", "", false},
		{`= no_key`, "", "no_key", false},
	}

	for _, tt := range tests {
		key, value, ok := parseConfigLine(tt.line)
		if key != tt.wantKey || value != tt.wantValue || ok != tt.wantOK {
			t.Errorf("parseConfigLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.line, key, value, ok, tt.wantKey, tt.wantValue, tt.wantOK)
		}
	}
}

func TestLoadConfig_EnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `namespace = "from-file"
gen_model = "from-file-gen-model"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEMSTORE_NAMESPACE", "from-env")
	t.Setenv("MEMSTORE_REMOTE", "http://memstored:8230")
	t.Setenv("MEMSTORE_API_KEY", "secret")
	t.Setenv("MEMSTORE_ADDR", "0.0.0.0:9999")

	cfg := LoadConfig()

	if cfg.Namespace != "from-env" {
		t.Errorf("Namespace = %q, want %q (env should override file)", cfg.Namespace, "from-env")
	}
	if cfg.GenModel != "from-file-gen-model" {
		t.Errorf("GenModel = %q, want %q (file value should persist when no env set)", cfg.GenModel, "from-file-gen-model")
	}
	if cfg.Remote != "http://memstored:8230" {
		t.Errorf("Remote = %q, want %q", cfg.Remote, "http://memstored:8230")
	}
	if cfg.APIKey != "secret" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "secret")
	}
	if cfg.Addr != "0.0.0.0:9999" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "0.0.0.0:9999")
	}
}

func TestLoadConfig_EnvOverridesDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config file
	clearMemstoreEnv(t)
	t.Setenv("MEMSTORE_DB", "/data/memory.db")
	t.Setenv("MEMSTORE_OLLAMA", "http://gpu:11434")
	t.Setenv("MEMSTORE_GEN_MODEL", "qwen2.5:7b")

	cfg := LoadConfig()

	if cfg.DB != "/data/memory.db" {
		t.Errorf("DB = %q, want %q", cfg.DB, "/data/memory.db")
	}
	if cfg.Ollama != "http://gpu:11434" {
		t.Errorf("Ollama = %q, want %q", cfg.Ollama, "http://gpu:11434")
	}
	if cfg.GenModel != "qwen2.5:7b" {
		t.Errorf("GenModel = %q, want %q", cfg.GenModel, "qwen2.5:7b")
	}
}

func TestLoadConfig_TLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	clearMemstoreEnv(t)

	configDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	content := `tls_cert_file = "/etc/memstored/cert.pem"
tls_key_file = "/etc/memstored/key.pem"
tls_client_ca_file = "/etc/memstored/clients.pem"
tls_disabled = false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()

	if cfg.TLSCertFile != "/etc/memstored/cert.pem" {
		t.Errorf("TLSCertFile = %q", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/etc/memstored/key.pem" {
		t.Errorf("TLSKeyFile = %q", cfg.TLSKeyFile)
	}
	if cfg.TLSClientCAFile != "/etc/memstored/clients.pem" {
		t.Errorf("TLSClientCAFile = %q", cfg.TLSClientCAFile)
	}
	if cfg.TLSDisabled {
		t.Error("TLSDisabled should be false from file")
	}

	// Env overrides file.
	t.Setenv("MEMSTORE_TLS_DISABLED", "true")
	t.Setenv("MEMSTORE_TLS_CERT_FILE", "/run/secrets/cert.pem")
	cfg = LoadConfig()
	if !cfg.TLSDisabled {
		t.Error("MEMSTORE_TLS_DISABLED=true should override file")
	}
	if cfg.TLSCertFile != "/run/secrets/cert.pem" {
		t.Errorf("TLSCertFile env override = %q", cfg.TLSCertFile)
	}
}

func TestExpandTilde(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/foo", filepath.Join(home, "foo")},
		{"~/", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~", home},
	}

	for _, tt := range tests {
		got := expandTilde(tt.input)
		if got != tt.want {
			t.Errorf("expandTilde(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
