// Command memstore-mcp is an MCP server that gives Claude (or any MCP client)
// persistent, searchable memory backed by SQLite with hybrid FTS5 + vector search.
//
// Usage:
//
//	memstore-mcp [flags]
//
// Flags:
//
//	--db         Path to SQLite database (default: ~/.local/share/memstore/memory.db)
//	--namespace  Namespace for fact isolation (default: "default")
//	--ollama     Ollama base URL (default: http://localhost:11434)
//	--model      Embedding model name (default: nomic-embed-text)
//
// The server communicates over stdio using newline-delimited JSON-RPC
// (the MCP stdio transport). Register it with Claude Code via:
//
//	claude mcp add memstore -s user -- /path/to/memstore-mcp [flags]
//
// This stores the config in ~/.claude.json at user scope so it is
// available in all projects.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

func main() {
	cfg := memstore.LoadConfig()
	remote := flag.String("remote", cfg.Remote, "memstored URL for daemon mode (empty = local SQLite)")
	apiKey := flag.String("api-key", cfg.APIKey, "API key for memstored auth")
	dbPath := flag.String("db", cfg.DB, "path to SQLite database (local mode only)")
	namespace := flag.String("namespace", cfg.Namespace, "namespace for fact isolation (local mode only)")
	ollamaURL := flag.String("ollama", cfg.Ollama, "LLM API base URL (local mode only)")
	model := flag.String("model", cfg.Model, "embedding model name (local mode only)")
	llmAPIKey := flag.String("llm-api-key", cfg.LLMAPIKey, "API key for the LLM provider (empty = no auth)")
	genModel := flag.String("gen-model", cfg.GenModel, "LLM model for generation")
	hookMode := flag.Bool("hook", false, "read Stop hook JSON from stdin, POST to memstored, exit")
	postCompactMode := flag.Bool("post-compact", false, "read PostCompact hook JSON from stdin, store the compact_summary as a memstore fact, exit")
	transcriptPath := flag.String("transcript", "", "read JSONL transcript from path, POST to memstored, exit")
	flag.Parse()

	tlsOpts := httpclient.ClientOptionsFromConfig(cfg)

	// Hook capture modes — run without starting the MCP server.
	if *hookMode {
		runHookCapture(*remote, *apiKey, tlsOpts)
		return
	}
	if *postCompactMode {
		runPostCompactCapture(*remote, *apiKey, tlsOpts)
		return
	}
	if *transcriptPath != "" {
		runTranscriptCapture(*remote, *apiKey, *transcriptPath, tlsOpts)
		return
	}

	// Log to stderr to keep stdout clean for MCP JSON-RPC.
	log.SetOutput(os.Stderr)

	var store memstore.Store
	var embedder memstore.Embedder

	if *remote != "" {
		// Daemon mode: talk to memstored over HTTP.
		c, err := httpclient.NewWithOptions(*remote, *apiKey, tlsOpts)
		if err != nil {
			log.Fatalf("memstore-mcp: build remote client: %v", err)
		}
		store = c
		log.Printf("memstore-mcp starting in daemon mode (remote=%s)", *remote)
	} else {
		// Local mode: open SQLite directly.
		if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
			log.Fatalf("creating db directory: %v", err)
		}

		db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
		if err != nil {
			log.Fatalf("opening database: %v", err)
		}
		defer db.Close()

		// Single connection for WAL mode correctness with memstore's mutex.
		db.SetMaxOpenConns(1)

		embedder = memstore.NewOpenAIEmbedder(*ollamaURL, *llmAPIKey, *model)

		sqlStore, err := memstore.NewSQLiteStore(db, embedder, *namespace)
		if err != nil {
			log.Fatalf("initializing store: %v", err)
		}
		store = sqlStore
		log.Printf("memstore-mcp starting in local mode (db=%s, namespace=%s, model=%s)", *dbPath, *namespace, *model)
	}

	srvCfg := mcpserver.Config{}
	if *remote != "" {
		// Daemon mode: generation and feedback go through memstored.
		rc, err := httpclient.NewWithOptions(*remote, *apiKey, tlsOpts)
		if err != nil {
			log.Fatalf("memstore-mcp: build remote feedback client: %v", err)
		}
		gen, err := httpclient.NewHTTPGeneratorWithOptions(*remote, *apiKey, tlsOpts)
		if err != nil {
			log.Fatalf("memstore-mcp: build remote generator: %v", err)
		}
		srvCfg.Generator = gen
		srvCfg.SessionStore = rc // enables memory_rate_context
	} else if *genModel != "" {
		// Local mode: talk to Ollama directly.
		srvCfg.Generator = memstore.NewOpenAIGenerator(*ollamaURL, *llmAPIKey, *genModel)
	}

	memorySrv := mcpserver.NewMemoryServerWithConfig(store, embedder, srvCfg)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "memstore",
		Version: "0.1.0",
	}, nil)

	memorySrv.Register(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("server error: %v", err)
	}
	// Note: pending uploads are drained gradually by the Stop hook
	// (runHookCapture → drainOnePendingUpload), one per Stop event.
	// We don't drain on MCP shutdown anymore — Claude Code SIGKILLs the
	// server, so any post-Run cleanup wouldn't reliably execute anyway.
}

// sessionsDir returns the directory where per-session state files are written by the Stop hook.
func sessionsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "memstore", "sessions")
}

// currentPersona returns the OS username of the user running this client.
// It is sent to the daemon as the subject for user/preference-scoped
// session summaries. Memstored is multi-user; identity must come from
// the client, never from the daemon process itself. Falls back to "user"
// if the OS lookup fails so the upload still succeeds.
func currentPersona() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "user"
}

// hookEvent is the Claude Code Stop hook payload. Only the fields we use
// are listed; extra fields in the JSON are ignored.
type hookEvent struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
}

// sessionState is the per-session state file written to ~/.cache/memstore/sessions/<session_id>.json.
// The format must stay compatible with anything else that reads or writes
// these files — the compact-before-exit hook (compact-before-exit.mjs) reads
// LastCompactedAt and toggles ExitGatePending, the older JS Stop hook left
// files in this format, and runHookCapture writes from Go.
type sessionState struct {
	SessionID       string `json:"session_id"`
	CWD             string `json:"cwd,omitempty"`
	TranscriptPath  string `json:"transcript_path,omitempty"`
	MessageCount    int    `json:"message_count"`
	Nudged          bool   `json:"nudged,omitempty"`
	LastCompactedAt string `json:"last_compacted_at,omitempty"`
	ExitGatePending bool   `json:"exit_gate_pending,omitempty"`
}

// Hook tuning knobs.
const (
	hookNudgeThreshold      = 8
	hookMaxInlineTranscript = 5 * 1024 * 1024 // 5 MB — anything larger is uploaded by a detached subprocess
	hookSessionPostTimeout  = 5 * time.Second
	hookNudgePostTimeout    = 2 * time.Second
	hookDrainUploadTimeout  = 5 * time.Second
	hookNudgeText           = "This session has had several exchanges. If architectural decisions were made, new repos created, or work was deferred, store them now using memory_store or memory_store_batch. Check what was discussed and whether anything should persist for future sessions."
)

// runHookCapture handles a Claude Code Stop hook event end-to-end:
//  1. Forwards the raw hook payload to /v1/sessions/hook (archive).
//  2. Updates the per-session state file (message count, transcript path).
//  3. Emits a "store your decisions" nudge after the message-count threshold.
//  4. Drains one pending session-transcript upload from the backlog,
//     skipping the current session whose transcript is still being written.
//
// All work previously lived in ~/.claude/hooks/stop-hook.mjs in JavaScript.
// Consolidating here gives us one upload code path with persona, retry,
// and routing all in Go.
func runHookCapture(remote, apiKey string, tlsOpts httpclient.ClientOptions) {
	if remote == "" {
		return // no remote configured — silently skip
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("memstore-mcp --hook: read stdin: %v", err)
	}
	var event hookEvent
	if err := json.Unmarshal(data, &event); err != nil {
		log.Printf("memstore-mcp --hook: invalid JSON on stdin: %v", err)
		return
	}

	c, err := httpclient.NewWithOptions(remote, apiKey, tlsOpts)
	if err != nil {
		log.Fatalf("memstore-mcp --hook: build client: %v", err)
	}

	// 1. Forward raw hook payload (archive).
	postCtx, postCancel := context.WithTimeout(context.Background(), hookSessionPostTimeout)
	if err := c.PostSessionHook(postCtx, json.RawMessage(data)); err != nil {
		log.Printf("memstore-mcp --hook: post hook: %v", err)
	}
	postCancel()

	// 2-3. Per-session state tracking and nudge emission.
	if event.SessionID != "" {
		state := updateSessionState(event)
		maybeEmitNudge(c, event, state)
	}

	// 4. Drain one pending upload, skipping any session whose Claude Code
	// process is still alive (i.e. transcript is still being written).
	drainOnePendingUpload(c)
}

// postCompactEvent is the Claude Code PostCompact hook payload. Fires after
// /compact (manual) or auto-compaction, with the model-generated summary in
// CompactSummary. Trigger distinguishes "manual" from "auto" so we can tag
// each in metadata for later filtering.
type postCompactEvent struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Trigger        string `json:"trigger"`
	CompactSummary string `json:"compact_summary"`
}

// runPostCompactCapture handles Claude Code's PostCompact hook by storing
// the compact_summary as a memstore fact directly. The summary is produced
// by the session's own model (Opus/Sonnet) using its full in-context
// understanding of the conversation, so it's higher quality than anything
// we can re-derive from the transcript on the daemon side.
//
// Subject is derived from the cwd's enclosing git repo. We don't try to
// classify scope here; daemon-side recall can layer scope inference on top
// of source=post_compact facts later if needed.
func runPostCompactCapture(remote, apiKey string, tlsOpts httpclient.ClientOptions) {
	if remote == "" {
		return // no remote configured — silently skip
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("memstore-mcp --post-compact: read stdin: %v", err)
	}
	var event postCompactEvent
	if err := json.Unmarshal(data, &event); err != nil {
		log.Printf("memstore-mcp --post-compact: invalid JSON on stdin: %v", err)
		return
	}
	if strings.TrimSpace(event.CompactSummary) == "" {
		log.Printf("memstore-mcp --post-compact: empty compact_summary, skipping")
		return
	}

	c, err := httpclient.NewWithOptions(remote, apiKey, tlsOpts)
	if err != nil {
		log.Fatalf("memstore-mcp --post-compact: build client: %v", err)
	}

	subject := memstore.ProjectNameFromCWD(event.CWD)
	persona := currentPersona()
	trigger := event.Trigger
	if trigger == "" {
		trigger = "unknown"
	}
	meta, _ := json.Marshal(map[string]string{
		"source":     "post_compact",
		"trigger":    trigger,
		"session_id": event.SessionID,
		"cwd":        event.CWD,
		"persona":    persona,
	})
	fact := memstore.Fact{
		Content:  event.CompactSummary,
		Subject:  subject,
		Category: "project",
		Kind:     "summary",
		Metadata: json.RawMessage(meta),
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookSessionPostTimeout)
	defer cancel()
	id, err := c.Insert(ctx, fact)
	if err != nil {
		log.Printf("memstore-mcp --post-compact: insert: %v", err)
		return
	}
	log.Printf("memstore-mcp --post-compact: stored fact id=%d subject=%s trigger=%s", id, subject, trigger)

	// Mark the session compacted so the compact-before-exit gate knows to
	// pass through subsequent /exit attempts without nagging.
	if event.SessionID != "" {
		markSessionCompacted(event.SessionID, event.CWD)
	}
}

// markSessionCompacted writes last_compacted_at and clears any pending exit
// gate flag on the session state file. Best-effort — failures don't affect
// the fact insert that already happened.
func markSessionCompacted(sessionID, cwd string) {
	dir := sessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("memstore-mcp --post-compact: mkdir sessions: %v", err)
		return
	}
	statePath := filepath.Join(dir, sessionID+".json")
	var state sessionState
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	state.SessionID = sessionID
	if cwd != "" {
		state.CWD = cwd
	}
	state.LastCompactedAt = time.Now().UTC().Format(time.RFC3339)
	state.ExitGatePending = false
	if data, err := json.MarshalIndent(state, "", "  "); err == nil {
		if err := os.WriteFile(statePath, data, 0o644); err != nil {
			log.Printf("memstore-mcp --post-compact: write state: %v", err)
		}
	}
}

// updateSessionState reads, mutates, and writes the per-session state file
// keyed on event.SessionID. Returns the post-write state so the caller can
// decide whether to emit a nudge.
func updateSessionState(event hookEvent) sessionState {
	dir := sessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("memstore-mcp --hook: mkdir sessions: %v", err)
		return sessionState{}
	}
	statePath := filepath.Join(dir, event.SessionID+".json")

	var state sessionState
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &state)
	}

	state.SessionID = event.SessionID
	if event.TranscriptPath != "" {
		state.TranscriptPath = event.TranscriptPath
	}
	if event.CWD != "" {
		state.CWD = event.CWD
	}
	state.MessageCount++

	if data, err := json.MarshalIndent(state, "", "  "); err == nil {
		if err := os.WriteFile(statePath, data, 0o644); err != nil {
			log.Printf("memstore-mcp --hook: write state: %v", err)
		}
	}
	return state
}

// maybeEmitNudge posts a "store your decisions" hint if the session has
// crossed the threshold and hasn't already been nudged. Best-effort —
// failures are logged but don't block the hook.
func maybeEmitNudge(c *httpclient.Client, event hookEvent, state sessionState) {
	if state.MessageCount < hookNudgeThreshold || state.Nudged {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookNudgePostTimeout)
	defer cancel()
	hint := memstore.ContextHint{
		SessionID:    event.SessionID,
		CWD:          event.CWD,
		TurnIndex:    state.MessageCount,
		HintText:     hookNudgeText,
		Relevance:    0.8,
		Desirability: 0.9,
	}
	if err := c.PostHint(ctx, hint); err != nil {
		log.Printf("memstore-mcp --hook: nudge: %v", err)
		return
	}
	// Mark nudged so we don't repeat. Re-read + write to avoid clobbering
	// concurrent updates from other Stop events.
	statePath := filepath.Join(sessionsDir(), event.SessionID+".json")
	if data, err := os.ReadFile(statePath); err == nil {
		var s sessionState
		if json.Unmarshal(data, &s) == nil {
			s.Nudged = true
			if out, err := json.MarshalIndent(s, "", "  "); err == nil {
				_ = os.WriteFile(statePath, out, 0o644)
			}
		}
	}
}

// drainOnePendingUpload picks one pending session state file whose Claude
// Code process is no longer alive, atomically claims it, uploads its
// transcript, and renames to .done on success. Returns after one attempt —
// the next Stop event drains the next entry.
//
// The "is the session still alive" check uses Claude Code's own session
// state files in ~/.claude/sessions/<pid>.json: any session whose pid
// exists and is still running is considered active. This handles the
// /exit+resume case correctly — Claude Code reuses the same session_id
// on resume but spawns a new process, so as long as that new process is
// alive, we keep skipping the (still-being-appended) transcript. Once
// the resumed process also exits, the next Stop hook from any other
// session will pick the transcript up.
func drainOnePendingUpload(c *httpclient.Client) {
	alive := aliveClaudeSessions()

	dir := sessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		statePath := filepath.Join(dir, entry.Name())
		var state sessionState
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &state); err != nil || state.SessionID == "" {
			continue
		}
		if alive[state.SessionID] {
			continue // session's Claude Code process is still running
		}
		if state.TranscriptPath == "" {
			continue
		}

		info, err := os.Stat(state.TranscriptPath)
		if err != nil {
			// Transcript file missing — mark as done so we don't retry forever.
			_ = os.Rename(statePath, strings.TrimSuffix(statePath, ".json")+".done")
			continue
		}
		if info.Size() > hookMaxInlineTranscript {
			// Too large for the hook timeout budget — spawn a detached
			// subprocess that uploads via --transcript and exits.
			cmd := exec.Command(os.Args[0], "--transcript", state.TranscriptPath)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err == nil {
				_ = cmd.Process.Release()
				_ = os.Rename(statePath, strings.TrimSuffix(statePath, ".json")+".done")
			}
			return // one per invocation
		}

		// Atomic claim — only one process wins this rename.
		uploading := statePath + ".uploading"
		if err := os.Rename(statePath, uploading); err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), hookDrainUploadTimeout)
		content, err := os.ReadFile(state.TranscriptPath)
		if err != nil {
			cancel()
			_ = os.Rename(uploading, statePath)
			continue
		}
		err = c.PostSessionTranscript(ctx, state.SessionID, state.CWD, currentPersona(), string(content))
		cancel()
		if err != nil {
			log.Printf("memstore-mcp --hook: upload %s: %v", state.SessionID, err)
			_ = os.Rename(uploading, statePath) // restore for retry
		} else {
			_ = os.Rename(uploading, strings.TrimSuffix(statePath, ".json")+".done")
		}
		return // one per invocation
	}
}

// aliveClaudeSessions returns the set of Claude Code session_ids whose
// process is currently running. It reads each ~/.claude/sessions/<pid>.json
// file (Claude Code's own session state) and probes the recorded pid with
// signal 0; living pids contribute their session_id to the set.
//
// Process recycling between scan and use is theoretically possible but the
// window is small. If we mistakenly skip an upload, the next Stop hook
// retries — there's no permanent data loss path.
func aliveClaudeSessions() map[string]bool {
	alive := map[string]bool{}
	home, err := os.UserHomeDir()
	if err != nil {
		return alive
	}
	dir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return alive // no Claude session dir → nothing alive that we know of
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var state struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		if state.PID <= 0 || state.SessionID == "" {
			continue
		}
		if isProcessAlive(state.PID) {
			alive[state.SessionID] = true
		}
	}
	return alive
}

// isProcessAlive reports whether a process with the given pid currently
// exists. On Unix, sending signal 0 returns nil if the process exists and
// the caller has permission; ESRCH means dead. EPERM means alive (different
// user) — we still count it alive because it's a real running process.
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// runTranscriptCapture reads a JSONL transcript file and POSTs it to memstored.
// Session metadata (session_id, cwd) is resolved by scanning the sessions directory
// for a state file whose transcript_path matches the given path.
func runTranscriptCapture(remote, apiKey, path string, tlsOpts httpclient.ClientOptions) {
	if remote == "" {
		return
	}
	var sessionID, cwd string
	if entries, err := os.ReadDir(sessionsDir()); err == nil {
		for _, entry := range entries {
			ext := filepath.Ext(entry.Name())
			if entry.IsDir() || (ext != ".json" && ext != ".done") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sessionsDir(), entry.Name()))
			if err != nil {
				continue
			}
			var state struct {
				SessionID      string `json:"session_id"`
				TranscriptPath string `json:"transcript_path"`
				CWD            string `json:"cwd"`
			}
			if json.Unmarshal(data, &state) == nil && state.TranscriptPath == path {
				sessionID = state.SessionID
				cwd = state.CWD
				break
			}
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("memstore-mcp --transcript: read file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := httpclient.NewWithOptions(remote, apiKey, tlsOpts)
	if err != nil {
		log.Fatalf("memstore-mcp --transcript: build client: %v", err)
	}
	if err := c.PostSessionTranscript(ctx, sessionID, cwd, currentPersona(), string(content)); err != nil {
		log.Fatalf("memstore-mcp --transcript: post: %v", err)
	}
}
