package memstore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// AppConfig holds persistent defaults for the memstore CLI and MCP server.
// Values are loaded from the config file and can be overridden by CLI flags.
type AppConfig struct {
	DB        string
	Namespace string
	Ollama    string
	Model     string
	GenModel  string
	Remote    string // memstored URL; if set, use daemon mode instead of local SQLite
	APIKey    string // API key for memstored auth
	LLMAPIKey string // API key for the LLM provider (LiteLLM, OpenAI, etc.)
	Addr      string // listen address for memstored daemon
	PG        string // PostgreSQL connection string; if set, use Postgres instead of SQLite
	VecDim    int    // embedding vector dimension for Postgres (e.g. 768)
}

// DefaultConfig returns the built-in defaults used when no config file exists.
func DefaultConfig() AppConfig {
	return AppConfig{
		DB:        defaultDBPath(),
		Namespace: "default",
		Ollama:    "http://localhost:11434",
		Model:     "embeddinggemma",
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
				case "model":
					cfg.Model = value
				case "gen_model":
					cfg.GenModel = value
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
	if v := os.Getenv("MEMSTORE_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("MEMSTORE_GEN_MODEL"); v != "" {
		cfg.GenModel = v
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
