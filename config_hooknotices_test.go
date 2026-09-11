package memstore_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// Hook notices default to on; config.toml can turn them off and the
// environment overrides the file, as for every other setting.
func TestLoadConfigHookNotices(t *testing.T) {
	t.Setenv("MEMSTORE_HOOK_NOTICES", "")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if !memstore.DefaultConfig().HookNotices || !memstore.LoadConfig().HookNotices {
		t.Error("hook notices should default to on")
	}

	if err := os.MkdirAll(filepath.Join(dir, "memstore"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memstore", "config.toml"), []byte("hook_notices = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if memstore.LoadConfig().HookNotices {
		t.Error("hook_notices = false in config.toml was ignored")
	}

	t.Setenv("MEMSTORE_HOOK_NOTICES", "true")
	if !memstore.LoadConfig().HookNotices {
		t.Error("MEMSTORE_HOOK_NOTICES=true did not override the file")
	}
}
