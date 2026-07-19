package memstore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AppConfig holds persistent defaults for the memstore CLI and MCP server.
// Values are loaded from the config file and can be overridden by CLI flags.
//
// Embedding configuration is NOT in this struct — it is read from the
// MEMSTORE_EMBED_* / EMBEDDING_* environment variables by go-embedding.
type AppConfig struct {
	DB        string
	Namespace string
	Ollama    string // chat LLM base URL (used by OpenAIGenerator)
	GenModel  string
	GenURL    string // separate LLM URL for generation (defaults to Ollama if empty)
	Remote    string // memstored URL; if set, use daemon mode instead of local SQLite
	APIKey    string // API key for memstored auth
	LLMAPIKey string // API key for the chat LLM provider (LiteLLM, OpenAI, etc.)
	Addr      string // listen address for memstored daemon
	PG        string // PostgreSQL connection string; if set, use Postgres instead of SQLite
	VecDim    int    // embedding vector dimension for Postgres (e.g. 768)

	// TLS configuration for memstored (server side). The daemon requires TLS
	// by default; TLSDisabled is the explicit opt-out for proxy-fronted
	// deployments.
	TLSCertFile     string // PEM-encoded server certificate
	TLSKeyFile      string // PEM-encoded server private key
	TLSClientCAFile string // PEM bundle of CAs trusted for client certs; presence enables mTLS
	TLSDisabled     bool   // listen plaintext (insecure)

	// Injection screening. Every write is screened by regex regardless of these;
	// they configure the model pass and the thresholds.
	//
	// ScreenMode is off | observe | gate, and defaults to off. Anything else requires
	// a deployment that actually runs the screening worker -- the daemon. See
	// ScreenMode for what each mode costs.
	ScreenMode        string // off | observe | gate
	ScreenThreat      int    // model threat score (0-10) at which a write is blocked (gate mode)
	ScreenDetectScore int    // detect score (0-100) at which the inline regex screen rejects
	ScreenConcurrency int    // simultaneous model screens
	ScreenBatch       int    // pending facts claimed per tick
	ScreenIntervalSec int    // seconds between worker ticks
	ScreenMaxAttempts int    // failed screens before a fact is abandoned

	// TLS configuration for memstore CLI / MCP (client side).
	TLSCAFile         string // PEM bundle to trust for the server cert (in addition to system roots)
	TLSClientCertFile string // PEM cert presented to memstored when mTLS is required
	TLSClientKeyFile  string // matching private key
}

// redactedAppConfig has AppConfig's fields but not its String method, so String
// can format it without recursing.
type redactedAppConfig AppConfig

// String implements fmt.Stringer so that printing a config -- in a log line, a
// test failure, a debug dump -- cannot leak its secrets. fmt routes %v and %+v
// through String, so this covers the accidental prints, which are the ones that
// matter: nobody deliberately writes an API key to a terminal.
func (c AppConfig) String() string {
	c.APIKey = redactSecret(c.APIKey)
	c.LLMAPIKey = redactSecret(c.LLMAPIKey)
	return fmt.Sprintf("%+v", redactedAppConfig(c))
}

// redactSecret masks a secret while preserving whether one was set at all, which
// is usually the thing being debugged.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// DefaultConfig returns the built-in defaults used when no config file exists.
func DefaultConfig() AppConfig {
	return AppConfig{
		DB:        defaultDBPath(),
		Namespace: "default",
		Ollama:    "http://localhost:11434",

		// Screening: model pass off, but its parameters carry real defaults so that
		// turning the switch on does not also require picking six numbers.
		//
		// The thresholds are guesses. Nothing here is calibrated against a real
		// corpus, which is what `memstore scan` exists to fix -- run it before
		// trusting ScreenThreat.
		ScreenMode:        string(ScreenModeOff),
		ScreenThreat:      6,
		ScreenDetectScore: 80,
		// The gemma-chat pool behind olla round-robins across several backends, so a
		// handful of concurrent screens spreads over distinct GPUs rather than
		// queueing on one. Kept modest because memstored shares that pool with
		// extraction and summarization, which are interactive-ish and should not be
		// starved by a backfill.
		ScreenConcurrency: 4,
		ScreenBatch:       16,
		ScreenIntervalSec: 30,
		ScreenMaxAttempts: 5,
	}
}

// ConfigPath returns the path to the config file, following the XDG Base
// Directory Specification: $XDG_CONFIG_HOME/memstore/config.toml
// (default ~/.config/memstore/config.toml).
func ConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "memstore", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "memstore", "config.toml")
}

// LoadConfig reads the config file and merges it with defaults. Missing keys
// retain their default values. If the config file does not exist or cannot be
// read, the defaults are returned without error.
func LoadConfig() AppConfig {
	cfg := DefaultConfig()

	// Parse config file if present.
	if path := ConfigPath(); path != "" {
		if f, err := os.Open(path); err == nil {
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				key, value, ok := parseConfigLine(line)
				if !ok {
					continue
				}
				switch key {
				case "db":
					cfg.DB = expandTilde(value)
				case "namespace":
					cfg.Namespace = value
				case "ollama":
					cfg.Ollama = value
				case "gen_model":
					cfg.GenModel = value
				case "gen_url":
					cfg.GenURL = value
				case "remote":
					cfg.Remote = value
				case "api_key":
					cfg.APIKey = value
				case "llm_api_key":
					cfg.LLMAPIKey = value
				case "addr":
					cfg.Addr = value
				case "pg":
					cfg.PG = value
				case "tls_cert_file":
					cfg.TLSCertFile = expandTilde(value)
				case "tls_key_file":
					cfg.TLSKeyFile = expandTilde(value)
				case "tls_client_ca_file":
					cfg.TLSClientCAFile = expandTilde(value)
				case "screen_mode":
					cfg.ScreenMode = value
				case "screen_threat":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenThreat = n
					}
				case "screen_detect_score":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenDetectScore = n
					}
				case "screen_concurrency":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenConcurrency = n
					}
				case "screen_batch":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenBatch = n
					}
				case "screen_interval_seconds":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenIntervalSec = n
					}
				case "screen_max_attempts":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenMaxAttempts = n
					}
				case "tls_disabled":
					if b, err := strconv.ParseBool(value); err == nil {
						cfg.TLSDisabled = b
					}
				case "tls_ca_file":
					cfg.TLSCAFile = expandTilde(value)
				case "tls_client_cert_file":
					cfg.TLSClientCertFile = expandTilde(value)
				case "tls_client_key_file":
					cfg.TLSClientKeyFile = expandTilde(value)
				}
			}
			f.Close()
		}
	}

	// Environment variables override config file values.
	// This enables Docker/container configuration via env.
	if v := os.Getenv("MEMSTORE_DB"); v != "" {
		cfg.DB = expandTilde(v)
	}
	if v := os.Getenv("MEMSTORE_NAMESPACE"); v != "" {
		cfg.Namespace = v
	}
	if v := os.Getenv("MEMSTORE_OLLAMA"); v != "" {
		cfg.Ollama = v
	}
	if v := os.Getenv("MEMSTORE_GEN_MODEL"); v != "" {
		cfg.GenModel = v
	}
	if v := os.Getenv("MEMSTORE_GEN_URL"); v != "" {
		cfg.GenURL = v
	}
	if v := os.Getenv("MEMSTORE_REMOTE"); v != "" {
		cfg.Remote = v
	}
	if v := os.Getenv("MEMSTORE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("MEMSTORE_LLM_API_KEY"); v != "" {
		cfg.LLMAPIKey = v
	}
	if v := os.Getenv("MEMSTORE_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("MEMSTORE_PG"); v != "" {
		cfg.PG = v
	}
	if v := os.Getenv("MEMSTORE_TLS_CERT_FILE"); v != "" {
		cfg.TLSCertFile = expandTilde(v)
	}
	if v := os.Getenv("MEMSTORE_TLS_KEY_FILE"); v != "" {
		cfg.TLSKeyFile = expandTilde(v)
	}
	if v := os.Getenv("MEMSTORE_TLS_CLIENT_CA_FILE"); v != "" {
		cfg.TLSClientCAFile = expandTilde(v)
	}
	if v := os.Getenv("MEMSTORE_SCREEN_MODE"); v != "" {
		cfg.ScreenMode = v
	}
	for _, e := range []struct {
		env string
		dst *int
	}{
		{"MEMSTORE_SCREEN_THREAT", &cfg.ScreenThreat},
		{"MEMSTORE_SCREEN_DETECT_SCORE", &cfg.ScreenDetectScore},
		{"MEMSTORE_SCREEN_CONCURRENCY", &cfg.ScreenConcurrency},
		{"MEMSTORE_SCREEN_BATCH", &cfg.ScreenBatch},
		{"MEMSTORE_SCREEN_INTERVAL_SECONDS", &cfg.ScreenIntervalSec},
		{"MEMSTORE_SCREEN_MAX_ATTEMPTS", &cfg.ScreenMaxAttempts},
	} {
		if v := os.Getenv(e.env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*e.dst = n
			}
		}
	}
	if v := os.Getenv("MEMSTORE_TLS_DISABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.TLSDisabled = b
		}
	}
	if v := os.Getenv("MEMSTORE_TLS_CA_FILE"); v != "" {
		cfg.TLSCAFile = expandTilde(v)
	}
	if v := os.Getenv("MEMSTORE_TLS_CLIENT_CERT_FILE"); v != "" {
		cfg.TLSClientCertFile = expandTilde(v)
	}
	if v := os.Getenv("MEMSTORE_TLS_CLIENT_KEY_FILE"); v != "" {
		cfg.TLSClientKeyFile = expandTilde(v)
	}

	return cfg
}

// parseConfigLine splits a line on the first '=' and strips whitespace and
// surrounding quotes from both key and value.
func parseConfigLine(line string) (key, value string, ok bool) {
	before, after, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(before)
	value = stripQuotes(strings.TrimSpace(after))
	return key, value, key != ""
}

// stripQuotes removes matching single or double quotes surrounding a string.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// expandTilde replaces a leading ~ with the user's home directory.
func expandTilde(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// defaultDBPath returns ~/.local/share/memstore/memory.db, following the
// XDG Base Directory Specification for user data.
func defaultDBPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "memstore", "memory.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "memory.db"
	}
	return filepath.Join(home, ".local", "share", "memstore", "memory.db")
}
