// Package hookcapture implements the Claude Code Stop hook: it archives the
// hook payload, tracks per-session state, emits the "store your decisions"
// nudge, and drains the session-transcript upload backlog one entry per event.
//
// It lives here rather than in a command because two binaries run it. The MCP
// server ran it first, through `memstore-mcp --hook`, which made the Stop hook
// the last thing on the machine that needed that binary installed locally. The
// CLI runs it now, as `memstore hook`, and the MCP server keeps its flags as
// deprecated aliases until it is retired -- so a machine whose hook script and
// whose binaries are updated in either order keeps working.
//
// Everything here is best-effort. A Stop hook that fails loudly interrupts the
// user's session over telemetry, so failures are logged and the hook exits
// cleanly; the next Stop event retries whatever did not finish.
package hookcapture

import (
	"context"
	"encoding/json"
	"errors"
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
)

// Options is everything a capture run needs from its caller.
type Options struct {
	// Remote is the memstored base URL. Empty means no daemon is configured,
	// and every entry point returns without doing anything.
	Remote string
	APIKey string
	TLS    httpclient.ClientOptions

	// Respawn is the argument list that re-enters the calling binary in
	// transcript-upload mode; the transcript path is appended to it. A
	// transcript too large for the hook's timeout budget is uploaded by a
	// detached copy of the caller rather than inline, and only the caller
	// knows what its own command line looks like: "hook", "--transcript" for
	// the CLI, "--transcript" for the MCP server.
	Respawn []string

	// Tool names the caller in log lines. Defaults to "memstore hook".
	Tool string
}

func (o Options) tool() string {
	if o.Tool == "" {
		return "memstore hook"
	}
	return o.Tool
}

func (o Options) respawn() []string {
	if len(o.Respawn) == 0 {
		return []string{"hook", "--transcript"}
	}
	return o.Respawn
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
// The older JS Stop hook left files in this format; runHookCapture writes from Go.
type sessionState struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	MessageCount   int    `json:"message_count"`
	Nudged         bool   `json:"nudged,omitempty"`

	// UploadFailures counts consecutive failed upload attempts. See
	// hookMaxUploadFailures for why an entry is eventually given up on.
	UploadFailures int `json:"upload_failures,omitempty"`
}

// Hook tuning knobs.
const (
	hookNudgeThreshold      = 8
	hookMaxInlineTranscript = 5 * 1024 * 1024 // 5 MB -- anything larger is uploaded by a detached subprocess
	hookSessionPostTimeout  = 5 * time.Second
	hookNudgePostTimeout    = 2 * time.Second
	hookDrainUploadTimeout  = 5 * time.Second
	// hookMaxUploadFailures bounds retries of one transcript.
	//
	// The drain picks the first eligible entry in directory order and returns
	// after a single attempt, so an entry that fails is the entry the next Stop
	// event tries first -- and an entry that can never succeed blocks every
	// entry behind it forever. That is not hypothetical: a transcript carrying
	// a NUL was rejected by Postgres on every attempt it was ever given, with
	// ten uploadable sessions queued behind it.
	//
	// Giving up after a few tries keeps a transient failure (daemon restarting,
	// network blip) retryable while refusing to let a permanent one hold the
	// queue. The state file is renamed .failed rather than deleted, so what was
	// dropped is still on disk to look at.
	hookMaxUploadFailures = 3
	hookNudgeText         = "This session has had several exchanges. If architectural decisions were made, new repos created, or work was deferred, store them now using memory_store or memory_store_batch. Check what was discussed and whether anything should persist for future sessions."
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
func (opts Options) Run() {
	if opts.Remote == "" {
		return // no remote configured -- silently skip
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf(opts.tool()+": read stdin: %v", err)
	}
	var event hookEvent
	if err := json.Unmarshal(data, &event); err != nil {
		log.Printf(opts.tool()+": invalid JSON on stdin: %v", err)
		return
	}

	c, err := httpclient.NewWithOptions(opts.Remote, opts.APIKey, opts.TLS)
	if err != nil {
		log.Fatalf(opts.tool()+": build client: %v", err)
	}

	// 1. Forward raw hook payload (archive).
	postCtx, postCancel := context.WithTimeout(context.Background(), hookSessionPostTimeout)
	if err := c.PostSessionHook(postCtx, json.RawMessage(data)); err != nil {
		log.Printf(opts.tool()+": post hook: %v", err)
	}
	postCancel()

	// 2-3. Per-session state tracking and nudge emission.
	if event.SessionID != "" {
		state := opts.updateSessionState(event)
		opts.maybeEmitNudge(c, event, state)
	}

	// 4. Drain one pending upload, skipping any session whose Claude Code
	// process is still alive (i.e. transcript is still being written).
	opts.drainOnePendingUpload(c)
}

// updateSessionState reads, mutates, and writes the per-session state file
// keyed on event.SessionID. Returns the post-write state so the caller can
// decide whether to emit a nudge.
func (opts Options) updateSessionState(event hookEvent) sessionState {
	dir := sessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf(opts.tool()+": mkdir sessions: %v", err)
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
			log.Printf(opts.tool()+": write state: %v", err)
		}
	}
	return state
}

// maybeEmitNudge posts a "store your decisions" hint if the session has
// crossed the threshold and hasn't already been nudged. Best-effort --
// failures are logged but don't block the hook.
func (opts Options) maybeEmitNudge(c *httpclient.Client, event hookEvent, state sessionState) {
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
		log.Printf(opts.tool()+": nudge: %v", err)
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
// transcript, and renames to .done on success. Returns after one attempt --
// the next Stop event drains the next entry.
//
// The "is the session still alive" check uses Claude Code's own session
// state files in ~/.claude/sessions/<pid>.json: any session whose pid
// exists and is still running is considered active. This handles the
// /exit+resume case correctly -- Claude Code reuses the same session_id
// on resume but spawns a new process, so as long as that new process is
// alive, we keep skipping the (still-being-appended) transcript. Once
// the resumed process also exits, the next Stop hook from any other
// session will pick the transcript up.
func (opts Options) drainOnePendingUpload(c *httpclient.Client) {
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
			// Transcript file missing -- mark as done so we don't retry forever.
			_ = os.Rename(statePath, strings.TrimSuffix(statePath, ".json")+".done")
			continue
		}
		if info.Size() > hookMaxInlineTranscript {
			// Too large for the hook timeout budget -- spawn a detached
			// subprocess that uploads via --transcript and exits.
			cmd := exec.Command(os.Args[0], append(opts.respawn(), state.TranscriptPath)...)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err == nil {
				_ = cmd.Process.Release()
				_ = os.Rename(statePath, strings.TrimSuffix(statePath, ".json")+".done")
			}
			return // one per invocation
		}

		// Atomic claim -- only one process wins this rename.
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
			state.UploadFailures++
			if state.UploadFailures >= hookMaxUploadFailures {
				log.Printf(opts.tool()+": upload %s: %v -- giving up after %d attempts, marking .failed",
					state.SessionID, err, state.UploadFailures)
				_ = os.Rename(uploading, strings.TrimSuffix(statePath, ".json")+".failed")
				return
			}
			log.Printf(opts.tool()+": upload %s: %v (attempt %d of %d)",
				state.SessionID, err, state.UploadFailures, hookMaxUploadFailures)
			// Restore for retry, carrying the incremented count forward. Written
			// before the rename so a crash in between leaves the entry claimed
			// rather than silently reset to zero attempts.
			if data, mErr := json.MarshalIndent(state, "", "  "); mErr == nil {
				_ = os.WriteFile(uploading, data, 0o644)
			}
			_ = os.Rename(uploading, statePath)
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
// retries -- there's no permanent data loss path.
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
// user) -- we still count it alive because it's a real running process.
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
func (opts Options) RunTranscript(path string) {
	if opts.Remote == "" {
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
		log.Fatalf(opts.tool()+" --transcript: read file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := httpclient.NewWithOptions(opts.Remote, opts.APIKey, opts.TLS)
	if err != nil {
		log.Fatalf(opts.tool()+" --transcript: build client: %v", err)
	}
	if err := c.PostSessionTranscript(ctx, sessionID, cwd, currentPersona(), string(content)); err != nil {
		log.Fatalf(opts.tool()+" --transcript: post: %v", err)
	}
}
