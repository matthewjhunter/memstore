package memstore

import (
	"bufio"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	Remote    string // memstored URL the CLI and hooks talk to
	APIKey    string // API key for memstored auth
	LLMAPIKey string // API key for the chat LLM provider (LiteLLM, OpenAI, etc.)
	Addr      string // listen address for memstored daemon
	// PG is the PostgreSQL connection string; if set, use Postgres instead of
	// SQLite. It is a SECRET: the DSN embeds the database password. It is named
	// pg_secret / MEMSTORE_PG_SECRET so that the ordinary secret-filtering
	// habit -- grep -v for pass|key|secret|token over an env dump -- catches it.
	// The old name, plain "pg" / MEMSTORE_PG, matched none of those and so read
	// as inert config; it leaked into a terminal that way. Both names still
	// load (see LoadConfig), but only the new one is documented.
	PG     string
	VecDim int // embedding vector dimension for Postgres (e.g. 768)

	// TLS configuration for memstored (server side). The daemon requires TLS
	// by default; TLSDisabled is the explicit opt-out for proxy-fronted
	// deployments.
	TLSCertFile     string // PEM-encoded server certificate
	TLSKeyFile      string // PEM-encoded server private key
	TLSClientCAFile string // PEM bundle of CAs trusted for client certs; presence enables mTLS
	TLSDisabled     bool   // listen plaintext (insecure)
	// InsecurePlaintext affirms that a plaintext listener is reachable only
	// over a trusted path. Required alongside TLSDisabled: the daemon cannot
	// tell a private container network from a routable one, so it asks once
	// rather than guessing. See checkTransport in cmd/memstored.
	InsecurePlaintext bool

	// Injection screening. Every write is screened by regex regardless of these;
	// they configure the model pass and the thresholds.
	//
	// ScreenMode is off | observe | gate, and defaults to off. Anything else requires
	// a deployment that actually runs the screening worker -- the daemon. See
	// ScreenMode for what each mode costs.
	ScreenMode        string // off | observe | gate
	ScreenThreat      int    // model threat score (0-10) at which a write is blocked (gate mode)
	ScreenDetectScore int    // detect score (0-100) at which the inline regex screen rejects

	// ScreenDetectWrite and ScreenDetectRead are allow | warn | block, defaulting to
	// block. They are separate because the two edges fail differently: a blocked
	// write returns an error the writer can act on by rephrasing, while a blocked
	// read simply withholds the memory. See ScreenDetectMode.
	ScreenDetectWrite string
	ScreenDetectRead  string

	// ScreenDetectReadScore is the score at which a read is withheld, defaulting to
	// DefaultDetectReadScore. Deliberately above ScreenDetectScore: a blocked read is
	// silent, so it should demand corroboration rather than a single rule.
	ScreenDetectReadScore int
	ScreenConcurrency     int // simultaneous model screens
	ScreenBatch           int // pending facts claimed per tick
	ScreenIntervalSec     int // seconds between worker ticks
	ScreenMaxAttempts     int // failed screens before a fact is abandoned

	// TLS configuration for memstore CLI / MCP (client side).
	TLSCAFile         string // PEM bundle to trust for the server cert (in addition to system roots)
	TLSClientCertFile string // PEM cert presented to memstored when mTLS is required
	TLSClientKeyFile  string // matching private key
}

// LoadIngestToken returns the dedicated ingest credential: the ingest_token
// config key, overridden by MEMSTORE_INGEST_TOKEN. It is deliberately NOT a
// field on AppConfig and NOT read by LoadConfig: memstore-mcp loads AppConfig,
// and if the ingest token rode along there, the model's process would hold the
// exact credential the ingest scope split exists to withhold
// (docs/document-ingest.md). Only `memstore ingest` calls this.
func LoadIngestToken() string {
	token := ""
	if path := ConfigPath(); path != "" {
		if f, err := os.Open(path); err == nil {
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if key, value, ok := parseConfigLine(line); ok && key == "ingest_token" {
					token = value
				}
			}
			warnIfUnreadable(scanner, path)
			f.Close()
		}
	}
	if v := os.Getenv("MEMSTORE_INGEST_TOKEN"); v != "" {
		token = v
	}
	return token
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
	c.PG = RedactDSN(c.PG)
	return fmt.Sprintf("%+v", redactedAppConfig(c))
}

// RedactDSN masks the password in a PostgreSQL DSN while leaving the parts an
// operator actually debugs -- scheme, user, host, database, options -- intact.
// Blanket-redacting the whole string would hide "am I pointed at the right
// host?", which is the usual question, and so would invite people to print the
// raw DSN instead.
//
// It is exported because the DSN is passed around outside AppConfig (flags,
// admin commands), and every one of those paths needs the same treatment.
// A DSN that will not parse is redacted whole: an unparseable string is not
// proof there is no password in it.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "[redacted]"
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return dsn
	}
	u.User = url.UserPassword(u.User.Username(), "[redacted]")
	// url.String re-encodes the masked password; undo that so the marker stays
	// readable rather than appearing as %5Bredacted%5D.
	return strings.Replace(u.String(), "%5Bredacted%5D", "[redacted]", 1)
}

// redactSecret masks a secret while preserving whether one was set at all, which
// is usually the thing being debugged.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// pgEnvWarnOnce keeps the deprecation notice to one line per process. LoadConfig
// is called from memstore-mcp, whose stderr is the MCP client's log: a warning
// per load would be noise, and noisy warnings get filtered out wholesale, taking
// this one with them.
var pgEnvWarnOnce sync.Once

// warnDeprecatedPGEnv notes that the deployment is still on the old env-var
// name. Deliberately to stderr and never to stdout: stdout carries MCP JSON-RPC.
func warnDeprecatedPGEnv() {
	pgEnvWarnOnce.Do(func() {
		fmt.Fprintln(os.Stderr,
			"memstore: MEMSTORE_PG is deprecated, use MEMSTORE_PG_SECRET "+
				"(the DSN holds a password; the name should say so)")
	})
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
		ScreenMode:            string(ScreenModeOff),
		ScreenThreat:          6,
		ScreenDetectScore:     80,
		ScreenDetectWrite:     string(ScreenDetectBlock),
		ScreenDetectRead:      string(ScreenDetectBlock),
		ScreenDetectReadScore: DefaultDetectReadScore,
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
				case "pg_secret":
					cfg.PG = value
				case "pg":
					// Deprecated spelling, still honored. See AppConfig.PG.
					// No warning here: unlike the env var, a config file key is
					// not something a shell dump splatters into a transcript,
					// and warning on every load would spam MCP stderr.
					if cfg.PG == "" {
						cfg.PG = value
					}
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
				case "screen_detect_write":
					cfg.ScreenDetectWrite = value
				case "screen_detect_read":
					cfg.ScreenDetectRead = value
				case "screen_detect_read_score":
					if n, err := strconv.Atoi(value); err == nil {
						cfg.ScreenDetectReadScore = n
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
				case "insecure_plaintext":
					if b, err := strconv.ParseBool(value); err == nil {
						cfg.InsecurePlaintext = b
					}
				case "tls_ca_file":
					cfg.TLSCAFile = expandTilde(value)
				case "tls_client_cert_file":
					cfg.TLSClientCertFile = expandTilde(value)
				case "tls_client_key_file":
					cfg.TLSClientKeyFile = expandTilde(value)
				}
			}
			warnIfUnreadable(scanner, path)
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
	if v := os.Getenv("MEMSTORE_PG_SECRET"); v != "" {
		cfg.PG = v
	} else if v := os.Getenv("MEMSTORE_PG"); v != "" {
		cfg.PG = v
		warnDeprecatedPGEnv()
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
	if v := os.Getenv("MEMSTORE_SCREEN_DETECT_WRITE"); v != "" {
		cfg.ScreenDetectWrite = v
	}
	if v := os.Getenv("MEMSTORE_SCREEN_DETECT_READ"); v != "" {
		cfg.ScreenDetectRead = v
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
		{"MEMSTORE_SCREEN_DETECT_READ_SCORE", &cfg.ScreenDetectReadScore},
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
	if v := os.Getenv("MEMSTORE_INSECURE_PLAINTEXT"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.InsecurePlaintext = b
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

// warnIfUnreadable reports a config file that could not be read to the end.
//
// Both config readers are documented as returning defaults rather than an
// error, and callers depend on that -- a missing config file is the ordinary
// first-run case. But a scan that stops early is different from a file that is
// absent: the keys below the failure are silently unread, so the caller gets a
// config that looks complete and is not.
//
// That matters most for `remote`. Losing it makes memstore-mcp fall back to an
// refusal to open any store, which presents as an empty
// corpus rather than as a misconfiguration. bufio.Scanner reports a line past
// its 64KB buffer as an error rather than a short read, so this is the only
// place that distinction is visible. Warn rather than fail: the partial config
// is still the best available answer, but the silence has to go.
func warnIfUnreadable(scanner *bufio.Scanner, path string) {
	if err := scanner.Err(); err != nil {
		log.Printf("memstore: config %s could not be read to the end (%v); "+
			"any settings after the failure were ignored", path, err)
	}
}
