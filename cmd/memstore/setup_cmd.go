package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
)

// hookRegistration describes a single hook entry for settings.local.json.
type hookRegistration struct {
	Event   string
	Matcher string
	Script  string
	Timeout int
}

// hookRegistrations defines all memstore hooks to register.
var hookRegistrations = []hookRegistration{
	{"SessionStart", "*", "memstore-startup.mjs", 5},
	{"UserPromptSubmit", "*", "memstore-prompt.mjs", 5},
	{"PreToolUse", "Read", "memstore-read.mjs", 5},
	{"PreToolUse", "Edit", "memstore-edit.mjs", 5},
	{"PostToolUse", "Write", "store-nudge.mjs", 2},
	{"PostToolUse", "Bash", "store-nudge.mjs", 2},
	{"Stop", "*", "stop-hook.mjs", 10},
	{"SessionEnd", "*", "memstore-session-end.mjs", 5},
}

// setupAction records what happened for the summary table.
type setupAction struct {
	Component string
	Status    string // installed, skipped, updated, warning
	Detail    string
}

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite existing hooks and config")
	remoteURL := fs.String("remote", "", "memstored daemon URL (skip auto-detection)")
	dryRun := fs.Bool("dry-run", false, "show what would be done without doing it")
	fs.Parse(args)

	var actions []setupAction

	// 1. Check prerequisites.
	fmt.Println("Checking prerequisites...")
	checkPrerequisites()

	// 2. Detect paths.
	memstoreBin := detectBinary("memstore")
	mcpBin := detectBinary("memstore-mcp")
	fmt.Printf("  memstore binary: %s\n", memstoreBin)
	fmt.Printf("  memstore-mcp binary: %s\n", mcpBin)

	// 3. Auto-detect daemon mode.
	daemonURL := detectDaemonURL(*remoteURL)
	if daemonURL != "" {
		fmt.Printf("  daemon: %s\n", daemonURL)
	} else {
		fmt.Println("  daemon: not detected (local-only mode)")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	hookDir := filepath.Join(home, ".claude", "hooks")

	// 4. Install hooks.
	fmt.Println("\nInstalling hooks...")
	hookActions := installHooks(hookDir, memstoreBin, daemonURL, *force, *dryRun)
	actions = append(actions, hookActions...)

	// 5. Generate settings.local.json.
	fmt.Println("\nConfiguring settings.local.json...")
	settingsPath := filepath.Join(home, ".claude", "settings.local.json")
	settingsAction := mergeSettingsFile(settingsPath, hookDir, *dryRun)
	actions = append(actions, settingsAction)

	// 6. Register MCP server.
	fmt.Println("\nRegistering MCP server...")
	mcpAction := registerMCP(mcpBin, daemonURL, *dryRun)
	actions = append(actions, mcpAction)

	// 7. Create config.toml.
	fmt.Println("\nChecking config.toml...")
	configAction := ensureConfig(daemonURL, *force, *dryRun)
	actions = append(actions, configAction)

	// 8. Print summary.
	printSummary(actions, daemonURL)
}

// checkPrerequisites verifies that external dependencies are available.
func checkPrerequisites() {
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Println("  [warn] claude CLI not found on PATH")
	} else {
		fmt.Println("  [ok]   claude CLI found")
	}

	ollamaURL := cliConfig.Ollama
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	if checkHTTP(ollamaURL+"/api/tags", 2*time.Second) {
		fmt.Printf("  [ok]   Ollama reachable at %s\n", ollamaURL)
	} else {
		fmt.Printf("  [warn] Ollama not reachable at %s\n", ollamaURL)
	}
}

// detectBinary finds a binary by checking os.Executable()'s directory first,
// then $GOPATH/bin, then PATH.
func detectBinary(name string) string {
	// Check sibling of current executable.
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}

	// Check $GOPATH/bin.
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		candidate := filepath.Join(gopath, "bin", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Check ~/go/bin (default GOPATH).
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, "go", "bin", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Fall back to PATH lookup.
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	return name // bare name as last resort
}

// detectDaemonURL determines the memstored URL to use.
func detectDaemonURL(explicit string) string {
	if explicit != "" {
		return explicit
	}

	// Try localhost default.
	if checkHTTP("http://localhost:8230/healthz", 2*time.Second) {
		return "http://localhost:8230"
	}

	// Try existing config.
	if cliConfig.Remote != "" && checkHTTP(cliConfig.Remote+"/healthz", 2*time.Second) {
		return cliConfig.Remote
	}

	return ""
}

// checkHTTP sends a GET to url and returns true if it gets a response.
func checkHTTP(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// installHooks templates and writes hook scripts to the target directory.
func installHooks(hookDir, memstoreBin, daemonURL string, force, dryRun bool) []setupAction {
	var actions []setupAction

	if !dryRun {
		if err := os.MkdirAll(hookDir, 0755); err != nil {
			log.Fatalf("create hook directory: %v", err)
		}
	}

	// Default daemon URL for templates (hooks degrade gracefully if unreachable).
	templateDaemonURL := daemonURL
	if templateDaemonURL == "" {
		templateDaemonURL = "http://localhost:8230"
	}

	replacements := map[string]string{
		"__MEMSTORE_BIN__":  memstoreBin,
		"__MEMSTORED_URL__": templateDaemonURL,
	}

	entries, err := hookFS.ReadDir("hooks")
	if err != nil {
		log.Fatalf("read embedded hooks: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		srcData, err := hookFS.ReadFile(filepath.Join("hooks", name))
		if err != nil {
			log.Fatalf("read embedded hook %s: %v", name, err)
		}

		content := templateHook(string(srcData), replacements)
		destPath := filepath.Join(hookDir, name)

		action := installOneHook(destPath, name, content, force, dryRun)
		actions = append(actions, action)
	}

	return actions
}

// installOneHook writes a single hook file, handling skip/warn/overwrite logic.
func installOneHook(destPath, name, content string, force, dryRun bool) setupAction {
	existing, err := os.ReadFile(destPath)
	if err == nil {
		// File exists.
		if string(existing) == content {
			fmt.Printf("  [skip] %s (identical)\n", name)
			return setupAction{name, "skipped", "identical"}
		}
		if !force {
			fmt.Printf("  [warn] %s exists and differs (use --force to overwrite)\n", name)
			return setupAction{name, "warning", "exists, differs"}
		}
		// Force mode — overwrite.
		if !dryRun {
			if err := os.WriteFile(destPath, []byte(content), 0644); err != nil {
				log.Fatalf("write hook %s: %v", name, err)
			}
		}
		fmt.Printf("  [upd]  %s\n", name)
		return setupAction{name, "updated", "overwritten"}
	}

	// File doesn't exist — install.
	if !dryRun {
		if err := os.WriteFile(destPath, []byte(content), 0644); err != nil {
			log.Fatalf("write hook %s: %v", name, err)
		}
	}
	fmt.Printf("  [new]  %s\n", name)
	return setupAction{name, "installed", ""}
}

// templateHook replaces placeholder strings in hook content.
func templateHook(content string, replacements map[string]string) string {
	for placeholder, value := range replacements {
		content = strings.ReplaceAll(content, placeholder, value)
	}
	return content
}

// mergeSettingsFile reads, merges, and writes settings.local.json.
func mergeSettingsFile(path, hookDir string, dryRun bool) setupAction {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("read settings: %v", err)
	}

	merged, err := mergeSettings(existing, hookDir)
	if err != nil {
		log.Fatalf("merge settings: %v", err)
	}

	if string(existing) == string(merged) {
		fmt.Println("  [skip] settings.local.json (no changes needed)")
		return setupAction{"settings.local.json", "skipped", "no changes"}
	}

	if dryRun {
		fmt.Println("  [dry]  settings.local.json would be updated")
		return setupAction{"settings.local.json", "dry-run", "would update"}
	}

	// Write backup before modifying.
	if len(existing) > 0 {
		bakPath := path + ".bak"
		if err := os.WriteFile(bakPath, existing, 0600); err != nil {
			log.Fatalf("write settings backup: %v", err)
		}
		fmt.Printf("  [bak]  %s\n", bakPath)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Fatalf("create settings directory: %v", err)
	}
	if err := os.WriteFile(path, merged, 0600); err != nil {
		log.Fatalf("write settings: %v", err)
	}
	fmt.Println("  [upd]  settings.local.json")
	return setupAction{"settings.local.json", "updated", ""}
}

// mergeSettings merges memstore hook entries into an existing settings.local.json.
// It preserves all non-memstore entries. If existing is nil/empty, a new file is created.
func mergeSettings(existing []byte, hookDir string) ([]byte, error) {
	var settings map[string]any
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &settings); err != nil {
			return nil, fmt.Errorf("parse existing settings: %w", err)
		}
	}
	if settings == nil {
		settings = make(map[string]any)
	}

	// Get or create hooks map.
	hooksRaw, ok := settings["hooks"]
	if !ok {
		hooksRaw = make(map[string]any)
	}
	hooks, ok := hooksRaw.(map[string]any)
	if !ok {
		hooks = make(map[string]any)
	}

	// Build the set of memstore script filenames for matching.
	memstoreScripts := make(map[string]bool)
	for _, reg := range hookRegistrations {
		memstoreScripts[reg.Script] = true
	}

	// For each hook registration, ensure it exists in the settings.
	for _, reg := range hookRegistrations {
		command := fmt.Sprintf("node %s", filepath.Join(hookDir, reg.Script))
		hookObj := map[string]any{
			"type":    "command",
			"command": command,
			"timeout": reg.Timeout,
		}

		eventEntries := getJSONArray(hooks, reg.Event)
		matcherIdx, hookIdx := findHookEntry(eventEntries, reg.Matcher, reg.Script)

		if matcherIdx >= 0 && hookIdx >= 0 {
			// Update existing entry.
			matcher := eventEntries[matcherIdx].(map[string]any)
			hookList := getJSONArray(matcher, "hooks")
			hookList[hookIdx] = hookObj
			matcher["hooks"] = hookList
		} else if matcherIdx >= 0 {
			// Matcher exists but hook not found — add it.
			matcher := eventEntries[matcherIdx].(map[string]any)
			hookList := getJSONArray(matcher, "hooks")
			hookList = append(hookList, hookObj)
			matcher["hooks"] = hookList
		} else {
			// New matcher entry.
			entry := map[string]any{
				"matcher": reg.Matcher,
				"hooks":   []any{hookObj},
			}
			eventEntries = append(eventEntries, entry)
		}

		hooks[reg.Event] = eventEntries
	}

	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	return out, nil
}

// getJSONArray returns the array at key in obj, or an empty slice.
func getJSONArray(obj map[string]any, key string) []any {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	return arr
}

// findHookEntry searches event entries for a matcher+script combination.
// Returns (matcherIndex, hookIndex) or (-1, -1) if not found.
func findHookEntry(entries []any, matcher, script string) (int, int) {
	for i, entry := range entries {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		m, _ := obj["matcher"].(string)
		if m != matcher {
			continue
		}
		// Found the matcher — look for the script in hooks.
		hookList := getJSONArray(obj, "hooks")
		for j, h := range hookList {
			hObj, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hObj["command"].(string)
			if containsScript(cmd, script) {
				return i, j
			}
		}
		return i, -1 // matcher found, but script not in its hooks
	}
	return -1, -1
}

// containsScript checks if a command string references the given script filename.
func containsScript(command, script string) bool {
	return strings.HasSuffix(command, "/"+script) ||
		strings.HasSuffix(command, " "+script) ||
		command == script
}

// registerMCP registers the memstore MCP server with Claude Code.
func registerMCP(mcpBin, daemonURL string, dryRun bool) setupAction {
	// Check if already registered with matching command.
	out, err := exec.Command("claude", "mcp", "list").Output()
	if err == nil && strings.Contains(string(out), "memstore") {
		fmt.Println("  [skip] memstore MCP already registered")
		return setupAction{"MCP server", "skipped", "already registered"}
	}

	args := []string{"mcp", "add", "memstore", "-s", "user", "--"}
	args = append(args, mcpBin)
	if daemonURL != "" {
		args = append(args, "--remote", daemonURL)
	}

	if dryRun {
		fmt.Printf("  [dry]  would run: claude %s\n", strings.Join(args, " "))
		return setupAction{"MCP server", "dry-run", "would register"}
	}

	cmd := exec.Command("claude", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("  [warn] MCP registration failed: %v\n%s\n", err, output)
		return setupAction{"MCP server", "warning", err.Error()}
	}

	fmt.Println("  [ok]   memstore MCP registered")
	return setupAction{"MCP server", "installed", ""}
}

// ensureConfig creates config.toml if it doesn't exist.
func ensureConfig(daemonURL string, force, dryRun bool) setupAction {
	configPath := memstore.ConfigPath()
	if configPath == "" {
		return setupAction{"config.toml", "warning", "cannot determine config path"}
	}

	if _, err := os.Stat(configPath); err == nil && !force {
		fmt.Println("  [skip] config.toml already exists")
		return setupAction{"config.toml", "skipped", "already exists"}
	}

	var lines []string
	lines = append(lines, "# memstore configuration")
	lines = append(lines, "# See: https://github.com/matthewjhunter/memstore")
	lines = append(lines, "")
	if daemonURL != "" {
		lines = append(lines, fmt.Sprintf("remote = %q", daemonURL))
	} else {
		lines = append(lines, "# remote = \"http://localhost:8230\"")
	}
	lines = append(lines, "")
	content := strings.Join(lines, "\n")

	if dryRun {
		fmt.Printf("  [dry]  would create %s\n", configPath)
		return setupAction{"config.toml", "dry-run", "would create"}
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		log.Fatalf("write config: %v", err)
	}
	fmt.Printf("  [ok]   %s\n", configPath)
	return setupAction{"config.toml", "installed", ""}
}

// printSummary outputs a table of all actions taken.
func printSummary(actions []setupAction, daemonURL string) {
	fmt.Println("\n--- Summary ---")
	fmt.Printf("%-30s %-12s %s\n", "Component", "Status", "Detail")
	fmt.Println(strings.Repeat("-", 60))
	for _, a := range actions {
		fmt.Printf("%-30s %-12s %s\n", a.Component, a.Status, a.Detail)
	}

	// Note daemon-dependent hooks.
	daemonHooks := []string{"memstore-prompt.mjs", "memstore-context-touch.mjs", "stop-hook.mjs"}
	if daemonURL == "" {
		fmt.Println("\nNote: The following hooks require memstored (daemon mode):")
		for _, h := range daemonHooks {
			fmt.Printf("  - %s (will silently no-op without daemon)\n", h)
		}
	}

	fmt.Println("\nRestart your Claude Code session to activate the hooks.")
}
