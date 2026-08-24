package memstore

import (
	"fmt"
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

// LoadIngestToken reads only its own key, with the env override winning.
// It is deliberately separate from LoadConfig so that memstore-mcp, which
// loads AppConfig, never holds the ingest credential.
func TestLoadIngestToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MEMSTORE_INGEST_TOKEN", "")

	if got := LoadIngestToken(); got != "" {
		t.Errorf("no config file: token = %q, want empty", got)
	}

	cfgDir := filepath.Join(dir, "memstore")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "api_key = \"shared-key\"\ningest_token = \"file-ingest-token\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := LoadIngestToken(); got != "file-ingest-token" {
		t.Errorf("token from file = %q", got)
	}
	// The shared config loader must NOT pick the ingest token up anywhere.
	cfg := LoadConfig()
	if cfg.APIKey != "shared-key" {
		t.Errorf("api_key = %q", cfg.APIKey)
	}
	if s := fmt.Sprintf("%+v", redactedAppConfig(cfg)); strings.Contains(s, "ingest") || strings.Contains(s, "file-ingest-token") {
		t.Errorf("AppConfig carries ingest material: %s", s)
	}

	t.Setenv("MEMSTORE_INGEST_TOKEN", "env-ingest-token")
	if got := LoadIngestToken(); got != "env-ingest-token" {
		t.Errorf("env override = %q", got)
	}
}

func TestLoadConfig_PGSecretEnv(t *testing.T) {
	const dsn = "postgres://memstore:pw@db:5432/memstore?sslmode=disable"

	t.Run("new name", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		clearMemstoreEnv(t)
		t.Setenv("MEMSTORE_PG_SECRET", dsn)
		if got := LoadConfig().PG; got != dsn {
			t.Errorf("PG = %q, want %q", got, dsn)
		}
	})

	t.Run("deprecated name still loads", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		clearMemstoreEnv(t)
		t.Setenv("MEMSTORE_PG", dsn)
		if got := LoadConfig().PG; got != dsn {
			t.Errorf("PG = %q, want %q (old name must keep working)", got, dsn)
		}
	})

	t.Run("new name wins over deprecated", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		clearMemstoreEnv(t)
		t.Setenv("MEMSTORE_PG", "postgres://memstore:pw@old:5432/memstore")
		t.Setenv("MEMSTORE_PG_SECRET", dsn)
		if got := LoadConfig().PG; got != dsn {
			t.Errorf("PG = %q, want %q", got, dsn)
		}
	})
}

func TestLoadConfig_PGSecretFileKey(t *testing.T) {
	const dsn = "postgres://memstore:pw@db:5432/memstore"

	// Both orderings: pg_secret must win regardless of which line comes first.
	for name, content := range map[string]string{
		"pg_secret only": "pg_secret = \"" + dsn + "\"\n",
		"legacy pg only": "pg = \"" + dsn + "\"\n",
		"secret first":   "pg_secret = \"" + dsn + "\"\npg = \"postgres://memstore:pw@old:5432/x\"\n",
		"legacy first":   "pg = \"postgres://memstore:pw@old:5432/x\"\npg_secret = \"" + dsn + "\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			clearMemstoreEnv(t)
			configDir := filepath.Join(dir, "memstore")
			if err := os.MkdirAll(configDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if got := LoadConfig().PG; got != dsn {
				t.Errorf("PG = %q, want %q", got, dsn)
			}
		})
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{
			"password masked, host and db preserved",
			"postgres://memstore:hunter2@db:5432/memstore?sslmode=disable",
			"postgres://memstore:[redacted]@db:5432/memstore?sslmode=disable",
		},
		{
			// The live homelab password contains '+', so this is the shape that
			// actually has to work, not a hypothetical.
			"password with url-unsafe characters",
			"postgres://memstore:aB+cD9=@db:5432/memstore?sslmode=disable",
			"postgres://memstore:[redacted]@db:5432/memstore?sslmode=disable",
		},
		{
			// A '/' in the password makes the DSN unparseable. Redacting the
			// whole string is the safe answer: failing to parse is not evidence
			// that there is no password in there.
			"unparseable password is redacted whole",
			"postgres://memstore:a+b/c=@db:5432/memstore",
			"[redacted]",
		},
		{"no password, left alone", "postgres://memstore@db:5432/memstore", "postgres://memstore@db:5432/memstore"},
		{"no credentials at all", "postgres://db:5432/memstore", "postgres://db:5432/memstore"},
		{"unparseable is redacted whole", "postgres://memstore:pw@db:70000/x\x7f", "[redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactDSN(tt.in); got != tt.want {
				t.Errorf("RedactDSN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestAppConfigString_RedactsPG(t *testing.T) {
	c := AppConfig{PG: "postgres://memstore:hunter2@db:5432/memstore"}
	s := c.String()
	if strings.Contains(s, "hunter2") {
		t.Errorf("AppConfig.String() leaked the DSN password: %s", s)
	}
	if !strings.Contains(s, "db:5432") {
		t.Errorf("AppConfig.String() should keep the host for debugging: %s", s)
	}
}

// The refusal message promises MEMSTORE_INSECURE_PLAINTEXT works. It has to,
// or an operator following the daemon's own instructions gets nowhere -- and
// containerised deployments configure by environment, not flags.
func TestInsecurePlaintextFromEnvAndFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MEMSTORE_INSECURE_PLAINTEXT", "true")
	if cfg := LoadConfig(); !cfg.InsecurePlaintext {
		t.Error("from env: InsecurePlaintext not set")
	}

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MEMSTORE_INSECURE_PLAINTEXT", "")
	if err := os.MkdirAll(filepath.Join(dir, "memstore"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memstore", "config.toml"),
		[]byte("insecure_plaintext = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg := LoadConfig(); !cfg.InsecurePlaintext {
		t.Error("from file: InsecurePlaintext not set")
	}
}
