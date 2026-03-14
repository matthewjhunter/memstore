package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateHook(t *testing.T) {
	input := `const BIN = '__MEMSTORE_BIN__';
const URL = '__MEMSTORED_URL__';`

	replacements := map[string]string{
		"__MEMSTORE_BIN__":  "/usr/local/bin/memstore",
		"__MEMSTORED_URL__": "http://myhost:8230",
	}

	got := templateHook(input, replacements)
	want := `const BIN = '/usr/local/bin/memstore';
const URL = 'http://myhost:8230';`

	if got != want {
		t.Errorf("templateHook:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestTemplateHook_noPlaceholders(t *testing.T) {
	input := "no placeholders here"
	got := templateHook(input, map[string]string{
		"__MEMSTORE_BIN__":  "/bin/memstore",
		"__MEMSTORED_URL__": "http://localhost:8230",
	})
	if got != input {
		t.Errorf("expected unchanged content, got %q", got)
	}
}

func TestMergeSettings_empty(t *testing.T) {
	hookDir := "/home/test/.claude/hooks"
	out, err := mergeSettings(nil, hookDir)
	if err != nil {
		t.Fatalf("mergeSettings: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(out, &settings); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("expected hooks key in output")
	}

	// Verify all events are present.
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"} {
		if _, ok := hooks[event]; !ok {
			t.Errorf("missing event: %s", event)
		}
	}

	// Verify PreToolUse has both Read and Edit matchers.
	preToolEntries, ok := hooks["PreToolUse"].([]any)
	if !ok {
		t.Fatal("PreToolUse should be an array")
	}
	matchers := make(map[string]bool)
	for _, entry := range preToolEntries {
		obj := entry.(map[string]any)
		matchers[obj["matcher"].(string)] = true
	}
	if !matchers["Read"] || !matchers["Edit"] {
		t.Errorf("PreToolUse should have Read and Edit matchers, got %v", matchers)
	}

	// Verify PostToolUse has both Write and Bash matchers.
	postToolEntries, ok := hooks["PostToolUse"].([]any)
	if !ok {
		t.Fatal("PostToolUse should be an array")
	}
	matchers = make(map[string]bool)
	for _, entry := range postToolEntries {
		obj := entry.(map[string]any)
		matchers[obj["matcher"].(string)] = true
	}
	if !matchers["Write"] || !matchers["Bash"] {
		t.Errorf("PostToolUse should have Write and Bash matchers, got %v", matchers)
	}
}

func TestMergeSettings_preservesExisting(t *testing.T) {
	hookDir := "/home/test/.claude/hooks"

	// Existing settings with a non-memstore hook.
	existing := `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "node /home/test/.claude/hooks/my-custom-hook.mjs",
            "timeout": 3
          }
        ]
      }
    ]
  },
  "other_setting": true
}`

	out, err := mergeSettings([]byte(existing), hookDir)
	if err != nil {
		t.Fatalf("mergeSettings: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(out, &settings); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Non-hook settings preserved.
	if settings["other_setting"] != true {
		t.Error("other_setting should be preserved")
	}

	// SessionStart should have both the custom hook and the memstore hook.
	hooks := settings["hooks"].(map[string]any)
	sessionStart := hooks["SessionStart"].([]any)

	// Find custom hook still present.
	foundCustom := false
	foundMemstore := false
	for _, entry := range sessionStart {
		obj := entry.(map[string]any)
		hookList := obj["hooks"].([]any)
		for _, h := range hookList {
			hObj := h.(map[string]any)
			cmd := hObj["command"].(string)
			if strings.Contains(cmd, "my-custom-hook.mjs") {
				foundCustom = true
			}
			if strings.Contains(cmd, "memstore-startup.mjs") {
				foundMemstore = true
			}
		}
	}
	if !foundCustom {
		t.Error("custom hook should be preserved")
	}
	if !foundMemstore {
		t.Error("memstore hook should be added")
	}
}

func TestMergeSettings_updatesExisting(t *testing.T) {
	hookDir := "/home/test/.claude/hooks"

	// Existing settings with an old memstore hook path.
	existing := `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "node /old/path/memstore-startup.mjs",
            "timeout": 3
          }
        ]
      }
    ]
  }
}`

	out, err := mergeSettings([]byte(existing), hookDir)
	if err != nil {
		t.Fatalf("mergeSettings: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(out, &settings); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	hooks := settings["hooks"].(map[string]any)
	sessionStart := hooks["SessionStart"].([]any)

	// The old path should be replaced with the new path.
	obj := sessionStart[0].(map[string]any)
	hookList := obj["hooks"].([]any)
	hObj := hookList[0].(map[string]any)
	cmd := hObj["command"].(string)

	if !strings.Contains(cmd, hookDir) {
		t.Errorf("expected command to use new hookDir %s, got %s", hookDir, cmd)
	}
	// Timeout should be updated too.
	timeout := hObj["timeout"]
	if timeout != float64(5) { // JSON numbers are float64
		t.Errorf("expected timeout 5, got %v", timeout)
	}
}

func TestMergeSettings_idempotent(t *testing.T) {
	hookDir := "/home/test/.claude/hooks"

	// First merge from empty.
	out1, err := mergeSettings(nil, hookDir)
	if err != nil {
		t.Fatalf("first mergeSettings: %v", err)
	}

	// Second merge with the output of the first.
	out2, err := mergeSettings(out1, hookDir)
	if err != nil {
		t.Fatalf("second mergeSettings: %v", err)
	}

	if string(out1) != string(out2) {
		t.Errorf("mergeSettings is not idempotent:\nfirst:  %s\nsecond: %s", out1, out2)
	}
}

func TestContainsScript(t *testing.T) {
	tests := []struct {
		command string
		script  string
		want    bool
	}{
		{"node /home/user/.claude/hooks/memstore-startup.mjs", "memstore-startup.mjs", true},
		{"node /different/path/memstore-startup.mjs", "memstore-startup.mjs", true},
		{"node memstore-startup.mjs", "memstore-startup.mjs", true},
		{"node /home/user/.claude/hooks/other-hook.mjs", "memstore-startup.mjs", false},
		{"memstore-startup.mjs", "memstore-startup.mjs", true},
	}

	for _, tt := range tests {
		got := containsScript(tt.command, tt.script)
		if got != tt.want {
			t.Errorf("containsScript(%q, %q) = %v, want %v", tt.command, tt.script, got, tt.want)
		}
	}
}

func TestFindHookEntry(t *testing.T) {
	entries := []any{
		map[string]any{
			"matcher": "*",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": "node /path/to/memstore-startup.mjs",
					"timeout": float64(5),
				},
			},
		},
		map[string]any{
			"matcher": "Read",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": "node /path/to/other-hook.mjs",
					"timeout": float64(3),
				},
			},
		},
	}

	// Should find memstore-startup.mjs under matcher "*".
	mi, hi := findHookEntry(entries, "*", "memstore-startup.mjs")
	if mi != 0 || hi != 0 {
		t.Errorf("expected (0, 0), got (%d, %d)", mi, hi)
	}

	// Should find matcher "Read" but not the script.
	mi, hi = findHookEntry(entries, "Read", "memstore-read.mjs")
	if mi != 1 || hi != -1 {
		t.Errorf("expected (1, -1), got (%d, %d)", mi, hi)
	}

	// Should not find matcher "Edit" at all.
	mi, hi = findHookEntry(entries, "Edit", "memstore-edit.mjs")
	if mi != -1 || hi != -1 {
		t.Errorf("expected (-1, -1), got (%d, %d)", mi, hi)
	}
}

func TestInstallOneHook_new(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test-hook.mjs")

	action := installOneHook(dest, "test-hook.mjs", "hook content", false, false)
	if action.Status != "installed" {
		t.Errorf("expected installed, got %s", action.Status)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hook content" {
		t.Errorf("expected %q, got %q", "hook content", string(data))
	}
}

func TestInstallOneHook_identical(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test-hook.mjs")
	os.WriteFile(dest, []byte("hook content"), 0644)

	action := installOneHook(dest, "test-hook.mjs", "hook content", false, false)
	if action.Status != "skipped" {
		t.Errorf("expected skipped, got %s", action.Status)
	}
}

func TestInstallOneHook_differs_noForce(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test-hook.mjs")
	os.WriteFile(dest, []byte("old content"), 0644)

	action := installOneHook(dest, "test-hook.mjs", "new content", false, false)
	if action.Status != "warning" {
		t.Errorf("expected warning, got %s", action.Status)
	}

	// Content should not have changed.
	data, _ := os.ReadFile(dest)
	if string(data) != "old content" {
		t.Error("file should not have been modified without --force")
	}
}

func TestInstallOneHook_differs_force(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test-hook.mjs")
	os.WriteFile(dest, []byte("old content"), 0644)

	action := installOneHook(dest, "test-hook.mjs", "new content", true, false)
	if action.Status != "updated" {
		t.Errorf("expected updated, got %s", action.Status)
	}

	data, _ := os.ReadFile(dest)
	if string(data) != "new content" {
		t.Error("file should have been overwritten with --force")
	}
}

func TestInstallOneHook_dryRun(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "test-hook.mjs")

	action := installOneHook(dest, "test-hook.mjs", "hook content", false, true)
	if action.Status != "installed" {
		t.Errorf("expected installed, got %s", action.Status)
	}

	// File should not exist in dry-run mode.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("file should not exist in dry-run mode")
	}
}
