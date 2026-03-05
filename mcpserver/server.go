// Package mcpserver provides an MCP (Model Context Protocol) server that
// exposes a memstore-backed persistent memory system as MCP tools. It is
// designed to give Claude (or any MCP client) durable, searchable memory
// across sessions via hybrid FTS5 + vector search.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GitRunnerFunc queries git for the last modification time of a file within a
// repository. Returns (modTime, true) if found, or (zero, false) if the file
// has no commits or git is unavailable.
type GitRunnerFunc func(ctx context.Context, repoPath, filePath string) (time.Time, bool)

// Config holds optional configuration for MemoryServer.
type Config struct {
	// ProjectPaths maps subject names to filesystem paths for project-surface
	// fact resolution. When a fact has project_subject metadata, the subject
	// is looked up here to find the project root path used for prefix matching
	// against the cwd passed to memory_list_project.
	ProjectPaths map[string]string

	// RepoPaths maps subject names to git repository root paths for drift
	// detection. Used by memory_check_drift when no explicit repo_path is
	// supplied in the tool call.
	RepoPaths map[string]string

	// GitRunner overrides the default git execution for testing.
	// If nil, uses exec.Command("git", ...).
	GitRunner GitRunnerFunc

	// Curator selects the most relevant subset of candidates for a given task.
	// If nil, memory_curate_context uses NopCurator (returns candidates unfiltered).
	Curator memstore.Curator

	// Generator produces text completions for LLM-based operations (e.g. memory_learn).
	// If nil, memory_learn returns an error.
	Generator memstore.Generator
}

// MemoryServer bridges MCP tool calls to a memstore.Store.
type MemoryServer struct {
	store     memstore.Store
	embedder  memstore.Embedder
	config    Config
	gitRunner GitRunnerFunc
	curator   memstore.Curator
	generator memstore.Generator
}

// NewMemoryServer creates a server backed by the given store and embedder.
// The embedder is used to compute embeddings at insert time so search always
// works. Both parameters are required.
func NewMemoryServer(store memstore.Store, embedder memstore.Embedder) *MemoryServer {
	return NewMemoryServerWithConfig(store, embedder, Config{})
}

// NewMemoryServerWithConfig is like NewMemoryServer but accepts additional
// configuration (e.g., project path mappings for memory_list_project).
func NewMemoryServerWithConfig(store memstore.Store, embedder memstore.Embedder, cfg Config) *MemoryServer {
	runner := cfg.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}
	curator := cfg.Curator
	if curator == nil {
		curator = memstore.NopCurator{}
	}
	return &MemoryServer{store: store, embedder: embedder, config: cfg, gitRunner: runner, curator: curator, generator: cfg.Generator}
}

// defaultGitRunner calls git to find the last commit touching filePath within repoPath.
func defaultGitRunner(ctx context.Context, repoPath, filePath string) (time.Time, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "log", "--format=%at", "-1", "--", filePath).Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return time.Time{}, false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(ts, 0), true
}

// --- Input types (MCP SDK infers JSON schemas from struct tags) ---

// StoreInput is the input schema for the memory_store tool.
type StoreInput struct {
	Content    string         `json:"content" jsonschema:"the factual claim or memory to store"`
	Subject    string         `json:"subject" jsonschema:"the entity this fact is about (e.g. a person or project)"`
	Category   string         `json:"category,omitempty" jsonschema:"fact category: preference, identity, project, capability, relationship, world, or note (default: note)"`
	Kind       string         `json:"kind,omitempty" jsonschema:"structural type: convention | failure_mode | invariant | pattern | decision | trigger (empty = unclassified)"`
	Subsystem  string         `json:"subsystem,omitempty" jsonschema:"optional project subsystem this fact belongs to (e.g. feeds, auth, storage)"`
	Metadata   map[string]any `json:"metadata,omitempty" jsonschema:"optional key-value metadata to attach"`
	Supersedes *int64         `json:"supersedes,omitempty" jsonschema:"ID of an existing fact that this new fact replaces (preserves history unlike delete)"`
}

// SearchInput is the input schema for the memory_search tool.
type SearchInput struct {
	Query             string         `json:"query" jsonschema:"natural language search query"`
	Subject           string         `json:"subject,omitempty" jsonschema:"filter results to a specific subject entity"`
	Category          string         `json:"category,omitempty" jsonschema:"filter results to a specific category"`
	Kind              string         `json:"kind,omitempty" jsonschema:"filter by kind: convention, failure_mode, invariant, pattern, decision, trigger (empty = all)"`
	Subsystem         string         `json:"subsystem,omitempty" jsonschema:"filter by subsystem (e.g. feeds, auth)"`
	Limit             int            `json:"limit,omitempty" jsonschema:"maximum number of results (default 10)"`
	IncludeSuperseded bool           `json:"include_superseded,omitempty" jsonschema:"if true, include superseded facts in results (tagged with [SUPERSEDED])"`
	Metadata          map[string]any `json:"metadata,omitempty" jsonschema:"filter by metadata fields (equality match, e.g. {\"source\": \"conversation\"})"`
}

// ListInput is the input schema for the memory_list tool.
type ListInput struct {
	Subject   string         `json:"subject,omitempty" jsonschema:"filter by subject entity"`
	Category  string         `json:"category,omitempty" jsonschema:"filter by category"`
	Kind      string         `json:"kind,omitempty" jsonschema:"filter by kind: convention, failure_mode, invariant, pattern, decision, trigger (empty = all)"`
	Subsystem string         `json:"subsystem,omitempty" jsonschema:"filter by subsystem (e.g. feeds, auth)"`
	Limit     int            `json:"limit,omitempty" jsonschema:"maximum number of results (default 20)"`
	Metadata  map[string]any `json:"metadata,omitempty" jsonschema:"filter by metadata fields (equality match, e.g. {\"source\": \"conversation\"})"`
}

// ListSubsystemsInput is the input schema for the memory_list_subsystems tool.
type ListSubsystemsInput struct {
	Subject string `json:"subject,omitempty" jsonschema:"filter to a specific subject entity (empty = all subjects)"`
}

// CheckDriftInput is the input schema for the memory_check_drift tool.
type CheckDriftInput struct {
	Subject   string `json:"subject,omitempty" jsonschema:"optional subject to scope the check; checks all facts if empty"`
	RepoPath  string `json:"repo_path,omitempty" jsonschema:"git repository root path; falls back to Config.RepoPaths[subject] if omitted"`
	SinceDays int    `json:"since_days,omitempty" jsonschema:"only report facts stale due to changes in the last N days (default 7, 0 = no limit)"`
}

// ListProjectInput is the input schema for the memory_list_project tool.
type ListProjectInput struct {
	CWD string `json:"cwd" jsonschema:"current working directory; facts whose project_path or package_path is a prefix of this are returned"`
}

// ListFileInput is the input schema for the memory_list_file tool.
type ListFileInput struct {
	FilePath   string `json:"file_path" jsonschema:"absolute path of the file being read or edited"`
	SymbolName string `json:"symbol_name,omitempty" jsonschema:"optional symbol (function, type, method) to also load symbol-surface facts for"`
}

// CurateContextInput is the input schema for the memory_curate_context tool.
type CurateContextInput struct {
	Task      string  `json:"task" jsonschema:"description of the task being worked on; used to rank candidate facts by relevance"`
	FactIDs   []int64 `json:"fact_ids" jsonschema:"IDs of candidate facts to evaluate; fetch via memory_get_context or memory_search first"`
	MaxOutput int     `json:"max_output,omitempty" jsonschema:"max facts to return (default 5)"`
}

// GetContextInput is the input schema for the memory_get_context tool.
type GetContextInput struct {
	Task    string `json:"task" jsonschema:"description of the task or feature being worked on"`
	Subject string `json:"subject,omitempty" jsonschema:"optional subject to scope context loading (e.g. a project name)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max total facts in the relevant context section (default 20)"`
}

// DeleteInput is the input schema for the memory_delete tool.
type DeleteInput struct {
	ID int64 `json:"id" jsonschema:"the fact ID to delete"`
}

// SupersedeInput is the input schema for the memory_supersede tool.
type SupersedeInput struct {
	OldID int64 `json:"old_id" jsonschema:"ID of the fact being replaced"`
	NewID int64 `json:"new_id" jsonschema:"ID of the fact that replaces it"`
}

// HistoryInput is the input schema for the memory_history tool.
type HistoryInput struct {
	ID      int64  `json:"id,omitempty" jsonschema:"fact ID to show the supersession chain for"`
	Subject string `json:"subject,omitempty" jsonschema:"subject to show all facts for (including superseded)"`
}

// ConfirmInput is the input schema for the memory_confirm tool.
type ConfirmInput struct {
	ID int64 `json:"id" jsonschema:"the fact ID to confirm"`
}

// UpdateInput is the input schema for the memory_update tool.
type UpdateInput struct {
	ID       int64          `json:"id" jsonschema:"the fact ID to update"`
	Metadata map[string]any `json:"metadata" jsonschema:"metadata keys to set (non-nil) or delete (nil)"`
}

// TaskCreateInput is the input schema for the memory_task_create tool.
type TaskCreateInput struct {
	Content  string `json:"content" jsonschema:"what needs to be done"`
	Scope    string `json:"scope" jsonschema:"task owner: matthew, claude, or collaborative"`
	Priority string `json:"priority,omitempty" jsonschema:"task priority: high, normal, or low (default: normal)"`
	Project  string `json:"project,omitempty" jsonschema:"project name for grouping tasks"`
	Due      string `json:"due,omitempty" jsonschema:"due date (free-form, e.g. 2026-03-01)"`
}

// TaskUpdateInput is the input schema for the memory_task_update tool.
type TaskUpdateInput struct {
	ID     int64  `json:"id" jsonschema:"the task fact ID to update"`
	Status string `json:"status" jsonschema:"new status: pending, in_progress, completed, or cancelled"`
	Note   string `json:"note,omitempty" jsonschema:"optional transition note (e.g. reason for cancellation)"`
}

// TaskListInput is the input schema for the memory_task_list tool.
type TaskListInput struct {
	Scope   string `json:"scope,omitempty" jsonschema:"filter by scope: matthew, claude, or collaborative"`
	Status  string `json:"status,omitempty" jsonschema:"filter by status (default: pending)"`
	Project string `json:"project,omitempty" jsonschema:"filter by project name"`
}

// StatusInput is the input schema for the memory_status tool.
type StatusInput struct{}

// LinkInput is the input schema for the memory_link tool.
type LinkInput struct {
	SourceID      int64          `json:"source_id" jsonschema:"ID of the source fact"`
	TargetID      int64          `json:"target_id" jsonschema:"ID of the target fact"`
	LinkType      string         `json:"link_type,omitempty" jsonschema:"edge type discriminator: passage, event, entrance, reference, etc. (default: reference)"`
	Bidirectional bool           `json:"bidirectional,omitempty" jsonschema:"if true, edge is traversable in both directions"`
	Label         string         `json:"label,omitempty" jsonschema:"human-readable description of this connection"`
	Metadata      map[string]any `json:"metadata,omitempty" jsonschema:"domain-specific properties for this edge"`
}

// UnlinkInput is the input schema for the memory_unlink tool.
type UnlinkInput struct {
	LinkID int64 `json:"link_id" jsonschema:"ID of the link to delete"`
}

// GetLinksInput is the input schema for the memory_get_links tool.
type GetLinksInput struct {
	FactID    int64  `json:"fact_id" jsonschema:"ID of the fact to get links for"`
	Direction string `json:"direction,omitempty" jsonschema:"outbound (default), inbound, or both"`
	LinkType  string `json:"link_type,omitempty" jsonschema:"filter to a specific link type (empty = all types)"`
}

// UpdateLinkInput is the input schema for the memory_update_link tool.
type UpdateLinkInput struct {
	LinkID   int64          `json:"link_id" jsonschema:"ID of the link to update"`
	Label    string         `json:"label,omitempty" jsonschema:"new label (empty leaves existing label unchanged)"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"metadata keys to set (non-nil) or delete (nil)"`
}

// SuggestAgentInput is the input schema for the memory_suggest_agent tool.
type SuggestAgentInput struct {
	Task    string `json:"task" jsonschema:"description of the work to be done"`
	Subject string `json:"subject,omitempty" jsonschema:"scope to a specific project's routing rules (falls back to global rules if no project-specific match)"`
}

// LearnInput is the input schema for the memory_learn tool.
type LearnInput struct {
	RepoPath     string   `json:"repo_path" jsonschema:"absolute path to the Go repository root"`
	Subject      string   `json:"subject" jsonschema:"project subject name (e.g. memstore)"`
	ModulePath   string   `json:"module_path,omitempty" jsonschema:"Go module path; empty = parse from go.mod"`
	MaxFileSize  int64    `json:"max_file_size,omitempty" jsonschema:"skip files larger than this in bytes (default 262144)"`
	ExcludeDirs  []string `json:"exclude_dirs,omitempty" jsonschema:"directories to skip (default: vendor, testdata, .git)"`
	Force        bool     `json:"force,omitempty" jsonschema:"re-learn all files even if unchanged"`
	ExcludeTests bool     `json:"exclude_tests,omitempty" jsonschema:"exclude _test.go files from ingestion"`
}

// --- Tool registration ---

// Register adds all memory tools to the given MCP server.
func (ms *MemoryServer) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_store",
		Description: `Store a fact or memory. Persists across sessions with automatic embedding for semantic search. Use this whenever you learn something worth remembering about the user, their projects, preferences, or any durable knowledge.

Store aggressively — it is better to store something and supersede it later than to lose it. Good candidates: user preferences, project decisions, technical choices, names, relationships, workflow habits, things the user corrects you on, environment details.

Conventions:
- subject: lowercase, singular entity name (e.g. "matthew", "memstore", "home-server"). This is the primary lookup key — be consistent.
- category: one of preference, identity, project, capability, relationship, world, note. Use "note" as the catch-all.
- metadata: use for attribution (source), confidence, temporal bounds (valid_from/valid_until), or any structured data.
- supersedes: pass the ID of the fact this replaces. The old fact is preserved in history. Always prefer superseding over deleting.
- source_files: add {"source_files": ["relative/path/to/file.go", ...]} in metadata when a fact documents code behavior. This enables drift detection: memory_check_drift will warn when those files are modified after the fact was last confirmed.`,
	}, ms.HandleStore)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_search",
		Description: `Search stored memories using hybrid full-text and semantic search. Returns ranked results with relevance scores. Use this to recall information from previous sessions.

Search early and often — check what you already know before asking the user to repeat themselves. Search at the start of a conversation if the user's identity or project context is unclear.

Set include_superseded=true when you need to understand how a fact has changed over time, or to find old information that may have been prematurely superseded.`,
	}, ms.HandleSearch)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_list",
		Description: `Browse stored memories with optional subject and category filters. Unlike search, this does not require a query — use it to see what you know about a topic.

Use this when you want a complete picture of a subject rather than matching a specific query. Good for: "what do I know about this user?", "what preferences are stored?", getting an overview before a task.`,
	}, ms.HandleList)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_delete",
		Description: `Delete a specific memory by its ID. Use this to remove outdated or incorrect information.

Prefer memory_supersede or memory_store with the 'supersedes' parameter instead — these preserve the old fact in history. Only delete facts that are genuinely wrong or harmful, not just outdated.`,
	}, ms.HandleDelete)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_supersede",
		Description: `Mark an existing fact as superseded by a newer fact. Both facts must already exist. The old fact is preserved in history but excluded from normal search results.

Use this when you discover a stored fact is outdated and you've already stored the replacement. For a single-step "store and supersede", use memory_store with the supersedes parameter instead.`,
	}, ms.HandleSupersede)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_history",
		Description: `Show the supersession history for a fact (by ID) or all facts for a subject (by subject). Reveals how knowledge has evolved over time, including superseded facts with their replacement chain.

Use by ID to trace a specific fact's lineage. Use by subject for a complete audit of everything stored about an entity.`,
	}, ms.HandleHistory)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_confirm",
		Description: `Confirm that a fact is still accurate. Increments its confirmation count and updates the last-confirmed timestamp.

Use this when:
- You retrieve a fact and the user's behavior or statement corroborates it
- The user explicitly confirms stored information is correct
- You use a fact in your response and it proves accurate

Facts with high confirmation counts are well-tested knowledge. Facts with zero confirmations are unverified. This signal helps prioritize what to trust.`,
	}, ms.HandleConfirm)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_status",
		Description: "Show memory store statistics: total active facts, and breakdown by subject and category.",
	}, ms.HandleStatus)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_update",
		Description: `Update metadata on an existing fact without replacing the fact itself. Keys with non-nil values are set; keys with nil values are deleted.

Use this for status transitions, adding surface flags, or updating structured metadata. Does not create supersession history — use memory_store with supersedes for content changes.`,
	}, ms.HandleUpdate)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_task_create",
		Description: `Create a task with enforced metadata schema. Tasks are stored as facts with subject="todo" and structured metadata (kind, scope, status, priority, surface).

Scope controls ownership:
- "matthew" — user's task (reminders, personal TODOs)
- "claude" — agent's task (follow-ups, deferred work)
- "collaborative" — shared between user and agent

Tasks with status "pending" or "in_progress" have surface="startup" so they appear at session start via memory_list(metadata: {surface: "startup"}).`,
	}, ms.HandleTaskCreate)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_task_update",
		Description: `Transition a task's status. Only works on facts with metadata.kind="task".

Valid statuses: pending, in_progress, completed, cancelled.
Completing or cancelling a task removes the "surface" flag so it no longer appears at startup.
Optional note is stored as metadata.note for transition context.`,
	}, ms.HandleTaskUpdate)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_task_list",
		Description: `List tasks with optional filters. Defaults to showing pending tasks across all scopes.

Filters: scope (matthew/claude/collaborative), status, project.
Output is task-focused: shows status, scope, priority, content, and due date.`,
	}, ms.HandleTaskList)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_link",
		Description: `Create a directed graph edge between two facts.

Use this to represent explicit connections that cannot be inferred from content alone:
- Map passages: secret doors, teleporters, one-way exits, building entrances
- Event triggers: traps or encounters associated with a location
- Provenance: derived_from edges so stale derived facts can be flagged
- Any domain relationship where the edge itself has properties

link_type is a short discriminator string. Suggested types: passage, event, entrance, reference, derived_from.
Set bidirectional=true for passages traversable in both directions (e.g. a corridor).
label is a human-readable description of the specific edge (e.g. "secret door behind bookshelf").
metadata holds edge-specific properties (e.g. {"hidden": true, "dc": 15} for a perception check).`,
	}, ms.HandleLink)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_unlink",
		Description: `Delete a link by ID. Removes the edge but leaves both facts intact.`,
	}, ms.HandleUnlink)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_get_links",
		Description: `Get all links touching a fact, with neighbor fact summaries.

direction controls which edges are returned:
- "outbound" (default): edges where the fact is the source, plus bidirectional edges where it is the target
- "inbound": edges where the fact is the target, plus bidirectional edges where it is the source
- "both": all edges touching the fact regardless of directionality

filter by link_type to retrieve only edges of a specific kind (e.g. "passage" for map navigation).
Each result includes the link metadata and a summary of the neighbor fact (ID, subject, content preview).`,
	}, ms.HandleGetLinks)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_update_link",
		Description: `Update the label and/or metadata of an existing link.

An empty label leaves the existing label unchanged.
Metadata keys with non-nil values are set; keys with nil values are deleted.
Use this to reveal hidden passages, change conditions, or annotate edges after creation.`,
	}, ms.HandleUpdateLink)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_list_subsystems",
		Description: `List all distinct subsystem values present in the store, optionally filtered by subject.

Use this to discover what structured knowledge exists for a project before starting a task.
Returns a sorted list of subsystem names (e.g. ["auth", "feeds", "storage"]).

Example: memory_list_subsystems(subject="herald") → all subsystems with facts stored for herald.`,
	}, ms.HandleListSubsystems)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_get_context",
		Description: `Load relevant context for a task without requiring the caller to know what to search for.

Given a task description, returns three categories of facts:
1. Invariants — rules and constraints that always apply when touching the subsystems involved
2. Failure modes — known symptom/cause/fix patterns for those subsystems
3. Relevant context — top search results for the task description

Use this at the start of any non-trivial implementation task to surface specifications,
constraints, and known failure patterns before writing code. Trigger facts (kind=trigger)
that match keywords in the task description are also included.

Example: memory_get_context(task="add retry logic to feed fetcher", subject="herald")`,
	}, ms.HandleGetContext)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_list_project",
		Description: `Load project-surface and package-surface facts for the current working directory.

Two warm-tier surface types are returned:
- surface=project: facts whose project_path (or project_subject) prefix-matches cwd
- surface=package: facts whose package_path prefix-matches cwd (sub-project granularity)

Metadata conventions:
  project-tier: {"surface":"project","project_path":"/abs/path/to/repo"}
  package-tier: {"surface":"package","package_path":"/abs/path/to/package/dir"}

Use at session start to auto-load architecture overviews, build conventions, subsystem maps,
and package-level responsibilities. Does not pollute startup budget in unrelated sessions.

Complements memory_get_context (call this first) and memory_list_file (call before editing).

Hook integration: the SessionStart hook calls this automatically via the memstore CLI
  memstore list-project --cwd <working_directory>

Example: memory_list_project(cwd="/home/matthew/go/src/github.com/matthewjhunter/herald/internal/feeds")
  → returns both herald project facts and feeds package facts`,
	}, ms.HandleListProject)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_check_drift",
		Description: `Check whether facts that document code behavior are still accurate.

Facts with source_files metadata (e.g. {"source_files": ["internal/feeds/fetcher.go"]})
are checked against the git log of those files. A fact is STALE if any of its source
files were modified in git after the fact was last confirmed (or created).

Returns:
- ⚠ STALE: facts where source files changed after the fact was last verified
- ✓ CURRENT: facts whose source files are unchanged since last confirmation
- count of facts with no source_files metadata (not checked)

Use this after pulling recent changes, before starting work on a subsystem, or as part
of a regular maintenance pass. Run memory_confirm <id> after verifying a stale fact is
still accurate.

Requires a git repository path. Configured via Config.RepoPaths or passed explicitly.

Metadata convention for source_files:
  {"source_files": ["relative/path/to/file.go", "another/file.go"]}
  Paths are relative to the repository root (repo_path).

Example: memory_check_drift(subject="herald", repo_path="/home/matthew/go/src/herald", since_days=7)`,
	}, ms.HandleCheckDrift)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_list_file",
		Description: `Load file-surface and symbol-surface facts for a specific file.

Two warm-tier surface types are returned:
- surface=file: facts about the file as a whole (role, patterns, known issues)
- surface=symbol: facts about specific functions/types/methods within the file

Metadata conventions:
  file-tier:   {"surface":"file","file_path":"/abs/path/to/file.go"}
  symbol-tier: {"surface":"symbol","file_path":"/abs/path/to/file.go","symbol_name":"FuncName"}

These facts are produced by memory_learn when it ingests a codebase at file/symbol depth.
They describe what a file does, key invariants, known failure patterns, and function-level
contracts — surfaced just-in-time when that file is opened.

Use before reading or editing a file to surface constraints before making changes.

Hook integration: PreToolUse Read/Edit hooks call this automatically via the memstore CLI
  memstore list-file --file <file_path> [--symbol <symbol_name>]

Example: memory_list_file(file_path="/home/matthew/go/src/herald/internal/feeds/fetcher.go")
  → returns file overview + all symbol facts for that file

Example: memory_list_file(file_path="...", symbol_name="FetchFeed")
  → returns file overview + only FetchFeed symbol facts`,
	}, ms.HandleListFile)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_curate_context",
		Description: `Filter a candidate set of facts down to the most relevant subset for a task.

Call this after memory_get_context or memory_search to reduce noise before injecting
context into your working memory. A fast curation model reads the candidates and returns
only the facts that are essential to the task, with a brief rationale.

Typical flow:
  1. memory_get_context(task=...) → get candidate fact IDs
  2. memory_curate_context(task=..., fact_ids=[...], max_output=5) → get curated subset

Falls back gracefully: if no curation model is configured, returns the first max_output
candidates unchanged.

Example: memory_curate_context(
  task="add retry logic to the RSS feed fetcher",
  fact_ids=[12, 34, 56, 78, 90, 101, 102],
  max_output=4
)`,
	}, ms.HandleCurateContext)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_learn",
		Description: `Ingest a Go codebase into structured facts with a four-level containment graph (repo → package → file → symbol).

Walks the repository, parses Go AST to extract symbols, uses LLM to generate summaries at file/package/repo levels, and creates containment links between all levels. Integrates with warm-tier surfacing (memory_list_project, memory_list_file).

Re-learning is incremental: unchanged files (by content hash) are skipped unless force=true. Changed files are re-summarized and old facts are superseded.

Requires a configured generator model. Scoped to Go codebases only.`,
	}, ms.HandleLearn)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_suggest_agent",
		Description: `Recommend which specialist agent to use for a given task based on stored agent-to-domain mappings.

Agent routing facts are stored as subsystem="agent-routing" with metadata containing agent_name and domains (list of keywords). This tool searches those facts using the task description, scores each agent by domain keyword overlap, and returns a ranked list of suggestions.

Store agent routing facts with memory_store:
  subject: project name or "global", subsystem: "agent-routing", kind: "convention"
  metadata: {"agent_name": "security-reviewer", "domains": ["security", "auth", "crypto"]}
  content: description of when to use this agent

Returns: ranked suggestions with agent_name, score, and rationale.
If no agent-routing facts exist, returns a message suggesting how to seed them.`,
	}, ms.HandleSuggestAgent)
}

// --- Handlers ---

func (ms *MemoryServer) HandleStore(ctx context.Context, _ *mcp.CallToolRequest, input StoreInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(input.Content) == "" {
		return textResult("Error: content is required", true), nil, nil
	}
	if strings.TrimSpace(input.Subject) == "" {
		return textResult("Error: subject is required", true), nil, nil
	}

	category := strings.TrimSpace(input.Category)
	if category == "" {
		category = "note"
	}

	// Dedup check.
	exists, err := ms.store.Exists(ctx, input.Content, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error checking for duplicates: %v", err), true), nil, nil
	}
	if exists {
		return textResult("Already stored (duplicate).", false), nil, nil
	}

	// Compute embedding (skip in daemon mode — the server handles embeddings).
	var emb []float32
	if ms.embedder != nil {
		var err error
		emb, err = memstore.Single(ctx, ms.embedder, input.Content)
		if err != nil {
			return textResult(fmt.Sprintf("Error computing embedding: %v", err), true), nil, nil
		}
	}

	fact := memstore.Fact{
		Content:   input.Content,
		Subject:   input.Subject,
		Category:  category,
		Kind:      strings.TrimSpace(input.Kind),
		Subsystem: strings.TrimSpace(input.Subsystem),
		Embedding: emb,
	}
	if len(input.Metadata) > 0 {
		metaJSON, err := json.Marshal(input.Metadata)
		if err != nil {
			return textResult(fmt.Sprintf("Error encoding metadata: %v", err), true), nil, nil
		}
		fact.Metadata = metaJSON
	}

	id, err := ms.store.Insert(ctx, fact)
	if err != nil {
		return textResult(fmt.Sprintf("Error storing fact: %v", err), true), nil, nil
	}

	msg := fmt.Sprintf("Stored (id=%d, subject=%q, category=%q).", id, input.Subject, category)

	// Handle supersession after successful insert.
	if input.Supersedes != nil {
		if err := ms.store.Supersede(ctx, *input.Supersedes, id); err != nil {
			msg += fmt.Sprintf(" Warning: supersession of fact %d failed: %v", *input.Supersedes, err)
		} else {
			msg += fmt.Sprintf(" Superseded fact %d.", *input.Supersedes)
		}
	}

	return textResult(msg, false), nil, nil
}

func (ms *MemoryServer) HandleSearch(ctx context.Context, _ *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(input.Query) == "" {
		return textResult("Error: query is required", true), nil, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	opts := memstore.SearchOpts{
		MaxResults:      limit,
		Subject:         input.Subject,
		Category:        input.Category,
		Kind:            input.Kind,
		Subsystem:       input.Subsystem,
		OnlyActive:      !input.IncludeSuperseded,
		MetadataFilters: metadataFilters(input.Metadata),
		// Stable facts (preference, identity) don't decay.
		// Ephemeral notes get 30-day half-life.
		CategoryDecay: map[string]time.Duration{
			"note": 720 * time.Hour, // 30 days
		},
	}

	results, err := ms.store.Search(ctx, input.Query, opts)
	if err != nil {
		return textResult(fmt.Sprintf("Error searching: %v", err), true), nil, nil
	}

	if len(results) == 0 {
		return textResult("No matching memories found.", false), nil, nil
	}

	// Auto-touch: bump use_count for all returned facts.
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.Fact.ID
	}
	_ = ms.store.Touch(ctx, ids) // best-effort; don't fail the search

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] (id=%d, score=%.3f, used=%d, confirmed=%d) %s | %s",
			i+1, r.Fact.ID, r.Combined,
			r.Fact.UseCount+1, r.Fact.ConfirmedCount, // +1 because Touch just ran
			r.Fact.Subject, r.Fact.Category)
		if r.Fact.Kind != "" {
			fmt.Fprintf(&b, " | kind=%s", r.Fact.Kind)
		}
		if r.Fact.Subsystem != "" {
			fmt.Fprintf(&b, " | subsystem=%s", r.Fact.Subsystem)
		}
		if r.Fact.SupersededBy != nil {
			fmt.Fprintf(&b, " [SUPERSEDED by %d]", *r.Fact.SupersededBy)
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "    %s\n", r.Fact.Content)
		if len(r.Fact.Metadata) > 0 && string(r.Fact.Metadata) != "null" {
			fmt.Fprintf(&b, "    metadata: %s\n", string(r.Fact.Metadata))
		}
		fmt.Fprintln(&b)
	}

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleList(ctx context.Context, _ *mcp.CallToolRequest, input ListInput) (*mcp.CallToolResult, any, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}

	opts := memstore.QueryOpts{
		Subject:         input.Subject,
		Category:        input.Category,
		Kind:            input.Kind,
		Subsystem:       input.Subsystem,
		OnlyActive:      true,
		Limit:           limit,
		MetadataFilters: metadataFilters(input.Metadata),
	}

	facts, err := ms.store.List(ctx, opts)
	if err != nil {
		return textResult(fmt.Sprintf("Error listing: %v", err), true), nil, nil
	}

	if len(facts) == 0 {
		return textResult("No memories found.", false), nil, nil
	}

	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "[id=%d, used=%d, confirmed=%d] %s | %s",
			f.ID, f.UseCount, f.ConfirmedCount,
			f.Subject, f.Category)
		if f.Kind != "" {
			fmt.Fprintf(&b, " | kind=%s", f.Kind)
		}
		if f.Subsystem != "" {
			fmt.Fprintf(&b, " | subsystem=%s", f.Subsystem)
		}
		fmt.Fprintf(&b, " | %s\n", f.CreatedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "  %s\n", f.Content)
		if len(f.Metadata) > 0 && string(f.Metadata) != "null" {
			fmt.Fprintf(&b, "  metadata: %s\n", string(f.Metadata))
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "%d memories listed.", len(facts))

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleDelete(ctx context.Context, _ *mcp.CallToolRequest, input DeleteInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 {
		return textResult("Error: id must be a positive integer", true), nil, nil
	}

	err := ms.store.Delete(ctx, input.ID)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Deleted memory %d.", input.ID), false), nil, nil
}

func (ms *MemoryServer) HandleStatus(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, any, error) {
	count, err := ms.store.ActiveCount(ctx)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	// Get subject and category breakdown.
	facts, err := ms.store.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	subjects := make(map[string]int)
	categories := make(map[string]int)
	kinds := make(map[string]int)
	for _, f := range facts {
		subjects[f.Subject]++
		categories[f.Category]++
		if f.Kind != "" {
			kinds[f.Kind]++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Active memories: %d\n\n", count)

	if len(categories) > 0 {
		fmt.Fprintln(&b, "By category:")
		for cat, n := range categories {
			fmt.Fprintf(&b, "  %s: %d\n", cat, n)
		}
		fmt.Fprintln(&b)
	}

	if len(kinds) > 0 {
		fmt.Fprintln(&b, "By kind:")
		for k, n := range kinds {
			fmt.Fprintf(&b, "  %s: %d\n", k, n)
		}
		fmt.Fprintln(&b)
	}

	if len(subjects) > 0 {
		fmt.Fprintln(&b, "By subject:")
		for subj, n := range subjects {
			fmt.Fprintf(&b, "  %s: %d\n", subj, n)
		}
	}

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleSupersede(ctx context.Context, _ *mcp.CallToolRequest, input SupersedeInput) (*mcp.CallToolResult, any, error) {
	if input.OldID <= 0 || input.NewID <= 0 {
		return textResult("Error: both old_id and new_id must be positive integers", true), nil, nil
	}
	if input.OldID == input.NewID {
		return textResult("Error: old_id and new_id must be different", true), nil, nil
	}

	// Validate both facts exist.
	oldFact, err := ms.store.Get(ctx, input.OldID)
	if err != nil {
		return textResult(fmt.Sprintf("Error looking up fact %d: %v", input.OldID, err), true), nil, nil
	}
	if oldFact == nil {
		return textResult(fmt.Sprintf("Error: fact %d not found", input.OldID), true), nil, nil
	}
	if oldFact.SupersededBy != nil {
		return textResult(fmt.Sprintf("Error: fact %d is already superseded by fact %d", input.OldID, *oldFact.SupersededBy), true), nil, nil
	}

	newFact, err := ms.store.Get(ctx, input.NewID)
	if err != nil {
		return textResult(fmt.Sprintf("Error looking up fact %d: %v", input.NewID, err), true), nil, nil
	}
	if newFact == nil {
		return textResult(fmt.Sprintf("Error: fact %d not found", input.NewID), true), nil, nil
	}

	if err := ms.store.Supersede(ctx, input.OldID, input.NewID); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Superseded fact %d with fact %d.\n  Old: %s\n  New: %s",
		input.OldID, input.NewID, oldFact.Content, newFact.Content), false), nil, nil
}

func (ms *MemoryServer) HandleHistory(ctx context.Context, _ *mcp.CallToolRequest, input HistoryInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 && strings.TrimSpace(input.Subject) == "" {
		return textResult("Error: provide either id or subject", true), nil, nil
	}

	entries, err := ms.store.History(ctx, input.ID, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	if len(entries) == 0 {
		return textResult("No history found.", false), nil, nil
	}

	var b strings.Builder
	for _, e := range entries {
		status := "ACTIVE"
		if e.Fact.SupersededBy != nil {
			status = fmt.Sprintf("SUPERSEDED by %d", *e.Fact.SupersededBy)
		}
		fmt.Fprintf(&b, "[%d/%d] (id=%d, used=%d, confirmed=%d) %s | %s | %s | %s\n",
			e.Position+1, e.ChainLength, e.Fact.ID,
			e.Fact.UseCount, e.Fact.ConfirmedCount,
			e.Fact.Subject, e.Fact.Category,
			status, e.Fact.CreatedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "  %s\n", e.Fact.Content)
		if len(e.Fact.Metadata) > 0 && string(e.Fact.Metadata) != "null" {
			fmt.Fprintf(&b, "  metadata: %s\n", string(e.Fact.Metadata))
		}
		fmt.Fprintln(&b)
	}

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleConfirm(ctx context.Context, _ *mcp.CallToolRequest, input ConfirmInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 {
		return textResult("Error: id must be a positive integer", true), nil, nil
	}

	if err := ms.store.Confirm(ctx, input.ID); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	// Re-fetch to show the updated count.
	fact, err := ms.store.Get(ctx, input.ID)
	if err != nil || fact == nil {
		return textResult(fmt.Sprintf("Confirmed fact %d.", input.ID), false), nil, nil
	}

	return textResult(fmt.Sprintf("Confirmed fact %d (count=%d). %s",
		input.ID, fact.ConfirmedCount, fact.Content), false), nil, nil
}

// --- Validation helpers ---

var validScopes = map[string]bool{
	"matthew":       true,
	"claude":        true,
	"collaborative": true,
}

var validPriorities = map[string]bool{
	"high":   true,
	"normal": true,
	"low":    true,
}

var validTaskStatuses = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"completed":   true,
	"cancelled":   true,
}

// --- New handlers ---

func (ms *MemoryServer) HandleUpdate(ctx context.Context, _ *mcp.CallToolRequest, input UpdateInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 {
		return textResult("Error: id must be a positive integer", true), nil, nil
	}
	if len(input.Metadata) == 0 {
		return textResult("Error: metadata must contain at least one key", true), nil, nil
	}

	if err := ms.store.UpdateMetadata(ctx, input.ID, input.Metadata); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Updated metadata on fact %d.", input.ID), false), nil, nil
}

func (ms *MemoryServer) HandleTaskCreate(ctx context.Context, _ *mcp.CallToolRequest, input TaskCreateInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(input.Content) == "" {
		return textResult("Error: content is required", true), nil, nil
	}
	if !validScopes[input.Scope] {
		return textResult(fmt.Sprintf("Error: scope must be one of: matthew, claude, collaborative (got %q)", input.Scope), true), nil, nil
	}

	priority := input.Priority
	if priority == "" {
		priority = "normal"
	}
	if !validPriorities[priority] {
		return textResult(fmt.Sprintf("Error: priority must be one of: high, normal, low (got %q)", priority), true), nil, nil
	}

	meta := map[string]any{
		"kind":     "task",
		"scope":    input.Scope,
		"status":   "pending",
		"priority": priority,
		"surface":  "startup",
	}
	if input.Project != "" {
		meta["project"] = input.Project
	}
	if input.Due != "" {
		meta["due"] = input.Due
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return textResult(fmt.Sprintf("Error encoding metadata: %v", err), true), nil, nil
	}

	// Compute embedding for searchability (skip in daemon mode).
	var emb []float32
	if ms.embedder != nil {
		emb, err = memstore.Single(ctx, ms.embedder, input.Content)
		if err != nil {
			return textResult(fmt.Sprintf("Error computing embedding: %v", err), true), nil, nil
		}
	}

	id, err := ms.store.Insert(ctx, memstore.Fact{
		Content:   input.Content,
		Subject:   "todo",
		Category:  "note",
		Kind:      "task",
		Metadata:  metaJSON,
		Embedding: emb,
	})
	if err != nil {
		return textResult(fmt.Sprintf("Error creating task: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Created task (id=%d, scope=%s, priority=%s).", id, input.Scope, priority), false), nil, nil
}

func (ms *MemoryServer) HandleTaskUpdate(ctx context.Context, _ *mcp.CallToolRequest, input TaskUpdateInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 {
		return textResult("Error: id must be a positive integer", true), nil, nil
	}
	if !validTaskStatuses[input.Status] {
		return textResult(fmt.Sprintf("Error: status must be one of: pending, in_progress, completed, cancelled (got %q)", input.Status), true), nil, nil
	}

	// Verify the fact is a task.
	fact, err := ms.store.Get(ctx, input.ID)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}
	if fact == nil {
		return textResult(fmt.Sprintf("Error: fact %d not found", input.ID), true), nil, nil
	}

	var meta map[string]any
	if len(fact.Metadata) > 0 {
		json.Unmarshal(fact.Metadata, &meta)
	}
	if meta == nil || meta["kind"] != "task" {
		return textResult(fmt.Sprintf("Error: fact %d is not a task", input.ID), true), nil, nil
	}

	patch := map[string]any{"status": input.Status}

	// Remove surface flag on terminal statuses.
	if input.Status == "completed" || input.Status == "cancelled" {
		patch["surface"] = nil
	}

	if input.Note != "" {
		patch["note"] = input.Note
	}

	if err := ms.store.UpdateMetadata(ctx, input.ID, patch); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Task %d → %s.", input.ID, input.Status), false), nil, nil
}

func (ms *MemoryServer) HandleTaskList(ctx context.Context, _ *mcp.CallToolRequest, input TaskListInput) (*mcp.CallToolResult, any, error) {
	status := input.Status
	if status == "" {
		status = "pending"
	}

	filters := []memstore.MetadataFilter{
		{Key: "status", Op: "=", Value: status},
	}
	if input.Scope != "" {
		filters = append(filters, memstore.MetadataFilter{Key: "scope", Op: "=", Value: input.Scope})
	}
	if input.Project != "" {
		filters = append(filters, memstore.MetadataFilter{Key: "project", Op: "=", Value: input.Project})
	}

	facts, err := ms.store.List(ctx, memstore.QueryOpts{
		Kind:            "task",
		OnlyActive:      true,
		MetadataFilters: filters,
	})
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	if len(facts) == 0 {
		return textResult("No tasks found.", false), nil, nil
	}

	var b strings.Builder
	for _, f := range facts {
		var meta map[string]any
		if len(f.Metadata) > 0 {
			json.Unmarshal(f.Metadata, &meta)
		}

		scope, _ := meta["scope"].(string)
		priority, _ := meta["priority"].(string)
		due, _ := meta["due"].(string)

		fmt.Fprintf(&b, "[id=%d] [%s] %s (scope=%s, priority=%s",
			f.ID, status, f.Content, scope, priority)
		if due != "" {
			fmt.Fprintf(&b, ", due=%s", due)
		}
		b.WriteString(")\n")
	}
	fmt.Fprintf(&b, "\n%d task(s).", len(facts))

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleListSubsystems(ctx context.Context, _ *mcp.CallToolRequest, input ListSubsystemsInput) (*mcp.CallToolResult, any, error) {
	subsystems, err := ms.store.ListSubsystems(ctx, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}
	if len(subsystems) == 0 {
		if input.Subject != "" {
			return textResult(fmt.Sprintf("No subsystems found for subject %q.", input.Subject), false), nil, nil
		}
		return textResult("No subsystems found.", false), nil, nil
	}
	var b strings.Builder
	for _, s := range subsystems {
		fmt.Fprintln(&b, s)
	}
	fmt.Fprintf(&b, "\n%d subsystem(s).", len(subsystems))
	return textResult(b.String(), false), nil, nil
}

// metadataFilters converts a map[string]any (from MCP input) to memstore.MetadataFilter
// equality conditions.
func metadataFilters(m map[string]any) []memstore.MetadataFilter {
	if len(m) == 0 {
		return nil
	}
	filters := make([]memstore.MetadataFilter, 0, len(m))
	for k, v := range m {
		filters = append(filters, memstore.MetadataFilter{Key: k, Op: "=", Value: v})
	}
	return filters
}

// --- Link handlers ---

func (ms *MemoryServer) HandleLink(ctx context.Context, _ *mcp.CallToolRequest, input LinkInput) (*mcp.CallToolResult, any, error) {
	if input.SourceID <= 0 {
		return textResult("Error: source_id is required", true), nil, nil
	}
	if input.TargetID <= 0 {
		return textResult("Error: target_id is required", true), nil, nil
	}
	linkType := strings.TrimSpace(input.LinkType)
	if linkType == "" {
		linkType = "reference"
	}

	id, err := ms.store.LinkFacts(ctx, input.SourceID, input.TargetID, linkType, input.Bidirectional, input.Label, input.Metadata)
	if err != nil {
		return textResult(fmt.Sprintf("Error creating link: %v", err), true), nil, nil
	}

	dir := "directed"
	if input.Bidirectional {
		dir = "bidirectional"
	}
	return textResult(fmt.Sprintf("Linked (link_id=%d, %d->%d, type=%q, %s).", id, input.SourceID, input.TargetID, linkType, dir), false), nil, nil
}

func (ms *MemoryServer) HandleUnlink(ctx context.Context, _ *mcp.CallToolRequest, input UnlinkInput) (*mcp.CallToolResult, any, error) {
	if input.LinkID <= 0 {
		return textResult("Error: link_id is required", true), nil, nil
	}
	if err := ms.store.DeleteLink(ctx, input.LinkID); err != nil {
		return textResult(fmt.Sprintf("Error deleting link: %v", err), true), nil, nil
	}
	return textResult(fmt.Sprintf("Deleted link %d.", input.LinkID), false), nil, nil
}

func (ms *MemoryServer) HandleGetLinks(ctx context.Context, _ *mcp.CallToolRequest, input GetLinksInput) (*mcp.CallToolResult, any, error) {
	if input.FactID <= 0 {
		return textResult("Error: fact_id is required", true), nil, nil
	}

	direction := memstore.LinkOutbound
	switch strings.ToLower(strings.TrimSpace(input.Direction)) {
	case "inbound":
		direction = memstore.LinkInbound
	case "both":
		direction = memstore.LinkBoth
	}

	var linkTypes []string
	if t := strings.TrimSpace(input.LinkType); t != "" {
		linkTypes = []string{t}
	}

	links, err := ms.store.GetLinks(ctx, input.FactID, direction, linkTypes...)
	if err != nil {
		return textResult(fmt.Sprintf("Error getting links: %v", err), true), nil, nil
	}
	if len(links) == 0 {
		return textResult("No links found.", false), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d link(s) for fact %d:\n", len(links), input.FactID)
	for _, l := range links {
		bidi := ""
		if l.Bidirectional {
			bidi = " [bidirectional]"
		}
		fmt.Fprintf(&b, "\n[link_id=%d] %d -> %d | type=%q%s\n", l.ID, l.SourceID, l.TargetID, l.LinkType, bidi)
		if l.Label != "" {
			fmt.Fprintf(&b, "  label: %s\n", l.Label)
		}
		if len(l.Metadata) > 0 && string(l.Metadata) != "null" {
			fmt.Fprintf(&b, "  metadata: %s\n", string(l.Metadata))
		}

		// Fetch neighbor fact summary.
		neighborID := l.TargetID
		if l.SourceID != input.FactID {
			neighborID = l.SourceID
		}
		if f, err := ms.store.Get(ctx, neighborID); err == nil && f != nil {
			preview := f.Content
			if len(preview) > 100 {
				preview = preview[:100] + "…"
			}
			fmt.Fprintf(&b, "  neighbor: id=%d subject=%q — %s\n", f.ID, f.Subject, preview)
		}
	}

	return textResult(strings.TrimRight(b.String(), "\n"), false), nil, nil
}

func (ms *MemoryServer) HandleUpdateLink(ctx context.Context, _ *mcp.CallToolRequest, input UpdateLinkInput) (*mcp.CallToolResult, any, error) {
	if input.LinkID <= 0 {
		return textResult("Error: link_id is required", true), nil, nil
	}
	if err := ms.store.UpdateLink(ctx, input.LinkID, input.Label, input.Metadata); err != nil {
		return textResult(fmt.Sprintf("Error updating link: %v", err), true), nil, nil
	}
	return textResult(fmt.Sprintf("Updated link %d.", input.LinkID), false), nil, nil
}

// textResult builds a CallToolResult with a single text content block.
func (ms *MemoryServer) HandleGetContext(ctx context.Context, _ *mcp.CallToolRequest, input GetContextInput) (*mcp.CallToolResult, any, error) {
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return textResult("Error: task is required", true), nil, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}

	// Hybrid search for the task description; fall back to FTS if no embedder configured.
	searchOpts := memstore.SearchOpts{
		MaxResults: limit,
		Subject:    input.Subject,
		OnlyActive: true,
	}
	searchResults, err := ms.store.Search(ctx, task, searchOpts)
	if err != nil {
		searchResults, err = ms.store.SearchFTS(ctx, task, searchOpts)
		if err != nil {
			return textResult(fmt.Sprintf("Error searching: %v", err), true), nil, nil
		}
	}

	// Collect unique subsystems from search results.
	seenSubsystems := make(map[string]bool)
	for _, r := range searchResults {
		if r.Fact.Subsystem != "" {
			seenSubsystems[r.Fact.Subsystem] = true
		}
	}

	seen := make(map[int64]bool)

	// Load invariants and failure_mode facts for each touched subsystem.
	var invariants, failureModes []memstore.Fact
	for sub := range seenSubsystems {
		inv, _ := ms.store.List(ctx, memstore.QueryOpts{
			Subject:    input.Subject,
			Kind:       "invariant",
			Subsystem:  sub,
			OnlyActive: true,
		})
		for _, f := range inv {
			if !seen[f.ID] {
				seen[f.ID] = true
				invariants = append(invariants, f)
			}
		}

		fm, _ := ms.store.List(ctx, memstore.QueryOpts{
			Subject:    input.Subject,
			Kind:       "failure_mode",
			Subsystem:  sub,
			OnlyActive: true,
		})
		for _, f := range fm {
			if !seen[f.ID] {
				seen[f.ID] = true
				failureModes = append(failureModes, f)
			}
		}
	}

	// Load trigger facts for the subject; include those matching task keywords.
	var triggers []memstore.Fact
	triggerFacts, _ := ms.store.List(ctx, memstore.QueryOpts{
		Subject:    input.Subject,
		Kind:       "trigger",
		OnlyActive: true,
	})
	taskWords := strings.Fields(strings.ToLower(task))
	for _, f := range triggerFacts {
		if !seen[f.ID] && triggerMatches(f.Content, taskWords) {
			seen[f.ID] = true
			triggers = append(triggers, f)
		}
	}

	// Remaining search results not already included.
	var relevant []memstore.SearchResult
	for _, r := range searchResults {
		if !seen[r.Fact.ID] {
			seen[r.Fact.ID] = true
			relevant = append(relevant, r)
		}
	}

	total := len(invariants) + len(failureModes) + len(triggers) + len(relevant)
	if total == 0 {
		return textResult("No relevant context found for this task.", false), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[context for task: %q]\n", task)
	if input.Subject != "" {
		fmt.Fprintf(&b, "[subject: %s", input.Subject)
		if len(seenSubsystems) > 0 {
			subs := make([]string, 0, len(seenSubsystems))
			for s := range seenSubsystems {
				subs = append(subs, s)
			}
			sort.Strings(subs)
			fmt.Fprintf(&b, ", subsystems touched: %s", strings.Join(subs, ", "))
		}
		fmt.Fprintf(&b, "]\n")
	}
	fmt.Fprintln(&b)

	if len(invariants) > 0 {
		fmt.Fprintf(&b, "--- invariants (always apply when touching these subsystems) ---\n")
		for _, f := range invariants {
			writeContextFact(&b, f)
		}
		fmt.Fprintln(&b)
	}

	if len(failureModes) > 0 {
		fmt.Fprintf(&b, "--- failure modes ---\n")
		for _, f := range failureModes {
			writeContextFact(&b, f)
		}
		fmt.Fprintln(&b)
	}

	if len(triggers) > 0 {
		fmt.Fprintf(&b, "--- triggered context ---\n")
		for _, f := range triggers {
			writeContextFact(&b, f)
		}
		fmt.Fprintln(&b)
	}

	if len(relevant) > 0 {
		fmt.Fprintf(&b, "--- relevant context ---\n")
		for _, r := range relevant {
			fmt.Fprintf(&b, "[id=%d, score=%.3f] %s | %s", r.Fact.ID, r.Combined, r.Fact.Subject, r.Fact.Category)
			if r.Fact.Kind != "" {
				fmt.Fprintf(&b, " | kind=%s", r.Fact.Kind)
			}
			if r.Fact.Subsystem != "" {
				fmt.Fprintf(&b, " | subsystem=%s", r.Fact.Subsystem)
			}
			fmt.Fprintln(&b)
			fmt.Fprintf(&b, "  %s\n", r.Fact.Content)
			fmt.Fprintln(&b)
		}
	}

	// Inline drift warning: if a repo path is configured for this subject,
	// check whether any returned facts have stale source files.
	if repoPath, ok := ms.config.RepoPaths[input.Subject]; ok && repoPath != "" {
		var allFacts []memstore.Fact
		allFacts = append(allFacts, invariants...)
		allFacts = append(allFacts, failureModes...)
		allFacts = append(allFacts, triggers...)
		for _, r := range relevant {
			allFacts = append(allFacts, r.Fact)
		}
		stale := ms.checkFactsDrift(ctx, repoPath, allFacts, 0)
		if len(stale) > 0 {
			var warn strings.Builder
			fmt.Fprintf(&warn, "⚠ DRIFT WARNING: %d fact(s) may be stale (source files changed after last confirmation):\n", len(stale))
			for _, s := range stale {
				fmt.Fprintf(&warn, "  [id=%d] %s — last source change: %s (%s)\n",
					s.fact.ID, s.fact.Content[:min(60, len(s.fact.Content))],
					s.fileChanged.Format("2006-01-02"), s.changedFile)
			}
			fmt.Fprintln(&warn, "Run memory_confirm <id> after verifying accuracy, or supersede if stale.")
			fmt.Fprintln(&warn)
			b.WriteString("") // flush — prepend by rebuilding
			// Rebuild with warning at top.
			var full strings.Builder
			full.WriteString(warn.String())
			full.WriteString(b.String())
			return textResult(full.String(), false), nil, nil
		}
	}

	return textResult(b.String(), false), nil, nil
}

// driftResult holds a single stale fact with the file that changed and when.
type driftResult struct {
	fact        memstore.Fact
	changedFile string
	fileChanged time.Time
}

// checkFactsDrift checks a slice of facts for staleness against git history.
// Returns only the stale facts. sinceDays=0 means no recency limit.
func (ms *MemoryServer) checkFactsDrift(ctx context.Context, repoPath string, facts []memstore.Fact, sinceDays int) []driftResult {
	var sinceCutoff time.Time
	if sinceDays > 0 {
		sinceCutoff = time.Now().AddDate(0, 0, -sinceDays)
	}

	var stale []driftResult
	for _, f := range facts {
		var meta map[string]any
		if len(f.Metadata) == 0 {
			continue
		}
		_ = json.Unmarshal(f.Metadata, &meta)
		sfRaw, ok := meta["source_files"]
		if !ok {
			continue
		}
		sourceFiles := toStringSlice(sfRaw)
		if len(sourceFiles) == 0 {
			continue
		}

		factTime := f.CreatedAt
		if f.LastConfirmedAt != nil && f.LastConfirmedAt.After(factTime) {
			factTime = *f.LastConfirmedAt
		}

		for _, file := range sourceFiles {
			modified, ok := ms.gitRunner(ctx, repoPath, file)
			if !ok {
				continue
			}
			if modified.After(factTime) && (sinceCutoff.IsZero() || modified.After(sinceCutoff)) {
				stale = append(stale, driftResult{fact: f, changedFile: file, fileChanged: modified})
				break // one stale file is enough to flag the fact
			}
		}
	}
	return stale
}

func (ms *MemoryServer) HandleCheckDrift(ctx context.Context, _ *mcp.CallToolRequest, input CheckDriftInput) (*mcp.CallToolResult, any, error) {
	// Resolve repo path.
	repoPath := strings.TrimSpace(input.RepoPath)
	if repoPath == "" && input.Subject != "" {
		repoPath = ms.config.RepoPaths[input.Subject]
	}
	if repoPath == "" {
		return textResult("Error: repo_path is required (or configure Config.RepoPaths for the subject)", true), nil, nil
	}

	sinceDays := input.SinceDays
	if sinceDays == 0 {
		sinceDays = 7
	}

	// List active facts, optionally scoped to subject.
	facts, err := ms.store.List(ctx, memstore.QueryOpts{
		Subject:    input.Subject,
		OnlyActive: true,
	})
	if err != nil {
		return textResult(fmt.Sprintf("Error listing facts: %v", err), true), nil, nil
	}

	// Separate facts with source_files from those without.
	var withSource []memstore.Fact
	var noSource int
	for _, f := range facts {
		var meta map[string]any
		if len(f.Metadata) > 0 {
			_ = json.Unmarshal(f.Metadata, &meta)
		}
		if sf, ok := meta["source_files"]; ok && len(toStringSlice(sf)) > 0 {
			withSource = append(withSource, f)
		} else {
			noSource++
		}
	}

	if len(withSource) == 0 {
		msg := "No facts with source_files metadata found"
		if input.Subject != "" {
			msg += fmt.Sprintf(" for subject=%q", input.Subject)
		}
		msg += ". Add source_files metadata to facts that document code behavior."
		return textResult(msg, false), nil, nil
	}

	stale := ms.checkFactsDrift(ctx, repoPath, withSource, sinceDays)
	staleIDs := make(map[int64]bool)
	for _, s := range stale {
		staleIDs[s.fact.ID] = true
	}

	var current []memstore.Fact
	for _, f := range withSource {
		if !staleIDs[f.ID] {
			current = append(current, f)
		}
	}

	scope := "all facts"
	if input.Subject != "" {
		scope = fmt.Sprintf("subject=%q", input.Subject)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[drift report for %s, last %d days]\n\n", scope, sinceDays)

	if len(stale) > 0 {
		fmt.Fprintf(&b, "⚠ STALE (%d facts) — source files changed after last confirmation:\n\n", len(stale))
		for _, s := range stale {
			fmt.Fprintf(&b, "[id=%d] %s | %s", s.fact.ID, s.fact.Subject, s.fact.Category)
			if s.fact.Kind != "" {
				fmt.Fprintf(&b, " | kind=%s", s.fact.Kind)
			}
			if s.fact.Subsystem != "" {
				fmt.Fprintf(&b, " | subsystem=%s", s.fact.Subsystem)
			}
			lastConfirmed := s.fact.CreatedAt.Format("2006-01-02")
			if s.fact.LastConfirmedAt != nil {
				lastConfirmed = s.fact.LastConfirmedAt.Format("2006-01-02")
			}
			fmt.Fprintf(&b, "\n  last confirmed: %s\n  source file changed: %s (%s)\n  %s\n\n",
				lastConfirmed, s.fileChanged.Format("2006-01-02"), s.changedFile, s.fact.Content)
		}
		fmt.Fprintf(&b, "Run memory_confirm <id> after verifying accuracy, or memory_store with supersedes= if content changed.\n\n")
	} else {
		fmt.Fprintf(&b, "⚠ STALE (0 facts)\n\n")
	}

	fmt.Fprintf(&b, "✓ CURRENT (%d facts) — source files unchanged since last confirmation.\n", len(current))
	if noSource > 0 {
		fmt.Fprintf(&b, "  (+ %d facts without source_files metadata, not checked)\n", noSource)
	}

	return textResult(b.String(), false), nil, nil
}

// toStringSlice converts an interface{} that may be []interface{} or []string to []string.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// writeContextFact writes a single fact line for the get_context output.
func writeContextFact(b *strings.Builder, f memstore.Fact) {
	fmt.Fprintf(b, "[id=%d] %s | %s", f.ID, f.Subject, f.Category)
	if f.Kind != "" {
		fmt.Fprintf(b, " | kind=%s", f.Kind)
	}
	if f.Subsystem != "" {
		fmt.Fprintf(b, " | subsystem=%s", f.Subsystem)
	}
	fmt.Fprintln(b)
	fmt.Fprintf(b, "  %s\n", f.Content)
	if q := contextFactQuality(f); q != "" {
		fmt.Fprintf(b, "  [draft: %s — rewrite with memory_store + supersedes if you have better context]\n", q)
	}
	fmt.Fprintln(b)
}

// contextFactQuality returns the quality tag if the fact is a local-model draft, or "" otherwise.
func contextFactQuality(f memstore.Fact) string {
	if len(f.Metadata) == 0 {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal(f.Metadata, &meta); err != nil {
		return ""
	}
	if q, _ := meta["quality"].(string); strings.HasPrefix(q, "local") {
		return q
	}
	return ""
}

func (ms *MemoryServer) HandleListProject(ctx context.Context, _ *mcp.CallToolRequest, input ListProjectInput) (*mcp.CallToolResult, any, error) {
	cwd := strings.TrimSpace(input.CWD)
	if cwd == "" {
		return textResult("Error: cwd is required", true), nil, nil
	}

	// Query both surface=project and surface=package facts.
	var allFacts []memstore.Fact
	for _, surface := range []string{"project", "package"} {
		facts, err := ms.store.List(ctx, memstore.QueryOpts{
			OnlyActive: true,
			MetadataFilters: []memstore.MetadataFilter{
				{Key: "surface", Op: "=", Value: surface},
			},
		})
		if err != nil {
			return textResult(fmt.Sprintf("Error listing %s facts: %v", surface, err), true), nil, nil
		}
		allFacts = append(allFacts, facts...)
	}

	var matching []memstore.Fact
	seen := make(map[int64]bool)
	for _, f := range allFacts {
		if !seen[f.ID] && ms.factMatchesCWD(f, cwd) {
			seen[f.ID] = true
			matching = append(matching, f)
		}
	}

	if len(matching) == 0 {
		return textResult("No project-surface facts found for this directory.", false), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[project context for: %s]\n\n", cwd)
	for _, f := range matching {
		writeContextFact(&b, f)
	}
	return textResult(b.String(), false), nil, nil
}

// factMatchesCWD reports whether fact f applies to the given working directory.
// A fact applies if its project_path metadata is a prefix of cwd, or if its
// project_subject metadata resolves (via ms.config.ProjectPaths) to a prefix of cwd.
func (ms *MemoryServer) factMatchesCWD(f memstore.Fact, cwd string) bool {
	var meta map[string]any
	if len(f.Metadata) > 0 {
		_ = json.Unmarshal(f.Metadata, &meta)
	}
	cwdSlash := cwd
	if !strings.HasSuffix(cwdSlash, "/") {
		cwdSlash += "/"
	}

	matchesPath := func(projectPath string) bool {
		if projectPath == "" {
			return false
		}
		if cwd == projectPath {
			return true
		}
		pp := projectPath
		if !strings.HasSuffix(pp, "/") {
			pp += "/"
		}
		return strings.HasPrefix(cwdSlash, pp)
	}

	if pp, _ := meta["project_path"].(string); matchesPath(pp) {
		return true
	}
	if pp, _ := meta["package_path"].(string); matchesPath(pp) {
		return true
	}
	if ps, _ := meta["project_subject"].(string); ps != "" {
		if path, ok := ms.config.ProjectPaths[ps]; ok {
			return matchesPath(path)
		}
	}
	return false
}

func (ms *MemoryServer) HandleListFile(ctx context.Context, _ *mcp.CallToolRequest, input ListFileInput) (*mcp.CallToolResult, any, error) {
	filePath := strings.TrimSpace(input.FilePath)
	if filePath == "" {
		return textResult("Error: file_path is required", true), nil, nil
	}

	// Query surface=file and surface=symbol for this exact file path.
	var fileFacts, symbolFacts []memstore.Fact
	for _, surface := range []string{"file", "symbol"} {
		facts, err := ms.store.List(ctx, memstore.QueryOpts{
			OnlyActive: true,
			MetadataFilters: []memstore.MetadataFilter{
				{Key: "surface", Op: "=", Value: surface},
				{Key: "file_path", Op: "=", Value: filePath},
			},
		})
		if err != nil {
			return textResult(fmt.Sprintf("Error listing %s facts: %v", surface, err), true), nil, nil
		}
		if surface == "file" {
			fileFacts = facts
		} else {
			symbolFacts = facts
		}
	}

	// Optionally narrow symbol results by name.
	if input.SymbolName != "" {
		lower := strings.ToLower(input.SymbolName)
		var filtered []memstore.Fact
		for _, f := range symbolFacts {
			var meta map[string]any
			if len(f.Metadata) > 0 {
				_ = json.Unmarshal(f.Metadata, &meta)
			}
			if sn, _ := meta["symbol_name"].(string); strings.ToLower(sn) == lower {
				filtered = append(filtered, f)
			}
		}
		symbolFacts = filtered
	}

	if len(fileFacts)+len(symbolFacts) == 0 {
		return textResult("No file-surface facts found for this file.", false), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[file context for: %s]\n\n", filePath)
	if len(fileFacts) > 0 {
		fmt.Fprintf(&b, "--- file ---\n")
		for _, f := range fileFacts {
			writeContextFact(&b, f)
		}
	}
	if len(symbolFacts) > 0 {
		if input.SymbolName != "" {
			fmt.Fprintf(&b, "--- symbol: %s ---\n", input.SymbolName)
		} else {
			fmt.Fprintf(&b, "--- symbols ---\n")
		}
		for _, f := range symbolFacts {
			writeContextFact(&b, f)
		}
	}
	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleCurateContext(ctx context.Context, _ *mcp.CallToolRequest, input CurateContextInput) (*mcp.CallToolResult, any, error) {
	if len(input.FactIDs) == 0 {
		return textResult("Error: fact_ids is required", true), nil, nil
	}
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return textResult("Error: task is required", true), nil, nil
	}
	maxOutput := input.MaxOutput
	if maxOutput <= 0 {
		maxOutput = 5
	}

	// Fetch the candidate facts by ID.
	candidates, err := ms.store.List(ctx, memstore.QueryOpts{
		IDs:        input.FactIDs,
		OnlyActive: true,
	})
	if err != nil {
		return textResult(fmt.Sprintf("Error fetching candidates: %v", err), true), nil, nil
	}
	if len(candidates) == 0 {
		return textResult("No active facts found for the provided fact_ids.", false), nil, nil
	}

	selected, rationale, err := ms.curator.Curate(ctx, task, candidates, maxOutput)
	if err != nil {
		// Fallback: return top maxOutput candidates unfiltered.
		fallback := candidates
		if maxOutput < len(fallback) {
			fallback = fallback[:maxOutput]
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[curation failed (%v); returning top %d unfiltered]\n\n", err, len(fallback))
		for _, f := range fallback {
			writeContextFact(&b, f)
		}
		return textResult(b.String(), false), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[curated context: %d of %d candidates selected]\n", len(selected), len(candidates))
	fmt.Fprintf(&b, "rationale: %s\n\n", rationale)
	for _, f := range selected {
		writeContextFact(&b, f)
	}
	return textResult(b.String(), false), nil, nil
}

// triggerMatches returns true if any word from taskWords appears in the trigger content.
// HandleSuggestAgent recommends specialist agents for a task based on stored
// agent-routing facts. Scores agents by domain keyword overlap with the task.
func (ms *MemoryServer) HandleSuggestAgent(ctx context.Context, _ *mcp.CallToolRequest, input SuggestAgentInput) (*mcp.CallToolResult, any, error) {
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return textResult("Error: task is required", true), nil, nil
	}

	// Collect agent-routing facts. Try subject-scoped first, then fall back to global.
	var routingFacts []memstore.Fact
	if input.Subject != "" {
		facts, err := ms.store.List(ctx, memstore.QueryOpts{
			Subject:    input.Subject,
			Subsystem:  "agent-routing",
			OnlyActive: true,
		})
		if err == nil {
			routingFacts = append(routingFacts, facts...)
		}
	}
	// Always include global (unscoped subject) routing facts.
	globalFacts, err := ms.store.List(ctx, memstore.QueryOpts{
		Subsystem:  "agent-routing",
		OnlyActive: true,
	})
	if err == nil {
		seen := make(map[int64]bool, len(routingFacts))
		for _, f := range routingFacts {
			seen[f.ID] = true
		}
		for _, f := range globalFacts {
			if !seen[f.ID] {
				routingFacts = append(routingFacts, f)
			}
		}
	}

	if len(routingFacts) == 0 {
		return textResult("No agent-routing facts found. Seed them with memory_store:\n"+
			"  subject: \"global\" (or project name), subsystem: \"agent-routing\", kind: \"convention\"\n"+
			"  metadata: {\"agent_name\": \"security-reviewer\", \"domains\": [\"security\", \"auth\"]}\n"+
			"  content: description of when to use this agent", false), nil, nil
	}

	taskLower := strings.ToLower(task)
	taskWords := strings.Fields(taskLower)

	type agentScore struct {
		name      string
		score     int
		rationale string
		content   string
	}

	// Score each agent by domain keyword overlap + content keyword match.
	var scores []agentScore
	for _, f := range routingFacts {
		var meta map[string]any
		if len(f.Metadata) > 0 {
			json.Unmarshal(f.Metadata, &meta)
		}
		agentName, _ := meta["agent_name"].(string)
		if agentName == "" {
			continue
		}

		score := 0
		var matched []string

		// Score domain keyword matches.
		if rawDomains, ok := meta["domains"].([]any); ok {
			for _, d := range rawDomains {
				domain, _ := d.(string)
				if domain == "" {
					continue
				}
				domainLower := strings.ToLower(domain)
				if strings.Contains(taskLower, domainLower) {
					score += 3
					matched = append(matched, domain)
				}
			}
		}

		// Score content keyword matches (weaker signal).
		contentLower := strings.ToLower(f.Content)
		for _, w := range taskWords {
			if len(w) >= 3 && strings.Contains(contentLower, w) {
				score++
			}
		}

		if score > 0 {
			rationale := fmt.Sprintf("matched domains: %s", strings.Join(matched, ", "))
			if len(matched) == 0 {
				rationale = "matched task keywords in agent description"
			}
			scores = append(scores, agentScore{
				name:      agentName,
				score:     score,
				rationale: rationale,
				content:   f.Content,
			})
		}
	}

	if len(scores) == 0 {
		return textResult("No agents matched the task description. Try broader domain keywords or check stored agent-routing facts with memory_list(subsystem=\"agent-routing\").", false), nil, nil
	}

	// Sort by score descending.
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	// Cap at 5 suggestions.
	if len(scores) > 5 {
		scores = scores[:5]
	}

	maxScore := scores[0].score
	var b strings.Builder
	fmt.Fprintf(&b, "[agent suggestions for: %q]\n\n", task)
	for _, s := range scores {
		confidence := float64(s.score) / float64(maxScore)
		level := "low"
		if confidence >= 0.8 {
			level = "high"
		} else if confidence >= 0.5 {
			level = "medium"
		}
		fmt.Fprintf(&b, "- %s (confidence: %s, score: %d)\n  %s\n  %s\n\n", s.name, level, s.score, s.rationale, s.content)
	}
	return textResult(b.String(), false), nil, nil
}

func triggerMatches(content string, taskWords []string) bool {
	lower := strings.ToLower(content)
	for _, w := range taskWords {
		if len(w) >= 3 && strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// HandleLearn ingests a Go codebase into structured facts.
func (ms *MemoryServer) HandleLearn(ctx context.Context, _ *mcp.CallToolRequest, input LearnInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(input.RepoPath) == "" {
		return textResult("Error: repo_path is required", true), nil, nil
	}
	if strings.TrimSpace(input.Subject) == "" {
		return textResult("Error: subject is required", true), nil, nil
	}
	if ms.generator == nil {
		return textResult("Error: no generator configured; set --gen-model on the MCP server", true), nil, nil
	}

	learner := memstore.NewCodebaseLearner(ms.store, ms.embedder, ms.generator)
	result, err := learner.Learn(ctx, memstore.LearnOpts{
		RepoPath:         input.RepoPath,
		Subject:          input.Subject,
		ModulePath:       input.ModulePath,
		MaxFileSizeBytes: input.MaxFileSize,
		ExcludeDirs:      input.ExcludeDirs,
		Force:            input.Force,
		ExcludeTests:     input.ExcludeTests,
	})
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Learned %s: repo=%d packages=%d files=%d symbols=%d links=%d",
		input.Subject, result.RepoFactID, result.Packages, result.Files, result.Symbols, result.Links)
	if result.Skipped > 0 {
		fmt.Fprintf(&b, " skipped=%d", result.Skipped)
	}
	if result.Superseded > 0 {
		fmt.Fprintf(&b, " superseded=%d", result.Superseded)
	}
	fmt.Fprintf(&b, " llm_calls=%d", result.LLMCalls)
	if len(result.Errors) > 0 {
		fmt.Fprintf(&b, "\n\n%d errors:", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Fprintf(&b, "\n  - %v", e)
		}
	}
	return textResult(b.String(), false), nil, nil
}

func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: isError,
	}
}
