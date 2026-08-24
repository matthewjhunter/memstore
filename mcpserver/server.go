// Package mcpserver provides an MCP (Model Context Protocol) server that
// exposes a memstore-backed persistent memory system as MCP tools. It is
// designed to give Claude (or any MCP client) durable, searchable memory
// across sessions via hybrid FTS5 + vector search.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/internal/fence"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config holds optional configuration for MemoryServer.
type Config struct {
	// Curator selects the most relevant subset of candidates for a given task.
	// If nil, memory_curate_context uses NopCurator (returns candidates unfiltered).
	Curator memstore.Curator

	// Generator produces text completions for LLM-based operations.
	Generator memstore.Generator

	// SessionStore enables the memory_rate_context tool for injection feedback.
	// Only RecordFeedback is required; httpclient.Client satisfies this.
	// If nil, memory_rate_context is not registered.
	SessionStore memstore.FeedbackStore

	// ReadOnly registers only the tools that do not mutate the store, for
	// retrieval-only consumers such as a chatbot doing RAG over the corpus.
	// The write tools are not advertised at all rather than being advertised
	// and failing when called: a model that can see memory_store will keep
	// trying to use it, and a tool list that does not match what the session
	// can do is its own source of confusion.
	//
	// This is an ergonomic boundary, not an authorization one — it lives in
	// the client process and anything holding the token could talk to the
	// daemon directly. Pair it with a token issued `--scopes read` so the
	// daemon enforces the same limit server-side.
	ReadOnly bool

	// RerankMode and RerankThreshold seed the server's default rerank policy for
	// memory_search and memory_get_context. RerankMode off (the default) leaves
	// rerank disabled until set via the memory_rerank_settings tool or per call. Both
	// are mutable at runtime via memory_rerank_settings.
	RerankMode      memstore.RerankMode
	RerankThreshold float64
	// RerankCandidates and RerankRecallCandidates seed the candidate-pool caps
	// for memory_search and memory_get_context respectively (0 = the store's
	// built-in default). They are starting points; the model can override them
	// per session via memory_rerank_settings.
	RerankCandidates       int
	RerankRecallCandidates int
	// RerankDocBytes and RerankRecallDocBytes seed the per-document truncation
	// budgets for memory_search and memory_get_context (0 = the store's default).
	// Runtime-tunable via memory_rerank_settings.
	RerankDocBytes       int
	RerankRecallDocBytes int
}

// MemoryServer bridges MCP tool calls to a memstore.Store.
type MemoryServer struct {
	store        memstore.Store
	embedder     embedding.Embedder
	embedCeiling int
	config       Config
	curator      memstore.Curator
	generator    memstore.Generator
	sessionStore memstore.FeedbackStore
	readOnly     bool

	// mu guards the runtime-mutable retrieval tunables (memory_rerank_settings). All
	// are per-session overrides the model can adjust from observed performance,
	// so it isn't pinned to the daemon's env defaults. A zero value means "use
	// the built-in/engine default" for that knob.
	mu               sync.RWMutex
	rerankMode       memstore.RerankMode
	rerankThreshold  float64
	rerankWeight     float64       // balanced-fusion weight; 0 = engine default
	searchCandidates int           // memory_search pool; 0 = store default
	recallCandidates int           // memory_get_context pool; 0 = store default
	searchDocBytes   int           // memory_search per-doc truncation; 0 = store default
	recallDocBytes   int           // memory_get_context per-doc truncation; 0 = store default
	rerankTimeout    time.Duration // deadline on search/get_context; 0 = none
}

// rerankTunables is a lock-free snapshot of the runtime knobs.
type rerankTunables struct {
	mode             memstore.RerankMode
	threshold        float64
	weight           float64
	searchCandidates int
	recallCandidates int
	searchDocBytes   int
	recallDocBytes   int
	timeout          time.Duration
}

// NewMemoryServer creates a server backed by the given store and embedder.
// The embedder is used to compute embeddings at insert time so search always
// works. Both parameters are required.
func NewMemoryServer(store memstore.Store, embedder embedding.Embedder) *MemoryServer {
	return NewMemoryServerWithConfig(store, embedder, Config{})
}

// NewMemoryServerWithConfig is like NewMemoryServer but accepts additional
// configuration (curator, generator, session store, rerank defaults).
func NewMemoryServerWithConfig(store memstore.Store, embedder embedding.Embedder, cfg Config) *MemoryServer {
	curator := cfg.Curator
	if curator == nil {
		curator = memstore.NopCurator{}
	}
	return &MemoryServer{
		store: store, embedder: embedder, config: cfg,
		curator: curator, generator: cfg.Generator, sessionStore: cfg.SessionStore,
		readOnly:   cfg.ReadOnly,
		rerankMode: cfg.RerankMode, rerankThreshold: cfg.RerankThreshold,
		searchCandidates: cfg.RerankCandidates, recallCandidates: cfg.RerankRecallCandidates,
		searchDocBytes: cfg.RerankDocBytes, recallDocBytes: cfg.RerankRecallDocBytes,
	}
}

// rerankPolicy returns the server's current default rerank mode and threshold.
func (ms *MemoryServer) rerankPolicy() (memstore.RerankMode, float64) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.rerankMode, ms.rerankThreshold
}

// tunables returns a consistent snapshot of all runtime retrieval knobs.
func (ms *MemoryServer) tunables() rerankTunables {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return rerankTunables{
		mode:             ms.rerankMode,
		threshold:        ms.rerankThreshold,
		weight:           ms.rerankWeight,
		searchCandidates: ms.searchCandidates,
		recallCandidates: ms.recallCandidates,
		searchDocBytes:   ms.searchDocBytes,
		recallDocBytes:   ms.recallDocBytes,
		timeout:          ms.rerankTimeout,
	}
}

// setRerankPolicy updates the runtime rerank mode and threshold (used by tests
// and the simple path of memory_rerank_settings).
func (ms *MemoryServer) setRerankPolicy(mode memstore.RerankMode, threshold float64) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.rerankMode = mode
	ms.rerankThreshold = threshold
}

// resolveRerank applies per-call overrides over the server default: a non-empty
// modeStr replaces the mode (parsed leniently — "off" disables); a non-nil
// threshold replaces the threshold. Returns the effective mode and threshold.
func (ms *MemoryServer) resolveRerank(modeStr string, threshold *float64) (memstore.RerankMode, float64) {
	mode, thr := ms.rerankPolicy()
	if strings.TrimSpace(modeStr) != "" {
		if m, err := memstore.ParseRerankMode(modeStr); err == nil {
			mode = m
		}
	}
	if threshold != nil {
		thr = *threshold
	}
	return mode, thr
}

// Metadata is domain-specific key-value data attached to a fact, link, or task,
// and the single type both tool inputs and tool results declare it as.
//
// It is deliberately open. Callers attach whatever their domain needs -- provenance
// (cwd, session_id, source), task state (status, scope, project), or anything else --
// and the store round-trips it verbatim. The schema this infers must therefore permit
// unknown keys, which rules out a struct: jsonschema-go gives structs
// additionalProperties:false, so a fact carrying any key the struct didn't declare
// would fail output validation. It also rules out a []byte-backed type such as
// json.RawMessage, which infers an array schema for what marshals as an object.
type Metadata map[string]any

// String returns the value at key when it is a string. JSON decoding gives no
// guarantee about a value's type, so callers reading a known key get the check for
// free rather than re-asserting at each use.
func (m Metadata) String(key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

// --- Output types for structured tool results ---

// SearchResult is the structured output for memory_search.
type SearchResult struct {
	Query   string       `json:"query"`
	Results []FactResult `json:"results"`
	// FloorDropped and FloorTopDropped report what the relevance floor removed,
	// omitted when it removed nothing. A result set the floor trimmed otherwise
	// looks identical to one that was always that short, which is the whole
	// reason the floor needs to account for itself (#170). FloorTopDropped is
	// the closest miss: several facts dropped just under the floor argue it is
	// set too high, several dropped far below argue it is working.
	FloorDropped    int     `json:"floor_dropped,omitempty"`
	FloorTopDropped float64 `json:"floor_top_dropped,omitempty"`
}

// FactResult represents a single search result with typed fields.
type FactResult struct {
	ID             int64    `json:"id"`
	Subject        string   `json:"subject"`
	Category       string   `json:"category"`
	Kind           string   `json:"kind,omitempty"`
	Subsystem      string   `json:"subsystem,omitempty"`
	Content        string   `json:"content"`
	Score          float64  `json:"score"`
	RerankScore    float64  `json:"rerank_score,omitempty"`
	UseCount       int      `json:"use_count"`
	ConfirmedCount int      `json:"confirmed_count"`
	SupersededBy   *int64   `json:"superseded_by,omitempty"`
	Metadata       Metadata `json:"metadata,omitempty"`
}

// ListResult is the structured output for memory_list.
type ListResult struct {
	Facts []FactResult `json:"facts"`
}

// GetContextResult is the structured output for memory_get_context.
type GetContextResult struct {
	Task         string       `json:"task"`
	Subject      string       `json:"subject,omitempty"`
	Invariants   []FactResult `json:"invariants"`
	FailureModes []FactResult `json:"failure_modes"`
	Triggers     []FactResult `json:"triggers"`
	Relevant     []FactResult `json:"relevant"`
	Subsystems   []string     `json:"subsystems,omitempty"`
}

// SuggestAgentResult is the structured output for memory_suggest_agent.
type SuggestAgentResult struct {
	Task        string       `json:"task"`
	Suggestions []AgentScore `json:"suggestions"`
}

// AgentScore represents a single agent suggestion.
type AgentScore struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"`
	Score      int    `json:"score"`
	Rationale  string `json:"rationale"`
	Content    string `json:"content"`
}

// CurateContextResult is the structured output for memory_curate_context.
type CurateContextResult struct {
	Task       string       `json:"task"`
	Selected   int          `json:"selected"`
	Candidates int          `json:"candidates"`
	Rationale  string       `json:"rationale"`
	Facts      []FactResult `json:"facts"`
}

// ListSubsystemsResult is the structured output for memory_list_subsystems.
type ListSubsystemsResult struct {
	Subsystems []string `json:"subsystems"`
}

// GetLinksResult is the structured output for memory_get_links.
type GetLinksResult struct {
	FactID int64       `json:"fact_id"`
	Links  []LinkEntry `json:"links"`
}

// LinkEntry represents a single link with neighbor info.
type LinkEntry struct {
	ID              int64    `json:"link_id"`
	SourceID        int64    `json:"source_id"`
	TargetID        int64    `json:"target_id"`
	LinkType        string   `json:"link_type"`
	Bidirectional   bool     `json:"bidirectional,omitempty"`
	Label           string   `json:"label,omitempty"`
	Metadata        Metadata `json:"metadata,omitempty"`
	NeighborID      int64    `json:"neighbor_id"`
	NeighborSubject string   `json:"neighbor_subject"`
	NeighborContent string   `json:"neighbor_content"`
}

// StoreResult is the structured output for memory_store.
type StoreResult struct {
	Status     string `json:"status"`
	ID         int64  `json:"id,omitempty"`
	Superseded *int64 `json:"superseded_by,omitempty"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// StoreBatchResult is the structured output for memory_store_batch.
type StoreBatchResult struct {
	Stored  int           `json:"stored"`
	Total   int           `json:"total"`
	Results []BatchResult `json:"results"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// BatchResult represents a single batch operation result.
type BatchResult struct {
	Index      int    `json:"index"`
	Status     string `json:"status"`
	ID         int64  `json:"id,omitempty"`
	Superseded *int64 `json:"superseded_by,omitempty"`
	Error      string `json:"error,omitempty"`
}

// DeleteResult is the structured output for memory_delete.
type DeleteResult struct {
	Status string `json:"status"`
	ID     int64  `json:"id"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// SupersedeResult is the structured output for memory_supersede.
type SupersedeResult struct {
	Status     string `json:"status"`
	OldID      int64  `json:"old_id"`
	NewID      int64  `json:"new_id"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// HistoryResult is the structured output for memory_history.
type HistoryResult struct {
	Entries []HistoryEntry `json:"entries"`
}

// HistoryEntry represents a single history entry.
type HistoryEntry struct {
	ID             int64    `json:"id"`
	Position       int      `json:"position"`
	ChainLength    int      `json:"chain_length"`
	Subject        string   `json:"subject"`
	Category       string   `json:"category"`
	Status         string   `json:"status"`
	CreatedAt      string   `json:"created_at"`
	Content        string   `json:"content"`
	Metadata       Metadata `json:"metadata,omitempty"`
	UseCount       int      `json:"use_count"`
	ConfirmedCount int      `json:"confirmed_count"`
}

// ConfirmResult is the structured output for memory_confirm.
type ConfirmResult struct {
	Status         string `json:"status"`
	ID             int64  `json:"id"`
	ConfirmedCount int    `json:"confirmed_count"`
	Content        string `json:"content"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// StatusResult is the structured output for memory_status.
type StatusResult struct {
	ActiveCount int64          `json:"active_count"`
	Categories  map[string]int `json:"categories,omitempty"`
	Kinds       map[string]int `json:"kinds,omitempty"`
	// Subjects holds at most statusMaxSubjects entries, the highest-count
	// ones, matching what the text rendering shows. A live corpus reached
	// 2,560 subjects, which serialized past the MCP result cap and spilled
	// the whole call to a file (#150). SubjectCount reports the true total
	// so a truncated map is never mistaken for the whole picture.
	Subjects          map[string]int `json:"subjects,omitempty"`
	SubjectCount      int            `json:"subject_count,omitempty"`
	SubjectsTruncated bool           `json:"subjects_truncated,omitempty"`
}

// UpdateResult is the structured output for memory_update.
type UpdateResult struct {
	Status string `json:"status"`
	ID     int64  `json:"id"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// TaskCreateResult is the structured output for memory_task_create.
type TaskCreateResult struct {
	Status   string `json:"status"`
	ID       int64  `json:"id"`
	Scope    string `json:"scope"`
	Priority string `json:"priority"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// TaskUpdateResult is the structured output for memory_task_update.
type TaskUpdateResult struct {
	Status    string `json:"status"`
	ID        int64  `json:"id"`
	OldStatus string `json:"old_status,omitempty"`
	NewStatus string `json:"new_status"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// TaskListResult is the structured output for memory_task_list.
type TaskListResult struct {
	Tasks []TaskResult `json:"tasks"`
}

// TaskResult represents a single task fact.
type TaskResult struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"`
	Scope    string `json:"scope"`
	Priority string `json:"priority"`
	Content  string `json:"content"`
	Due      string `json:"due,omitempty"`
}

// LinkResult is the structured output for memory_link.
type LinkResult struct {
	Status        string `json:"status"`
	LinkID        int64  `json:"link_id"`
	SourceID      int64  `json:"source_id"`
	TargetID      int64  `json:"target_id"`
	LinkType      string `json:"link_type"`
	Bidirectional bool   `json:"bidirectional"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// UnlinkResult is the structured output for memory_unlink.
type UnlinkResult struct {
	Status string `json:"status"`
	LinkID int64  `json:"link_id"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// UpdateLinkResult is the structured output for memory_update_link.
type UpdateLinkResult struct {
	Status string `json:"status"`
	LinkID int64  `json:"link_id"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// RateContextResult is the structured output for memory_rate_context.
type RateContextResult struct {
	Status string `json:"status"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// RerankSettingsResult is the structured output for memory_rerank_settings.
type RerankSettingsResult struct {
	Mode             string  `json:"mode"`
	Threshold        float64 `json:"threshold"`
	Weight           float64 `json:"weight,omitempty"`
	SearchCandidates int     `json:"search_candidates"`
	RecallCandidates int     `json:"recall_candidates"`
	SearchDocBytes   int     `json:"search_doc_bytes"`
	RecallDocBytes   int     `json:"recall_doc_bytes"`
	Timeout          string  `json:"timeout,omitempty"`

	// Error names a caller-side mistake; empty on every other path.
	Error string `json:"error,omitempty"`
}

// --- Input types (MCP SDK infers JSON schemas from struct tags) ---

// StoreInput is the input schema for the memory_store tool.
type StoreInput struct {
	Content    string   `json:"content" jsonschema:"the factual claim or memory to store"`
	Subject    string   `json:"subject" jsonschema:"the entity this fact is about (e.g. a person or project)"`
	Category   string   `json:"category,omitempty" jsonschema:"fact category: preference, identity, project, capability, relationship, world, or note (default: note)"`
	Kind       string   `json:"kind,omitempty" jsonschema:"structural type: convention | failure_mode | invariant | pattern | decision | trigger (empty = unclassified)"`
	Subsystem  string   `json:"subsystem,omitempty" jsonschema:"optional project subsystem this fact belongs to (e.g. feeds, auth, storage)"`
	Metadata   Metadata `json:"metadata,omitempty" jsonschema:"optional key-value metadata to attach"`
	Supersedes *int64   `json:"supersedes,omitempty" jsonschema:"ID of an existing fact that this new fact replaces (preserves history unlike delete)"`
}

// StoreBatchInput is the input schema for the memory_store_batch tool.
type StoreBatchInput struct {
	Facts []StoreInput `json:"facts" jsonschema:"array of facts to store (max 20)"`
}

// SearchInput is the input schema for the memory_search tool.
type SearchInput struct {
	Query             string   `json:"query" jsonschema:"natural language search query"`
	Subject           string   `json:"subject,omitempty" jsonschema:"filter results to a specific subject entity"`
	Category          string   `json:"category,omitempty" jsonschema:"filter results to a specific category"`
	Kind              string   `json:"kind,omitempty" jsonschema:"filter by kind: convention, failure_mode, invariant, pattern, decision, trigger (empty = all)"`
	Subsystem         string   `json:"subsystem,omitempty" jsonschema:"filter by subsystem (e.g. feeds, auth)"`
	Limit             int      `json:"limit,omitempty" jsonschema:"maximum number of results (default 10)"`
	IncludeSuperseded bool     `json:"include_superseded,omitempty" jsonschema:"if true, include superseded facts in results (tagged with [SUPERSEDED])"`
	Metadata          Metadata `json:"metadata,omitempty" jsonschema:"filter by metadata fields (equality match, e.g. {\"source\": \"conversation\"})"`
	RerankMode        string   `json:"rerank_mode,omitempty" jsonschema:"override the server's rerank mode for this call: off|balanced|dominant|gate (empty = server default)"`
	Threshold         *float64 `json:"threshold,omitempty" jsonschema:"override the relevance threshold [0,1] for this call; facts scoring below it are dropped (omit = server default)"`
}

// ListInput is the input schema for the memory_list tool.
type ListInput struct {
	Subject   string   `json:"subject,omitempty" jsonschema:"filter by subject entity"`
	Category  string   `json:"category,omitempty" jsonschema:"filter by category"`
	Kind      string   `json:"kind,omitempty" jsonschema:"filter by kind: convention, failure_mode, invariant, pattern, decision, trigger (empty = all)"`
	Subsystem string   `json:"subsystem,omitempty" jsonschema:"filter by subsystem (e.g. feeds, auth)"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum number of results (default 20)"`
	Metadata  Metadata `json:"metadata,omitempty" jsonschema:"filter by metadata fields (equality match, e.g. {\"source\": \"conversation\"})"`
}

// ListSubsystemsInput is the input schema for the memory_list_subsystems tool.
type ListSubsystemsInput struct {
	Subject string `json:"subject,omitempty" jsonschema:"filter to a specific subject entity (empty = all subjects)"`
}

// CurateContextInput is the input schema for the memory_curate_context tool.
type CurateContextInput struct {
	Task      string  `json:"task" jsonschema:"description of the task being worked on; used to rank candidate facts by relevance"`
	FactIDs   []int64 `json:"fact_ids" jsonschema:"IDs of candidate facts to evaluate; fetch via memory_get_context or memory_search first"`
	MaxOutput int     `json:"max_output,omitempty" jsonschema:"max facts to return (default 5)"`
}

// GetContextInput is the input schema for the memory_get_context tool.
type GetContextInput struct {
	Task       string   `json:"task" jsonschema:"description of the task or feature being worked on"`
	Subject    string   `json:"subject,omitempty" jsonschema:"optional subject to scope context loading (e.g. a project name)"`
	Limit      int      `json:"limit,omitempty" jsonschema:"max total facts in the relevant context section (default 20)"`
	RerankMode string   `json:"rerank_mode,omitempty" jsonschema:"override the server's rerank mode for this call: off|balanced|dominant|gate (empty = server default)"`
	Threshold  *float64 `json:"threshold,omitempty" jsonschema:"override the relevance threshold [0,1] for this call (omit = server default)"`
}

// RerankSettingsInput is the input schema for the memory_rerank_settings tool.
type RerankSettingsInput struct {
	Mode             string   `json:"mode,omitempty" jsonschema:"rerank fusion mode: off|balanced|dominant|gate (omit to leave unchanged)"`
	Threshold        *float64 `json:"threshold,omitempty" jsonschema:"relevance threshold 0-1; facts scoring below it are dropped (omit to leave unchanged)"`
	Weight           *float64 `json:"weight,omitempty" jsonschema:"balanced-fusion weight 0-1: rerank's share vs the first-stage score (0 resets to the engine default; omit to leave unchanged)"`
	SearchCandidates *int     `json:"search_candidates,omitempty" jsonschema:"how many first-stage candidates memory_search reranks per pass; more = better recall, slower (0 resets to default; omit to leave unchanged)"`
	RecallCandidates *int     `json:"recall_candidates,omitempty" jsonschema:"how many candidates memory_get_context reranks per pass (0 resets to default; omit to leave unchanged)"`
	SearchDocBytes   *int     `json:"search_doc_bytes,omitempty" jsonschema:"truncate each memory_search rerank document to this many bytes; rerank cost is superlinear in length, so this is the strongest latency lever (0 resets to default; omit to leave unchanged)"`
	RecallDocBytes   *int     `json:"recall_doc_bytes,omitempty" jsonschema:"same, for memory_get_context; keep it small for a tight injection budget (0 resets to default; omit to leave unchanged)"`
	TimeoutSeconds   *float64 `json:"timeout_seconds,omitempty" jsonschema:"max seconds to wait for rerank before degrading to first-stage order (0 disables the deadline; omit to leave unchanged)"`
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
	ID       int64    `json:"id" jsonschema:"the fact ID to update"`
	Metadata Metadata `json:"metadata" jsonschema:"metadata keys to set (non-nil) or delete (nil)"`
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
	SourceID      int64    `json:"source_id" jsonschema:"ID of the source fact"`
	TargetID      int64    `json:"target_id" jsonschema:"ID of the target fact"`
	LinkType      string   `json:"link_type,omitempty" jsonschema:"edge type discriminator: passage, event, entrance, reference, etc. (default: reference)"`
	Bidirectional bool     `json:"bidirectional,omitempty" jsonschema:"if true, edge is traversable in both directions"`
	Label         string   `json:"label,omitempty" jsonschema:"human-readable description of this connection"`
	Metadata      Metadata `json:"metadata,omitempty" jsonschema:"domain-specific properties for this edge"`
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
	LinkID   int64    `json:"link_id" jsonschema:"ID of the link to update"`
	Label    string   `json:"label,omitempty" jsonschema:"new label (empty leaves existing label unchanged)"`
	Metadata Metadata `json:"metadata,omitempty" jsonschema:"metadata keys to set (non-nil) or delete (nil)"`
}

// SuggestAgentInput is the input schema for the memory_suggest_agent tool.
type SuggestAgentInput struct {
	Task    string `json:"task" jsonschema:"description of the work to be done"`
	Subject string `json:"subject,omitempty" jsonschema:"scope to a specific project's routing rules (falls back to global rules if no project-specific match)"`
}

// RateContextInput is the input schema for the memory_rate_context tool.
type RateContextInput struct {
	RefID     string `json:"ref_id" jsonschema:"fact ID or session turn UUID to rate"`
	RefType   string `json:"ref_type" jsonschema:"type of the ref: 'fact' or 'turn'"`
	SessionID string `json:"session_id" jsonschema:"current session ID"`
	Score     int    `json:"score" jsonschema:"rating: +1 (useful) or -1 (not useful)"`
	Reason    string `json:"reason,omitempty" jsonschema:"brief explanation of the rating"`
}

// --- Tool registration ---

// addWriteTool registers a tool that mutates the store. Under Config.ReadOnly
// the tool is not registered at all, so it never appears in tools/list and a
// client cannot call it.
//
// Every store-mutating registration must go through this rather than
// mcp.AddTool directly. TestEveryToolIsClassified fails when a new tool is
// added without being sorted into the read or write list, which is what stops
// a write tool from silently defaulting into read-only mode.
func addWriteTool[In, Out any](ms *MemoryServer, s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if ms.readOnly {
		return
	}
	mcp.AddTool(s, t, h)
}

// Register adds all memory tools to the given MCP server.
func (ms *MemoryServer) Register(s *mcp.Server) {
	addWriteTool(ms, s, &mcp.Tool{
		Name: "memory_store",
		Description: `Store a fact or memory. Persists across sessions with automatic embedding for semantic search.

**Scope — what belongs here:** facts that travel with the user across sessions AND across repos. Durable facts about who they are, their preferences, their interests (authors, hobbies, ongoing reading), people in their life, their hardware/homelab, and the broader cross-repo project landscape. Ask: "would a fresh session in any working directory benefit from knowing this?" If yes, store it.

**What does NOT belong here:** architecture, invariants, or conventions of the current repo — those live in the code and CLAUDE.md, which are authoritative there. Per-task scratch state (use plans/tasks). Anything already in a project's CLAUDE.md. The current repo's details are *secondary* in memstore; the person-and-world layer is primary.

Store aggressively within scope — it is better to store something and supersede it later than to lose it.

Conventions:
- subject: lowercase, singular entity name (e.g. the user's name, "jane-austen" for an external author, "memstore" for a subsystem, "home-server" for a machine). This is the primary lookup key — be consistent.
- category: pick by what kind of fact this is —
  - identity: immutable traits of the user (background, role, credentials)
  - preference: how the user likes things done
  - relationship: people the user knows or interacts with
  - capability: skills, tools, or what their systems can do
  - project: project decisions, repos, work-in-progress
  - world: facts about external entities — authors they read, books, hardware they own, places, organizations. Use this for durable interests and reference data about the world outside themselves.
  - note: catch-all when nothing else fits
- metadata: attribution (source), confidence, temporal bounds (valid_from/valid_until), or any structured data.
- supersedes: pass the ID of the fact this replaces. The old fact is preserved in history. Always prefer superseding over deleting.`,
	}, ms.HandleStore)

	addWriteTool(ms, s, &mcp.Tool{
		Name: "memory_store_batch",
		Description: `Store multiple facts in a single call. Each fact is validated and stored independently — failures on individual items do not block others. Maximum 20 facts per batch.

Use this for end-of-session catch-up when multiple decisions, repos, or deferred work items need to be stored at once. Same conventions as memory_store apply to each fact.`,
	}, ms.HandleStoreBatch)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_search",
		Description: `Search stored memories using hybrid full-text and semantic search. Returns ranked results with relevance scores. Use this to recall information from previous sessions.

Search early and often — check what you already know before asking the user to repeat themselves. Search at the start of a conversation if the user's identity or project context is unclear. Search across repos, too: a fact stored about the user while working in repo A is just as relevant in repo B — that cross-repo continuity is the point of memstore.

Set include_superseded=true when you need to understand how a fact has changed over time, or to find old information that may have been prematurely superseded.

Results show a rerank=N.NNN score (0-1) when reranking is active — use it to judge whether the relevance threshold is set well, and tune it with memory_rerank_settings.`,
	}, ms.HandleSearch)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_rerank_settings",
		Description: `Get and set this session's retrieval tunables for memory_search and memory_get_context. Call with no args to read the current values; pass any subset to change them. Tune these live from what you observe — latency, and whether the right facts surface — instead of living with fixed defaults.

- mode: off | balanced | dominant | gate
  - off: no reranking (first-stage FTS+vector order)
  - balanced: blend rerank with the first-stage score (see weight)
  - dominant: cross-encoder drives the order, first stage only breaks ties
  - gate: keep first-stage order, use rerank only to filter by threshold
- threshold: 0-1; facts whose rerank relevance is below it are dropped. Defaults to 0.05, which clears the noise floor without touching genuine matches; 0 turns filtering off entirely. Raise it if irrelevant context is surfacing; lower it if relevant facts are being missed. A search that comes back empty says so when the floor is what emptied it.
- weight: 0-1; in balanced mode, rerank's share vs the first-stage score. Higher trusts the cross-encoder more. 0 resets to the engine default.
- search_candidates: how many first-stage candidates memory_search reranks. More improves recall but each is a CPU pass, so it costs latency. 0 resets to the default.
- recall_candidates: same, for memory_get_context. Keep it smaller than search if you call get_context on a tight budget.
- search_doc_bytes: truncate each search document to this many bytes before scoring. Rerank cost is superlinear in document length, so this is the strongest latency lever — lower it if search feels slow, raise it if long facts are mis-ranked on their lead content alone.
- recall_doc_bytes: same, for memory_get_context. Usually smaller than search.
- timeout_seconds: cap on how long to wait for rerank; on timeout the result degrades to first-stage order rather than blocking. 0 disables the cap.

Omit a field to leave it unchanged. Watch the rerank=N.NNN scores in memory_search output to calibrate threshold and weight.`,
	}, ms.HandleRerankSettings)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_list",
		Description: `Browse stored memories with optional subject and category filters. Unlike search, this does not require a query — use it to see what you know about a topic.

Use this when you want a complete picture of a subject rather than matching a specific query. Good for: "what do I know about this user?", "what preferences are stored?", getting an overview before a task.`,
	}, ms.HandleList)

	addWriteTool(ms, s, &mcp.Tool{
		Name: "memory_delete",
		Description: `Delete a specific memory by its ID. Use this to remove outdated or incorrect information.

Prefer memory_supersede or memory_store with the 'supersedes' parameter instead — these preserve the old fact in history. Only delete facts that are genuinely wrong or harmful, not just outdated.`,
	}, ms.HandleDelete)

	addWriteTool(ms, s, &mcp.Tool{
		Name: "memory_supersede",
		Description: `Mark an existing fact as superseded by a newer fact. Both facts must already exist. The old fact is preserved in history but excluded from normal search results.

Use this when you discover a stored fact is outdated and you've already stored the replacement. For a single-step "store and supersede", use memory_store with the supersedes parameter instead.`,
	}, ms.HandleSupersede)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_history",
		Description: `Show the supersession history for a fact (by ID) or all facts for a subject (by subject). Reveals how knowledge has evolved over time, including superseded facts with their replacement chain.

Use by ID to trace a specific fact's lineage. Use by subject for a complete audit of everything stored about an entity.`,
	}, ms.HandleHistory)

	addWriteTool(ms, s, &mcp.Tool{
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

	addWriteTool(ms, s, &mcp.Tool{
		Name: "memory_update",
		Description: `Update metadata on an existing fact without replacing the fact itself. Keys with non-nil values are set; keys with nil values are deleted.

Use this for status transitions, adding surface flags, or updating structured metadata. Does not create supersession history — use memory_store with supersedes for content changes.`,
	}, ms.HandleUpdate)

	addWriteTool(ms, s, &mcp.Tool{
		Name: "memory_task_create",
		Description: `Create a task with enforced metadata schema. Tasks are stored as facts with subject="todo" and structured metadata (kind, scope, status, priority, surface).

Scope controls ownership:
- "matthew" — user's task (reminders, personal TODOs)
- "claude" — agent's task (follow-ups, deferred work)
- "collaborative" — shared between user and agent

Tasks with status "pending" or "in_progress" have surface="startup" so they appear at session start via memory_list(metadata: {surface: "startup"}).`,
	}, ms.HandleTaskCreate)

	addWriteTool(ms, s, &mcp.Tool{
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

	addWriteTool(ms, s, &mcp.Tool{
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

	addWriteTool(ms, s, &mcp.Tool{
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

	addWriteTool(ms, s, &mcp.Tool{
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

	// Only register memory_curate_context when a real curator is configured.
	// NopCurator returns candidates unfiltered, wasting context window tokens.
	if _, nop := ms.curator.(memstore.NopCurator); !nop {
		mcp.AddTool(s, &mcp.Tool{
			Name: "memory_curate_context",
			Description: `Filter a candidate set of facts down to the most relevant subset for a task.

Call this after memory_get_context or memory_search to reduce noise before injecting
context into your working memory. A fast curation model reads the candidates and returns
only the facts that are essential to the task, with a brief rationale.

Typical flow:
  1. memory_get_context(task=...) → get candidate fact IDs
  2. memory_curate_context(task=..., fact_ids=[...], max_output=5) → get curated subset

Example: memory_curate_context(
  task="add retry logic to the RSS feed fetcher",
  fact_ids=[12, 34, 56, 78, 90, 101, 102],
  max_output=4
)`,
		}, ms.HandleCurateContext)
	}

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

	if ms.sessionStore != nil {
		addWriteTool(ms, s, &mcp.Tool{
			Name: "memory_rate_context",
			Description: `Rate a piece of context that was injected into this session. Call this immediately after processing injected context to signal whether it was useful.

score: +1 if the context was directly applicable or helped you answer/reason about the current task. -1 if it was off-topic, from an unrelated project, or actively misleading.

ref_type: "fact" for a memstore fact ID, "turn" for a session turn UUID.

Your ratings feed into future injection ranking: high-scoring refs are injected more readily, low-scoring refs are deprioritized. One rating per ref per session is recorded — duplicates are silently ignored.

session_id: pass the current session ID (available from the hook context).`,
		}, ms.HandleRateContext)
	}
}

// --- Handlers ---

func (ms *MemoryServer) HandleStore(ctx context.Context, _ *mcp.CallToolRequest, input StoreInput) (*mcp.CallToolResult, StoreResult, error) {
	if strings.TrimSpace(input.Content) == "" {
		return invalidWrite[StoreResult]("content is required")
	}
	if strings.TrimSpace(input.Subject) == "" {
		return invalidWrite[StoreResult]("subject is required")
	}

	category := strings.TrimSpace(input.Category)
	if category == "" {
		category = "note"
	}

	// Dedup check.
	exists, err := ms.store.Exists(ctx, input.Content, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error checking for duplicates: %v", err), true), StoreResult{}, nil
	}
	if exists {
		return textResult("Already stored (duplicate).", false), StoreResult{}, nil
	}

	fact := memstore.Fact{
		Content:   input.Content,
		Subject:   input.Subject,
		Category:  category,
		Kind:      strings.TrimSpace(input.Kind),
		Subsystem: strings.TrimSpace(input.Subsystem),
	}
	if len(input.Metadata) > 0 {
		metaJSON, err := json.Marshal(input.Metadata)
		if err != nil {
			return textResult(fmt.Sprintf("Error encoding metadata: %v", err), true), StoreResult{}, nil
		}
		fact.Metadata = metaJSON
	}

	id, err := ms.store.Insert(ctx, fact)
	if err != nil {
		return textResult(fmt.Sprintf("Error storing fact: %v", err), true), StoreResult{}, nil
	}

	if err := ms.embedFact(ctx, id, fact); err != nil {
		return textResult(fmt.Sprintf("Error computing embedding: %v", err), true), StoreResult{}, nil
	}

	msg := fmt.Sprintf("Stored (id=%d, subject=%q, category=%q).", id, input.Subject, category)

	// Handle supersession after successful insert.
	var supersededBy *int64
	if input.Supersedes != nil {
		if err := ms.store.Supersede(ctx, *input.Supersedes, id); err != nil {
			msg += fmt.Sprintf(" Warning: supersession of fact %d failed: %v", *input.Supersedes, err)
		} else {
			msg += fmt.Sprintf(" Superseded fact %d.", *input.Supersedes)
			supersededBy = input.Supersedes
		}
	}

	out := StoreResult{Status: "stored", ID: id, Superseded: supersededBy}
	return textResult(msg, false), out, nil
}

// SetEmbedCeiling sets the hard byte bound on any single embed request,
// normally the configured embedder's effective budget
// (embedding.Config.Limits().MaxBytes). Zero leaves chunk sizing to the
// retrieval target alone.
func (ms *MemoryServer) SetEmbedCeiling(n int) { ms.embedCeiling = n }

// embedFact computes and stores a freshly inserted fact's chunk vectors.
//
// It runs after the insert rather than supplying Fact.Embedding before it,
// because a fact is a set of vectors: chunk offsets and ordinals need the
// fact's ID to be stored against, and a long fact is several vectors rather
// than one.
//
// Routing every inline embed through memstore.EmbedFact also settles a recipe
// split that predates chunking. This path embedded the bare content while the
// daemon's embed queue embedded "subject: content", so a fact's vector depended
// on which path happened to produce it, and the two sat in different regions of
// the space. EmbedFact is now the single renderer for both.
//
// A nil embedder means daemon mode, where the queue owns embedding; the fact is
// left unembedded and NeedingEmbedding picks it up.
func (ms *MemoryServer) embedFact(ctx context.Context, id int64, f memstore.Fact) error {
	if ms.embedder == nil {
		return nil
	}
	vecs, err := memstore.EmbedFact(ctx, ms.embedder, ms.embedder.Model(), f, ms.embedCeiling)
	if err != nil {
		return err
	}
	if len(vecs.Chunks) == 0 {
		return nil
	}
	return ms.store.SetFactVectors(ctx, id, vecs)
}

func (ms *MemoryServer) HandleStoreBatch(ctx context.Context, _ *mcp.CallToolRequest, input StoreBatchInput) (*mcp.CallToolResult, StoreBatchResult, error) {
	if len(input.Facts) == 0 {
		return invalidWrite[StoreBatchResult]("facts array is required and must be non-empty")
	}
	if len(input.Facts) > 20 {
		return invalidWrite[StoreBatchResult]("maximum 20 facts per batch")
	}

	var results []BatchResult
	stored := 0
	for i, f := range input.Facts {
		if strings.TrimSpace(f.Content) == "" {
			results = append(results, BatchResult{Index: i + 1, Status: statusInvalidInput, Error: "content is required"})
			continue
		}
		if strings.TrimSpace(f.Subject) == "" {
			results = append(results, BatchResult{Index: i + 1, Status: statusInvalidInput, Error: "subject is required"})
			continue
		}

		category := strings.TrimSpace(f.Category)
		if category == "" {
			category = "note"
		}

		exists, err := ms.store.Exists(ctx, f.Content, f.Subject)
		if err != nil {
			results = append(results, BatchResult{Index: i + 1, Status: "error", Error: err.Error()})
			continue
		}
		if exists {
			results = append(results, BatchResult{Index: i + 1, Status: "skipped", Error: "duplicate"})
			continue
		}

		fact := memstore.Fact{
			Content:   f.Content,
			Subject:   f.Subject,
			Category:  category,
			Kind:      strings.TrimSpace(f.Kind),
			Subsystem: strings.TrimSpace(f.Subsystem),
		}
		if len(f.Metadata) > 0 {
			metaJSON, err := json.Marshal(f.Metadata)
			if err != nil {
				results = append(results, BatchResult{Index: i + 1, Status: "error", Error: fmt.Sprintf("metadata error: %v", err)})
				continue
			}
			fact.Metadata = metaJSON
		}

		id, err := ms.store.Insert(ctx, fact)
		if err != nil {
			results = append(results, BatchResult{Index: i + 1, Status: "error", Error: err.Error()})
			continue
		}

		if err := ms.embedFact(ctx, id, fact); err != nil {
			results = append(results, BatchResult{Index: i + 1, Status: "error", Error: fmt.Sprintf("embedding error: %v", err)})
			continue
		}

		// The fact stored; a supersede failure is surfaced (not swallowed) via
		// the result's Error field, which formatBatchResults renders.
		result := BatchResult{Index: i + 1, Status: "stored", ID: id}
		if f.Supersedes != nil {
			if err := ms.store.Supersede(ctx, *f.Supersedes, id); err != nil {
				result.Error = fmt.Sprintf("supersede failed: %v", err)
			} else {
				result.Superseded = f.Supersedes
			}
		}
		results = append(results, result)
		stored++
	}

	summary := fmt.Sprintf("Batch complete: %d/%d stored.\n%s", stored, len(input.Facts), formatBatchResults(results))
	out := StoreBatchResult{Stored: stored, Total: len(input.Facts), Results: results}
	return textResult(summary, false), out, nil
}

func formatBatchResults(results []BatchResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Error != "" {
			fmt.Fprintf(&b, "[%d] %s: %s\n", r.Index, r.Status, r.Error)
		} else if r.Superseded != nil {
			fmt.Fprintf(&b, "[%d] %s (id=%d, superseded %d)\n", r.Index, r.Status, r.ID, *r.Superseded)
		} else {
			fmt.Fprintf(&b, "[%d] %s (id=%d)\n", r.Index, r.Status, r.ID)
		}
	}
	return b.String()
}

func (ms *MemoryServer) HandleSearch(ctx context.Context, _ *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, fence.Envelope, error) {
	if strings.TrimSpace(input.Query) == "" {
		return invalidInputResult("Error: query is required")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	tun := ms.tunables()
	mode, threshold := ms.resolveRerank(input.RerankMode, input.Threshold)
	opts := memstore.SearchOpts{
		MaxResults:       limit,
		Subject:          input.Subject,
		Category:         input.Category,
		Kind:             input.Kind,
		Subsystem:        input.Subsystem,
		OnlyActive:       !input.IncludeSuperseded,
		MetadataFilters:  metadataFilters(input.Metadata),
		RerankMode:       mode,
		RerankThreshold:  &threshold,
		RerankCandidates: tun.searchCandidates,
		RerankWeight:     tun.weight,
		RerankDocBytes:   tun.searchDocBytes,
		// Stable facts (preference, identity) don't decay.
		// Ephemeral notes get 30-day half-life.
		CategoryDecay: map[string]time.Duration{
			"note": 720 * time.Hour, // 30 days
		},
	}
	var floorStats memstore.RerankStats
	opts.RerankStats = &floorStats

	// Bound rerank latency when the model set a timeout: on deadline the rerank
	// call is cancelled and the store degrades to first-stage order.
	if tun.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tun.timeout)
		defer cancel()
	}

	// Hybrid search (FTS + vector). The backing store owns query embedding —
	// SQLiteStore/PostgresStore embed locally, the remote daemon embeds
	// server-side — so route to Search regardless of whether this process holds a
	// local embedder. In daemon/remote mode ms.embedder is nil even though the
	// daemon can embed, so gating on it would wrongly drop to FTS-only and lose
	// vector recall. Fall back to FTS only if the store itself can't embed (e.g.
	// memstore-mcp --no-embeddings against a local store built with no embedder),
	// which surfaces as a Search error. Mirrors HandleGetContext.
	results, err := ms.store.Search(ctx, input.Query, opts)
	if err != nil {
		results, err = ms.store.SearchFTS(ctx, input.Query, opts)
		if err != nil {
			return noticeResult(fmt.Sprintf("Error searching: %v", err), true)
		}
	}

	if len(results) == 0 {
		// Name the floor when one is in force. An empty result means either
		// "nothing is stored on this" or "everything scored too low to return",
		// and those call for opposite reactions from the reader -- store
		// something, versus rephrase or lower the floor. A silent filter set
		// wrong is indistinguishable from an empty store (#163).
		if threshold > 0 {
			return noticeResult(fmt.Sprintf(
				"No matching memories found. Nothing scored at or above the relevance floor of %.3f; "+
					"a weaker match may exist below it (lower the threshold via memory_rerank_settings to see it).",
				threshold), false)
		}
		return noticeResult("No matching memories found.", false)
	}

	// Auto-touch: bump use_count for all returned facts.
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.Fact.ID
	}
	_ = ms.store.Touch(ctx, ids) // best-effort; don't fail the search

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	facts := make([]FactResult, 0, len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] (id=%d, score=%.3f", i+1, r.Fact.ID, r.Combined)
		if r.RerankScore > 0 {
			fmt.Fprintf(&b, ", rerank=%.3f", r.RerankScore)
		}
		fmt.Fprintf(&b, ", used=%d, confirmed=%d) %s | %s",
			r.Fact.UseCount+1, r.Fact.ConfirmedCount, // +1 because Touch just ran
			fnc.Inline(r.Fact.Subject), fnc.Inline(r.Fact.Category))
		if r.Fact.Kind != "" {
			fmt.Fprintf(&b, " | kind=%s", fnc.Inline(r.Fact.Kind))
		}
		if r.Fact.Subsystem != "" {
			fmt.Fprintf(&b, " | subsystem=%s", fnc.Inline(r.Fact.Subsystem))
		}
		if r.Fact.SupersededBy != nil {
			fmt.Fprintf(&b, " [SUPERSEDED by %d]", *r.Fact.SupersededBy)
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "%s\n", fnc.Indent(r.Fact.Content, "    "))
		b.WriteString(fnc.Metadata(r.Fact.Metadata, "metadata", "    "))
		fmt.Fprintln(&b)

		facts = append(facts, FactResult{
			ID:             r.Fact.ID,
			Subject:        r.Fact.Subject,
			Category:       r.Fact.Category,
			Kind:           r.Fact.Kind,
			Subsystem:      r.Fact.Subsystem,
			Content:        r.Fact.Content,
			Score:          r.Combined,
			RerankScore:    r.RerankScore,
			UseCount:       r.Fact.UseCount + 1,
			ConfirmedCount: r.Fact.ConfirmedCount,
			SupersededBy:   r.Fact.SupersededBy,
			Metadata:       decodeMetadata(r.Fact.Metadata),
		})
	}

	out := SearchResult{Query: input.Query, Results: facts}
	if floorStats.Dropped > 0 {
		fmt.Fprintf(&b, "%d result(s) dropped below the relevance floor of %.3f (closest miss %.4f).\n",
			floorStats.Dropped, threshold, floorStats.TopDropped)
		out.FloorDropped = floorStats.Dropped
		out.FloorTopDropped = floorStats.TopDropped
	}
	return sealedResult(fnc, b.String(), out, ids)
}

// HandleRerankSettings gets and sets the session's retrieval tunables. Omitted
// fields are left unchanged; with no fields it just reports the current values,
// so the same tool both reads and writes. The model uses it to self-tune from
// observed performance — fusion mode/threshold/weight, the search and
// get_context candidate pools, and a rerank timeout — without restarting or
// touching the daemon's env defaults.
func (ms *MemoryServer) HandleRerankSettings(_ context.Context, _ *mcp.CallToolRequest, input RerankSettingsInput) (*mcp.CallToolResult, RerankSettingsResult, error) {
	// Validate everything before mutating so a bad field leaves state untouched.
	var mode *memstore.RerankMode
	if strings.TrimSpace(input.Mode) != "" {
		m, err := memstore.ParseRerankMode(input.Mode)
		if err != nil {
			return invalidWrite[RerankSettingsResult](err.Error())
		}
		mode = &m
	}
	if input.Threshold != nil && (*input.Threshold < 0 || *input.Threshold > 1) {
		return invalidWrite[RerankSettingsResult](fmt.Sprintf("threshold %v out of range [0,1]", *input.Threshold))
	}
	if input.Weight != nil && (*input.Weight < 0 || *input.Weight > 1) {
		return invalidWrite[RerankSettingsResult](fmt.Sprintf("weight %v out of range [0,1]", *input.Weight))
	}
	if input.SearchCandidates != nil && *input.SearchCandidates < 0 {
		return invalidWrite[RerankSettingsResult]("search_candidates must be >= 0")
	}
	if input.RecallCandidates != nil && *input.RecallCandidates < 0 {
		return invalidWrite[RerankSettingsResult]("recall_candidates must be >= 0")
	}
	if input.SearchDocBytes != nil && *input.SearchDocBytes < 0 {
		return invalidWrite[RerankSettingsResult]("search_doc_bytes must be >= 0")
	}
	if input.RecallDocBytes != nil && *input.RecallDocBytes < 0 {
		return invalidWrite[RerankSettingsResult]("recall_doc_bytes must be >= 0")
	}
	if input.TimeoutSeconds != nil && *input.TimeoutSeconds < 0 {
		return invalidWrite[RerankSettingsResult]("timeout_seconds must be >= 0")
	}

	ms.mu.Lock()
	if mode != nil {
		ms.rerankMode = *mode
	}
	if input.Threshold != nil {
		ms.rerankThreshold = *input.Threshold
	}
	if input.Weight != nil {
		ms.rerankWeight = *input.Weight
	}
	if input.SearchCandidates != nil {
		ms.searchCandidates = *input.SearchCandidates
	}
	if input.RecallCandidates != nil {
		ms.recallCandidates = *input.RecallCandidates
	}
	if input.SearchDocBytes != nil {
		ms.searchDocBytes = *input.SearchDocBytes
	}
	if input.RecallDocBytes != nil {
		ms.recallDocBytes = *input.RecallDocBytes
	}
	if input.TimeoutSeconds != nil {
		ms.rerankTimeout = time.Duration(*input.TimeoutSeconds * float64(time.Second))
	}
	ms.mu.Unlock()

	t := ms.tunables()
	modeStr := string(t.mode)
	if !t.mode.Enabled() {
		modeStr = "off"
	}
	pool := func(n int) string {
		if n > 0 {
			return strconv.Itoa(n)
		}
		return "default"
	}
	weightStr := "default"
	if t.weight > 0 {
		weightStr = fmt.Sprintf("%.2f", t.weight)
	}
	timeoutStr := "none"
	if t.timeout > 0 {
		timeoutStr = t.timeout.String()
	}

	report := fmt.Sprintf("Rerank tunables: mode=%s threshold=%.3f weight=%s search_candidates=%s recall_candidates=%s search_doc_bytes=%s recall_doc_bytes=%s timeout=%s",
		modeStr, t.threshold, weightStr, pool(t.searchCandidates), pool(t.recallCandidates),
		pool(t.searchDocBytes), pool(t.recallDocBytes), timeoutStr)

	out := RerankSettingsResult{
		Mode:             modeStr,
		Threshold:        t.threshold,
		Weight:           t.weight,
		SearchCandidates: t.searchCandidates,
		RecallCandidates: t.recallCandidates,
		SearchDocBytes:   t.searchDocBytes,
		RecallDocBytes:   t.recallDocBytes,
		Timeout:          timeoutStr,
	}
	return textResult(report, false), out, nil
}

// tunablesReport renders the current retrieval tunables. A zero pool/weight is
// shown as "default" (the store/engine value); a zero timeout as "none".
func (ms *MemoryServer) tunablesReport() string {
	t := ms.tunables()
	modeStr := string(t.mode)
	if !t.mode.Enabled() {
		modeStr = "off"
	}
	pool := func(n int) string {
		if n > 0 {
			return strconv.Itoa(n)
		}
		return "default"
	}
	weightStr := "default"
	if t.weight > 0 {
		weightStr = fmt.Sprintf("%.2f", t.weight)
	}
	timeoutStr := "none"
	if t.timeout > 0 {
		timeoutStr = t.timeout.String()
	}
	return fmt.Sprintf("Rerank tunables: mode=%s threshold=%.3f weight=%s search_candidates=%s recall_candidates=%s search_doc_bytes=%s recall_doc_bytes=%s timeout=%s",
		modeStr, t.threshold, weightStr, pool(t.searchCandidates), pool(t.recallCandidates),
		pool(t.searchDocBytes), pool(t.recallDocBytes), timeoutStr)
}

func (ms *MemoryServer) HandleList(ctx context.Context, _ *mcp.CallToolRequest, input ListInput) (*mcp.CallToolResult, fence.Envelope, error) {
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
		return noticeResult(fmt.Sprintf("Error listing: %v", err), true)
	}

	if len(facts) == 0 {
		return noticeResult("No memories found.", false)
	}

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	factResults := make([]FactResult, 0, len(facts))
	for _, f := range facts {
		fmt.Fprintf(&b, "[id=%d, used=%d, confirmed=%d] %s | %s",
			f.ID, f.UseCount, f.ConfirmedCount,
			fnc.Inline(f.Subject), fnc.Inline(f.Category))
		if f.Kind != "" {
			fmt.Fprintf(&b, " | kind=%s", fnc.Inline(f.Kind))
		}
		if f.Subsystem != "" {
			fmt.Fprintf(&b, " | subsystem=%s", fnc.Inline(f.Subsystem))
		}
		fmt.Fprintf(&b, " | %s\n", f.CreatedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "%s\n", fnc.Indent(f.Content, "  "))
		b.WriteString(fnc.Metadata(f.Metadata, "metadata", "  "))
		fmt.Fprintln(&b)

		factResults = append(factResults, curatedFactResult(f))
	}
	fmt.Fprintf(&b, "%d memories listed.", len(facts))

	out := ListResult{Facts: factResults}
	return sealedResult(fnc, b.String(), out, citableFacts(out.Facts))
}

func (ms *MemoryServer) HandleDelete(ctx context.Context, _ *mcp.CallToolRequest, input DeleteInput) (*mcp.CallToolResult, DeleteResult, error) {
	if input.ID <= 0 {
		return invalidWrite[DeleteResult]("id must be a positive integer")
	}

	err := ms.store.Delete(ctx, input.ID)
	if err != nil {
		if errors.Is(err, memstore.ErrNotFound) {
			return invalidWrite[DeleteResult](err.Error())
		}
		return textResult(fmt.Sprintf("Error: %v", err), true), DeleteResult{}, nil
	}

	out := DeleteResult{Status: "deleted", ID: input.ID}
	return textResult(fmt.Sprintf("Deleted memory %d.", input.ID), false), out, nil
}

func (ms *MemoryServer) HandleStatus(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, StatusResult, error) {
	count, err := ms.store.ActiveCount(ctx)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), StatusResult{}, nil
	}

	// Aggregated in the database. This used to List every active fact and
	// count them in Go, which pulled 5,791 full rows -- content and embedding
	// included -- across the wire to build three small maps (#150).
	bd, err := ms.store.Breakdown(ctx)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), StatusResult{}, nil
	}
	subjects, categories, kinds := bd.Subjects, bd.Categories, bd.Kinds

	var b strings.Builder
	fmt.Fprintf(&b, "Active memories: %d\n\n", count)

	if len(categories) > 0 {
		fmt.Fprintln(&b, "By category:")
		for _, kv := range sortedMapDesc(categories) {
			fmt.Fprintf(&b, "  %s: %d\n", kv.key, kv.val)
		}
		fmt.Fprintln(&b)
	}

	if len(kinds) > 0 {
		fmt.Fprintln(&b, "By kind:")
		for _, kv := range sortedMapDesc(kinds) {
			fmt.Fprintf(&b, "  %s: %d\n", kv.key, kv.val)
		}
		fmt.Fprintln(&b)
	}

	if len(subjects) > 0 {
		writeSubjectSummary(&b, subjects)
	}

	topSubjects, truncated := capSubjects(subjects)
	out := StatusResult{
		ActiveCount:       count,
		Categories:        categories,
		Kinds:             kinds,
		Subjects:          topSubjects,
		SubjectCount:      len(subjects),
		SubjectsTruncated: truncated,
	}
	return textResult(b.String(), false), out, nil
}

func (ms *MemoryServer) HandleSupersede(ctx context.Context, _ *mcp.CallToolRequest, input SupersedeInput) (*mcp.CallToolResult, SupersedeResult, error) {
	if input.OldID <= 0 || input.NewID <= 0 {
		return invalidWrite[SupersedeResult]("both old_id and new_id must be positive integers")
	}
	if input.OldID == input.NewID {
		return invalidWrite[SupersedeResult]("old_id and new_id must be different")
	}

	// Validate both facts exist.
	oldFact, err := ms.store.Get(ctx, input.OldID)
	if err != nil {
		return textResult(fmt.Sprintf("Error looking up fact %d: %v", input.OldID, err), true), SupersedeResult{}, nil
	}
	if oldFact == nil {
		return invalidWrite[SupersedeResult](fmt.Sprintf("fact %d not found", input.OldID))
	}
	if oldFact.SupersededBy != nil {
		return invalidWrite[SupersedeResult](fmt.Sprintf("fact %d is already superseded by fact %d", input.OldID, *oldFact.SupersededBy))
	}

	newFact, err := ms.store.Get(ctx, input.NewID)
	if err != nil {
		return textResult(fmt.Sprintf("Error looking up fact %d: %v", input.NewID, err), true), SupersedeResult{}, nil
	}
	if newFact == nil {
		return invalidWrite[SupersedeResult](fmt.Sprintf("fact %d not found", input.NewID))
	}

	if err := ms.store.Supersede(ctx, input.OldID, input.NewID); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), SupersedeResult{}, nil
	}

	out := SupersedeResult{
		Status:     "superseded",
		OldID:      input.OldID,
		NewID:      input.NewID,
		OldContent: oldFact.Content,
		NewContent: newFact.Content,
	}
	return textResult(fmt.Sprintf("Superseded fact %d with fact %d.\n  Old: %s\n  New: %s",
		input.OldID, input.NewID, oldFact.Content, newFact.Content), false), out, nil
}

func (ms *MemoryServer) HandleHistory(ctx context.Context, _ *mcp.CallToolRequest, input HistoryInput) (*mcp.CallToolResult, fence.Envelope, error) {
	if input.ID <= 0 && strings.TrimSpace(input.Subject) == "" {
		return invalidInputResult("Error: provide either id or subject")
	}

	entries, err := ms.store.History(ctx, input.ID, input.Subject)
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	if len(entries) == 0 {
		return noticeResult("No history found.", false)
	}

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	historyEntries := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		status := "ACTIVE"
		if e.Fact.SupersededBy != nil {
			status = fmt.Sprintf("SUPERSEDED by %d", *e.Fact.SupersededBy)
		}
		fmt.Fprintf(&b, "[%d/%d] (id=%d, used=%d, confirmed=%d) %s | %s | %s | %s\n",
			e.Position+1, e.ChainLength, e.Fact.ID,
			e.Fact.UseCount, e.Fact.ConfirmedCount,
			fnc.Inline(e.Fact.Subject), fnc.Inline(e.Fact.Category),
			status, e.Fact.CreatedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "%s\n", fnc.Indent(e.Fact.Content, "  "))
		b.WriteString(fnc.Metadata(e.Fact.Metadata, "metadata", "  "))
		fmt.Fprintln(&b)

		historyEntries = append(historyEntries, HistoryEntry{
			ID:             e.Fact.ID,
			Position:       e.Position,
			ChainLength:    e.ChainLength,
			Subject:        e.Fact.Subject,
			Category:       e.Fact.Category,
			Status:         status,
			CreatedAt:      e.Fact.CreatedAt.Format("2006-01-02 15:04"),
			Content:        e.Fact.Content,
			Metadata:       decodeMetadata(e.Fact.Metadata),
			UseCount:       e.Fact.UseCount,
			ConfirmedCount: e.Fact.ConfirmedCount,
		})
	}

	out := HistoryResult{Entries: historyEntries}
	return sealedResult(fnc, b.String(), out, citableHistory(out.Entries))
}

func (ms *MemoryServer) HandleConfirm(ctx context.Context, _ *mcp.CallToolRequest, input ConfirmInput) (*mcp.CallToolResult, ConfirmResult, error) {
	if input.ID <= 0 {
		return invalidWrite[ConfirmResult]("id must be a positive integer")
	}

	if err := ms.store.Confirm(ctx, input.ID); err != nil {
		if errors.Is(err, memstore.ErrNotFound) {
			return invalidWrite[ConfirmResult](err.Error())
		}
		return textResult(fmt.Sprintf("Error: %v", err), true), ConfirmResult{}, nil
	}

	// Re-fetch to show the updated count.
	fact, err := ms.store.Get(ctx, input.ID)
	if err != nil || fact == nil {
		out := ConfirmResult{Status: "confirmed", ID: input.ID, ConfirmedCount: 0}
		return textResult(fmt.Sprintf("Confirmed fact %d.", input.ID), false), out, nil
	}

	out := ConfirmResult{
		Status:         "confirmed",
		ID:             input.ID,
		ConfirmedCount: fact.ConfirmedCount,
		Content:        fact.Content,
	}
	return textResult(fmt.Sprintf("Confirmed fact %d (count=%d). %s",
		input.ID, fact.ConfirmedCount, fact.Content), false), out, nil
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

func (ms *MemoryServer) HandleUpdate(ctx context.Context, _ *mcp.CallToolRequest, input UpdateInput) (*mcp.CallToolResult, UpdateResult, error) {
	if input.ID <= 0 {
		return invalidWrite[UpdateResult]("id must be a positive integer")
	}
	if len(input.Metadata) == 0 {
		return invalidWrite[UpdateResult]("metadata must contain at least one key")
	}

	if err := ms.store.UpdateMetadata(ctx, input.ID, input.Metadata); err != nil {
		if errors.Is(err, memstore.ErrNotFound) {
			return invalidWrite[UpdateResult](err.Error())
		}
		return textResult(fmt.Sprintf("Error: %v", err), true), UpdateResult{}, nil
	}

	out := UpdateResult{Status: "updated", ID: input.ID}
	return textResult(fmt.Sprintf("Updated metadata on fact %d.", input.ID), false), out, nil
}

func (ms *MemoryServer) HandleTaskCreate(ctx context.Context, _ *mcp.CallToolRequest, input TaskCreateInput) (*mcp.CallToolResult, TaskCreateResult, error) {
	if strings.TrimSpace(input.Content) == "" {
		return invalidWrite[TaskCreateResult]("content is required")
	}
	if !validScopes[input.Scope] {
		return invalidWrite[TaskCreateResult](fmt.Sprintf("scope must be one of: matthew, claude, collaborative (got %q)", input.Scope))
	}

	priority := input.Priority
	if priority == "" {
		priority = "normal"
	}
	if !validPriorities[priority] {
		return invalidWrite[TaskCreateResult](fmt.Sprintf("priority must be one of: high, normal, low (got %q)", priority))
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
		return textResult(fmt.Sprintf("Error encoding metadata: %v", err), true), TaskCreateResult{}, nil
	}

	task := memstore.Fact{
		Content:  input.Content,
		Subject:  "todo",
		Category: "note",
		Kind:     "task",
		Metadata: metaJSON,
	}
	id, err := ms.store.Insert(ctx, task)
	if err != nil {
		return textResult(fmt.Sprintf("Error creating task: %v", err), true), TaskCreateResult{}, nil
	}
	if err := ms.embedFact(ctx, id, task); err != nil {
		return textResult(fmt.Sprintf("Error computing embedding: %v", err), true), TaskCreateResult{}, nil
	}

	out := TaskCreateResult{Status: "created", ID: id, Scope: input.Scope, Priority: priority}
	return textResult(fmt.Sprintf("Created task (id=%d, scope=%s, priority=%s).", id, input.Scope, priority), false), out, nil
}

func (ms *MemoryServer) HandleTaskUpdate(ctx context.Context, _ *mcp.CallToolRequest, input TaskUpdateInput) (*mcp.CallToolResult, TaskUpdateResult, error) {
	if input.ID <= 0 {
		return invalidWrite[TaskUpdateResult]("id must be a positive integer")
	}
	if !validTaskStatuses[input.Status] {
		return invalidWrite[TaskUpdateResult](fmt.Sprintf("status must be one of: pending, in_progress, completed, cancelled (got %q)", input.Status))
	}

	// Verify the fact is a task.
	fact, err := ms.store.Get(ctx, input.ID)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), TaskUpdateResult{}, nil
	}
	if fact == nil {
		return invalidWrite[TaskUpdateResult](fmt.Sprintf("fact %d not found", input.ID))
	}

	meta := decodeMetadata(fact.Metadata)
	if kind, _ := meta.String("kind"); kind != "task" {
		return invalidWrite[TaskUpdateResult](fmt.Sprintf("fact %d is not a task", input.ID))
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
		return textResult(fmt.Sprintf("Error: %v", err), true), TaskUpdateResult{}, nil
	}

	out := TaskUpdateResult{Status: "updated", ID: input.ID, NewStatus: input.Status}
	return textResult(fmt.Sprintf("Task %d → %s.", input.ID, input.Status), false), out, nil
}

func (ms *MemoryServer) HandleTaskList(ctx context.Context, _ *mcp.CallToolRequest, input TaskListInput) (*mcp.CallToolResult, fence.Envelope, error) {
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
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	if len(facts) == 0 {
		return noticeResult("No tasks found.", false)
	}

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	taskResults := make([]TaskResult, 0, len(facts))
	for _, f := range facts {
		row := FormatTaskRow(fnc, f)
		b.WriteString(row)

		meta := decodeMetadata(f.Metadata)
		statusVal, _ := meta.String("status")
		scopeVal, _ := meta.String("scope")
		priorityVal, _ := meta.String("priority")
		dueVal, _ := meta.String("due")

		taskResults = append(taskResults, TaskResult{
			ID:       f.ID,
			Status:   statusVal,
			Scope:    scopeVal,
			Priority: priorityVal,
			Content:  f.Content,
			Due:      dueVal,
		})
	}
	fmt.Fprintf(&b, "\n%d task(s).", len(facts))

	out := TaskListResult{Tasks: taskResults}
	return sealedResult(fnc, b.String(), out, citableTasks(out.Tasks))
}

// NewFence mints a content fence for one response. Callers rendering rows with
// FormatTaskRow need one; see the fence package for what it protects against.
func NewFence() (fence.Fence, error) { return fence.New() }

// FormatTaskRow renders a single task fact for display in HandleTaskList output.
// Every visible field is read from the fact's stored metadata — never from request
// parameters — so the row faithfully reflects what is in the store.
//
// The task's content is stored text like any other fact, so it goes inside fnc rather
// than inline in the row. That costs the one-line row format: the metadata stays on
// the header line and the content follows it, fenced.
func FormatTaskRow(fnc fence.Fence, f memstore.Fact) string {
	meta := decodeMetadata(f.Metadata)
	status, _ := meta.String("status")
	scope, _ := meta.String("scope")
	priority, _ := meta.String("priority")
	due, _ := meta.String("due")

	var b strings.Builder
	fmt.Fprintf(&b, "[id=%d] [%s] (scope=%s, priority=%s",
		f.ID, fnc.Inline(status), fnc.Inline(scope), fnc.Inline(priority))
	if due != "" {
		fmt.Fprintf(&b, ", due=%s", fnc.Inline(due))
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "%s\n", fnc.Indent(f.Content, "  "))
	return b.String()
}

func (ms *MemoryServer) HandleListSubsystems(ctx context.Context, _ *mcp.CallToolRequest, input ListSubsystemsInput) (*mcp.CallToolResult, ListSubsystemsResult, error) {
	subsystems, err := ms.store.ListSubsystems(ctx, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), ListSubsystemsResult{}, nil
	}
	if len(subsystems) == 0 {
		if input.Subject != "" {
			return textResult(fmt.Sprintf("No subsystems found for subject %q.", input.Subject), false), ListSubsystemsResult{}, nil
		}
		return textResult("No subsystems found.", false), ListSubsystemsResult{}, nil
	}
	var b strings.Builder
	for _, s := range subsystems {
		fmt.Fprintln(&b, s)
	}
	fmt.Fprintf(&b, "\n%d subsystem(s).", len(subsystems))
	out := ListSubsystemsResult{Subsystems: subsystems}
	return textResult(b.String(), false), out, nil
}

// metadataFilters converts Metadata (from MCP input) to memstore.MetadataFilter
// equality conditions.
func metadataFilters(m Metadata) []memstore.MetadataFilter {
	if len(m) == 0 {
		return nil
	}
	filters := make([]memstore.MetadataFilter, 0, len(m))
	for k, v := range m {
		filters = append(filters, memstore.MetadataFilter{Key: k, Op: "=", Value: v})
	}
	return filters
}

// decodeMetadata converts the store's raw metadata JSON into the Metadata the
// structured output schema declares. Unparseable or absent metadata decodes to nil
// rather than failing the call: metadata is decoration on a recalled fact, not the
// fact itself.
func decodeMetadata(raw json.RawMessage) Metadata {
	if len(raw) == 0 {
		return nil
	}
	var m Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// --- Link handlers ---

func (ms *MemoryServer) HandleLink(ctx context.Context, _ *mcp.CallToolRequest, input LinkInput) (*mcp.CallToolResult, LinkResult, error) {
	if input.SourceID <= 0 {
		return invalidWrite[LinkResult]("source_id is required")
	}
	if input.TargetID <= 0 {
		return invalidWrite[LinkResult]("target_id is required")
	}
	linkType := strings.TrimSpace(input.LinkType)
	if linkType == "" {
		linkType = "reference"
	}

	id, err := ms.store.LinkFacts(ctx, input.SourceID, input.TargetID, linkType, input.Bidirectional, input.Label, input.Metadata)
	if err != nil {
		if errors.Is(err, memstore.ErrNotFound) {
			return invalidWrite[LinkResult](err.Error())
		}
		return textResult(fmt.Sprintf("Error creating link: %v", err), true), LinkResult{}, nil
	}

	dir := "directed"
	if input.Bidirectional {
		dir = "bidirectional"
	}
	out := LinkResult{
		Status:        "linked",
		LinkID:        id,
		SourceID:      input.SourceID,
		TargetID:      input.TargetID,
		LinkType:      linkType,
		Bidirectional: input.Bidirectional,
	}
	return textResult(fmt.Sprintf("Linked (link_id=%d, %d->%d, type=%q, %s).", id, input.SourceID, input.TargetID, linkType, dir), false), out, nil
}

func (ms *MemoryServer) HandleUnlink(ctx context.Context, _ *mcp.CallToolRequest, input UnlinkInput) (*mcp.CallToolResult, UnlinkResult, error) {
	if input.LinkID <= 0 {
		return invalidWrite[UnlinkResult]("link_id is required")
	}
	if err := ms.store.DeleteLink(ctx, input.LinkID); err != nil {
		if errors.Is(err, memstore.ErrNotFound) {
			return invalidWrite[UnlinkResult](err.Error())
		}
		return textResult(fmt.Sprintf("Error deleting link: %v", err), true), UnlinkResult{}, nil
	}
	out := UnlinkResult{Status: "deleted", LinkID: input.LinkID}
	return textResult(fmt.Sprintf("Deleted link %d.", input.LinkID), false), out, nil
}

func (ms *MemoryServer) HandleGetLinks(ctx context.Context, _ *mcp.CallToolRequest, input GetLinksInput) (*mcp.CallToolResult, fence.Envelope, error) {
	if input.FactID <= 0 {
		return invalidInputResult("Error: fact_id is required")
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
		return noticeResult(fmt.Sprintf("Error getting links: %v", err), true)
	}
	if len(links) == 0 {
		return noticeResult("No links found.", false)
	}

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	linkEntries := make([]LinkEntry, 0, len(links))
	fmt.Fprintf(&b, "%d link(s) for fact %d:\n", len(links), input.FactID)
	for _, l := range links {
		bidi := ""
		if l.Bidirectional {
			bidi = " [bidirectional]"
		}
		fmt.Fprintf(&b, "\n[link_id=%d] %d -> %d | type=%q%s\n", l.ID, l.SourceID, l.TargetID, fnc.Inline(l.LinkType), bidi)
		if l.Label != "" {
			fmt.Fprintf(&b, "  label: %s\n", fnc.Inline(l.Label))
		}
		b.WriteString(fnc.Metadata(l.Metadata, "link metadata", "  "))

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
			fmt.Fprintf(&b, "  neighbor: id=%d subject=%q\n%s\n", f.ID, fnc.Inline(f.Subject), fnc.Indent(preview, "  "))

			linkEntries = append(linkEntries, LinkEntry{
				ID:              l.ID,
				SourceID:        l.SourceID,
				TargetID:        l.TargetID,
				LinkType:        l.LinkType,
				Bidirectional:   l.Bidirectional,
				Label:           l.Label,
				Metadata:        decodeMetadata(l.Metadata),
				NeighborID:      f.ID,
				NeighborSubject: f.Subject,
				NeighborContent: preview,
			})
		} else {
			linkEntries = append(linkEntries, LinkEntry{
				ID:            l.ID,
				SourceID:      l.SourceID,
				TargetID:      l.TargetID,
				LinkType:      l.LinkType,
				Bidirectional: l.Bidirectional,
				Label:         l.Label,
				Metadata:      decodeMetadata(l.Metadata),
				NeighborID:    neighborID,
			})
		}
	}

	out := GetLinksResult{FactID: input.FactID, Links: linkEntries}
	return sealedResult(fnc, strings.TrimRight(b.String(), "\n"), out, citableLinks(out.FactID, out.Links))
}

func (ms *MemoryServer) HandleUpdateLink(ctx context.Context, _ *mcp.CallToolRequest, input UpdateLinkInput) (*mcp.CallToolResult, UpdateLinkResult, error) {
	if input.LinkID <= 0 {
		return invalidWrite[UpdateLinkResult]("link_id is required")
	}
	if err := ms.store.UpdateLink(ctx, input.LinkID, input.Label, input.Metadata); err != nil {
		if errors.Is(err, memstore.ErrNotFound) {
			return invalidWrite[UpdateLinkResult](err.Error())
		}
		return textResult(fmt.Sprintf("Error updating link: %v", err), true), UpdateLinkResult{}, nil
	}
	out := UpdateLinkResult{Status: "updated", LinkID: input.LinkID}
	return textResult(fmt.Sprintf("Updated link %d.", input.LinkID), false), out, nil
}

// textResult builds a CallToolResult with a single text content block.
func (ms *MemoryServer) HandleGetContext(ctx context.Context, _ *mcp.CallToolRequest, input GetContextInput) (*mcp.CallToolResult, fence.Envelope, error) {
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return invalidInputResult("Error: task is required")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}

	// Hybrid search for the task description; fall back to FTS if no embedder configured.
	tun := ms.tunables()
	mode, threshold := ms.resolveRerank(input.RerankMode, input.Threshold)
	searchOpts := memstore.SearchOpts{
		MaxResults:       limit,
		Subject:          input.Subject,
		OnlyActive:       true,
		RerankMode:       mode,
		RerankThreshold:  &threshold,
		RerankCandidates: tun.recallCandidates,
		RerankWeight:     tun.weight,
		RerankDocBytes:   tun.recallDocBytes,
	}
	if tun.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tun.timeout)
		defer cancel()
	}
	searchResults, err := ms.store.Search(ctx, task, searchOpts)
	if err != nil {
		searchResults, err = ms.store.SearchFTS(ctx, task, searchOpts)
		if err != nil {
			return noticeResult(fmt.Sprintf("Error searching: %v", err), true)
		}
	}

	// Collect unique subsystems from search results.
	seenSubsystems := make(map[string]bool)
	var subsystems []string
	for _, r := range searchResults {
		if r.Fact.Subsystem != "" && !seenSubsystems[r.Fact.Subsystem] {
			seenSubsystems[r.Fact.Subsystem] = true
			subsystems = append(subsystems, r.Fact.Subsystem)
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
		return noticeResult("No relevant context found for this task.", false)
	}

	// Count this as use, the same as a search hit: the model asked for context
	// and these facts are what it got. Without this, a fact that only ever
	// arrives through get_context looks untouched, which is exactly the blind
	// spot #157's prune predicate would act on. seen holds every ID surfaced
	// across all four sections.
	touched := make([]int64, 0, len(seen))
	for id := range seen {
		touched = append(touched, id)
	}
	_ = ms.store.Touch(ctx, touched) // best-effort; don't fail the lookup

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	invariantResults := make([]FactResult, 0, len(invariants))
	for _, f := range invariants {
		writeContextFact(&b, fnc, f)
		invariantResults = append(invariantResults, FactResult{
			ID:             f.ID,
			Subject:        f.Subject,
			Category:       f.Category,
			Kind:           f.Kind,
			Subsystem:      f.Subsystem,
			Content:        f.Content,
			Score:          0,
			UseCount:       f.UseCount,
			ConfirmedCount: f.ConfirmedCount,
			Metadata:       decodeMetadata(f.Metadata),
		})
	}

	failureModeResults := make([]FactResult, 0, len(failureModes))
	for _, f := range failureModes {
		writeContextFact(&b, fnc, f)
		failureModeResults = append(failureModeResults, FactResult{
			ID:             f.ID,
			Subject:        f.Subject,
			Category:       f.Category,
			Kind:           f.Kind,
			Subsystem:      f.Subsystem,
			Content:        f.Content,
			Score:          0,
			UseCount:       f.UseCount,
			ConfirmedCount: f.ConfirmedCount,
			Metadata:       decodeMetadata(f.Metadata),
		})
	}

	triggerResults := make([]FactResult, 0, len(triggers))
	for _, f := range triggers {
		writeContextFact(&b, fnc, f)
		triggerResults = append(triggerResults, FactResult{
			ID:             f.ID,
			Subject:        f.Subject,
			Category:       f.Category,
			Kind:           f.Kind,
			Subsystem:      f.Subsystem,
			Content:        f.Content,
			Score:          0,
			UseCount:       f.UseCount,
			ConfirmedCount: f.ConfirmedCount,
			Metadata:       decodeMetadata(f.Metadata),
		})
	}

	relevantResults := make([]FactResult, 0, len(relevant))
	fmt.Fprintf(&b, "[context for task: %q]\n", task)
	if input.Subject != "" {
		fmt.Fprintf(&b, "[subject: %s", input.Subject)
		if len(subsystems) > 0 {
			sort.Strings(subsystems)
			fmt.Fprintf(&b, ", subsystems touched: %s", strings.Join(subsystems, ", "))
		}
		fmt.Fprintf(&b, "]\n")
	}
	fmt.Fprintln(&b)

	if len(invariants) > 0 {
		fmt.Fprintf(&b, "--- invariants (always apply when touching these subsystems) ---\n")
		for _, f := range invariants {
			writeContextFact(&b, fnc, f)
		}
		fmt.Fprintln(&b)
	}

	if len(failureModes) > 0 {
		fmt.Fprintf(&b, "--- failure modes ---\n")
		for _, f := range failureModes {
			writeContextFact(&b, fnc, f)
		}
		fmt.Fprintln(&b)
	}

	if len(triggers) > 0 {
		fmt.Fprintf(&b, "--- triggered context ---\n")
		for _, f := range triggers {
			writeContextFact(&b, fnc, f)
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

			relevantResults = append(relevantResults, FactResult{
				ID:             r.Fact.ID,
				Subject:        r.Fact.Subject,
				Category:       r.Fact.Category,
				Kind:           r.Fact.Kind,
				Subsystem:      r.Fact.Subsystem,
				Content:        r.Fact.Content,
				Score:          r.Combined,
				UseCount:       r.Fact.UseCount,
				ConfirmedCount: r.Fact.ConfirmedCount,
				Metadata:       decodeMetadata(r.Fact.Metadata),
			})
		}
	}

	out := GetContextResult{
		Task:         task,
		Subject:      input.Subject,
		Invariants:   invariantResults,
		FailureModes: failureModeResults,
		Triggers:     triggerResults,
		Relevant:     relevantResults,
		Subsystems:   subsystems,
	}
	return sealedResult(fnc, b.String(), out, citableFacts(out.Invariants, out.FailureModes, out.Triggers, out.Relevant))
}

// writeContextFact writes a single fact line for the get_context output.
func writeContextFact(b *strings.Builder, fnc fence.Fence, f memstore.Fact) {
	fmt.Fprintf(b, "[id=%d] %s | %s", f.ID, fnc.Inline(f.Subject), fnc.Inline(f.Category))
	if f.Kind != "" {
		fmt.Fprintf(b, " | kind=%s", fnc.Inline(f.Kind))
	}
	if f.Subsystem != "" {
		fmt.Fprintf(b, " | subsystem=%s", fnc.Inline(f.Subsystem))
	}
	fmt.Fprintln(b)
	fmt.Fprintf(b, "%s\n", fnc.Indent(f.Content, "  "))
	if q := contextFactQuality(f); q != "" {
		fmt.Fprintf(b, "  [draft: %s — rewrite with memory_store + supersedes if you have better context]\n", q)
	}
	fmt.Fprintln(b)
}

// contextFactQuality returns the quality tag if the fact is a local-model draft, or "" otherwise.
func contextFactQuality(f memstore.Fact) string {
	if q, _ := decodeMetadata(f.Metadata).String("quality"); strings.HasPrefix(q, "local") {
		return q
	}
	return ""
}

func (ms *MemoryServer) HandleCurateContext(ctx context.Context, _ *mcp.CallToolRequest, input CurateContextInput) (*mcp.CallToolResult, fence.Envelope, error) {
	if len(input.FactIDs) == 0 {
		return invalidInputResult("Error: fact_ids is required")
	}
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return invalidInputResult("Error: task is required")
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
		return noticeResult(fmt.Sprintf("Error fetching candidates: %v", err), true)
	}
	if len(candidates) == 0 {
		return noticeResult("No active facts found for the provided fact_ids.", false)
	}

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	selected, rationale, err := ms.curator.Curate(ctx, task, candidates, maxOutput)
	if err != nil {
		// Fallback: return top maxOutput candidates unfiltered.
		fallback := candidates
		if maxOutput < len(fallback) {
			fallback = fallback[:maxOutput]
		}
		note := fmt.Sprintf("curation failed (%v); returning top %d unfiltered", err, len(fallback))
		var b strings.Builder
		b.WriteString(fnc.Preamble())
		fmt.Fprintf(&b, "[%s]\n\n", note)
		// The facts go to both channels. This path returns content, so an empty
		// envelope would drop the fallback entirely for a client reading structured
		// output -- the failure mode noticeResult exists to prevent, one step worse.
		factResults := make([]FactResult, 0, len(fallback))
		for _, f := range fallback {
			writeContextFact(&b, fnc, f)
			factResults = append(factResults, curatedFactResult(f))
		}
		out := CurateContextResult{
			Task:       task,
			Selected:   len(fallback),
			Candidates: len(candidates),
			Rationale:  note,
			Facts:      factResults,
		}
		return sealedResult(fnc, b.String(), out, citableFacts(out.Facts))
	}

	var b strings.Builder
	b.WriteString(fnc.Preamble())
	fmt.Fprintf(&b, "[curated context: %d of %d candidates selected]\n", len(selected), len(candidates))
	factResults := make([]FactResult, 0, len(selected))
	// The rationale is model-authored prose about attacker-influenceable content, so it
	// is untrusted in exactly the way the facts are -- fence it, do not print it raw.
	fmt.Fprintf(&b, "rationale:\n%s\n\n", fnc.Content(rationale))
	for _, f := range selected {
		writeContextFact(&b, fnc, f)

		factResults = append(factResults, FactResult{
			ID:             f.ID,
			Subject:        f.Subject,
			Category:       f.Category,
			Kind:           f.Kind,
			Subsystem:      f.Subsystem,
			Content:        f.Content,
			Score:          0,
			UseCount:       f.UseCount,
			ConfirmedCount: f.ConfirmedCount,
			Metadata:       decodeMetadata(f.Metadata),
		})
	}

	out := CurateContextResult{
		Task:       task,
		Selected:   len(selected),
		Candidates: len(candidates),
		Rationale:  rationale,
		Facts:      factResults,
	}
	return sealedResult(fnc, b.String(), out, citableFacts(out.Facts))
}

// triggerMatches returns true if any word from taskWords appears in the trigger content.
// HandleSuggestAgent recommends specialist agents for a task based on stored
// agent-routing facts. Scores agents by domain keyword overlap with the task.
func (ms *MemoryServer) HandleSuggestAgent(ctx context.Context, _ *mcp.CallToolRequest, input SuggestAgentInput) (*mcp.CallToolResult, fence.Envelope, error) {
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return invalidInputResult("Error: task is required")
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
		return noticeResult("No agent-routing facts found. Seed them with memory_store:\n"+
			"  subject: \"global\" (or project name), subsystem: \"agent-routing\", kind: \"convention\"\n"+
			"  metadata: {\"agent_name\": \"security-reviewer\", \"domains\": [\"security\", \"auth\"]}\n"+
			"  content: description of when to use this agent", false)
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
		meta := decodeMetadata(f.Metadata)
		agentName, _ := meta.String("agent_name")
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
		return noticeResult("No agents matched the task description. Try broader domain keywords or check stored agent-routing facts with memory_list(subsystem=\"agent-routing\").", false)
	}

	// Sort by score descending.
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	// Cap at 5 suggestions.
	if len(scores) > 5 {
		scores = scores[:5]
	}

	fnc, err := fence.New()
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}

	maxScore := scores[0].score
	var b strings.Builder
	b.WriteString(fnc.Preamble())
	suggestions := make([]AgentScore, 0, len(scores))
	fmt.Fprintf(&b, "[agent suggestions for: %q]\n\n", task)
	for _, s := range scores {
		confidence := float64(s.score) / float64(maxScore)
		level := "low"
		if confidence >= 0.8 {
			level = "high"
		} else if confidence >= 0.5 {
			level = "medium"
		}
		fmt.Fprintf(&b, "- %s (confidence: %s, score: %d)\n  %s\n%s\n\n",
			fnc.Inline(s.name), level, s.score, fnc.Inline(s.rationale), fnc.Indent(s.content, "  "))

		suggestions = append(suggestions, AgentScore{
			Name:       s.name,
			Confidence: level,
			Score:      s.score,
			Rationale:  s.rationale,
			Content:    s.content,
		})
	}

	out := SuggestAgentResult{Task: task, Suggestions: suggestions}
	return sealedResult(fnc, b.String(), out, nil)
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

func (ms *MemoryServer) HandleRateContext(ctx context.Context, _ *mcp.CallToolRequest, input RateContextInput) (*mcp.CallToolResult, RateContextResult, error) {
	if input.Score != 1 && input.Score != -1 {
		return invalidWrite[RateContextResult]("score must be 1 or -1")
	}
	if input.RefID == "" || input.RefType == "" || input.SessionID == "" {
		return invalidWrite[RateContextResult]("ref_id, ref_type, and session_id are required")
	}
	fb := memstore.ContextFeedback{
		RefID:     input.RefID,
		RefType:   input.RefType,
		SessionID: input.SessionID,
		Score:     input.Score,
		Reason:    input.Reason,
	}
	if err := ms.sessionStore.RecordFeedback(ctx, fb); err != nil {
		return textResult("Error: "+err.Error(), true), RateContextResult{}, nil
	}
	out := RateContextResult{Status: "recorded"}
	return textResult("Feedback recorded.", false), out, nil
}

func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: isError,
	}
}

// statusMaxSubjects is the maximum number of individual subjects shown in
// the status output before remaining subjects are summarized.
const statusMaxSubjects = 20

type kvPair struct {
	key string
	val int
}

func sortedMapDesc(m map[string]int) []kvPair {
	pairs := make([]kvPair, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kvPair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].val != pairs[j].val {
			return pairs[i].val > pairs[j].val
		}
		return pairs[i].key < pairs[j].key
	})
	return pairs
}

// capSubjects reduces the subject histogram to the highest-count
// statusMaxSubjects entries, reporting whether anything was dropped. It shares
// sortedMapDesc with writeSubjectSummary so the structured channel and the text
// channel always show the same subjects rather than two different top-20s.
func capSubjects(subjects map[string]int) (map[string]int, bool) {
	if len(subjects) <= statusMaxSubjects {
		return subjects, false
	}
	sorted := sortedMapDesc(subjects)
	capped := make(map[string]int, statusMaxSubjects)
	for _, kv := range sorted[:statusMaxSubjects] {
		capped[kv.key] = kv.val
	}
	return capped, true
}

func writeSubjectSummary(b *strings.Builder, subjects map[string]int) {
	fmt.Fprintf(b, "By subject: (%d unique)\n", len(subjects))
	if len(subjects) == 0 {
		return
	}
	sorted := sortedMapDesc(subjects)
	shown := min(len(sorted), statusMaxSubjects)
	for _, kv := range sorted[:shown] {
		fmt.Fprintf(b, "  %s: %d\n", kv.key, kv.val)
	}
	if remaining := len(sorted) - shown; remaining > 0 {
		fmt.Fprintf(b, "  ... and %d more\n", remaining)
	}
}

// sealedResult is the common tail of every read tool: the human-readable text block
// on one side, the fenced structured envelope on the other.
//
// Both channels reach a model and clients choose between them without telling the
// server which they took, so neither may be the unprotected one. The text block
// carries fnc's preamble and per-fact fences; this seals the same data for the
// structured channel.
//
// A seal failure returns an error result rather than the bare struct. Falling back to
// unfenced output would drop the protection at exactly the moment it could not be
// established, and nothing downstream could tell the difference.
func sealedResult(fnc fence.Fence, text string, v any, citable []int64) (*mcp.CallToolResult, fence.Envelope, error) {
	env, err := fnc.Seal(v, citable)
	if err != nil {
		return noticeResult(fmt.Sprintf("Error: %v", err), true)
	}
	return textResult(text, false), env, nil
}

// noticeResult is sealedResult's counterpart for a result with nothing to seal: a
// validation error, a store failure, an empty result set.
//
// The same reasoning applies -- both channels reach a model and the server is not
// told which one the client took -- so a message delivered to only one of them is a
// message the reader may never see. fence.Notice puts it in the envelope's framing,
// which is memstore's own voice, and leaves the payload empty.
func noticeResult(text string, isError bool) (*mcp.CallToolResult, fence.Envelope, error) {
	return textResult(text, isError), fence.Notice(text), nil
}

// statusInvalidInput is the status a write tool reports when the call never
// became a request it could act on. It exists because the zero value of Status
// is the empty string, which a structured-output client cannot tell from a
// result that was simply never filled in -- the same failure the read tools had
// before they carried framing on both channels.
const statusInvalidInput = "invalid_input"

// invalidWrite is invalidInputResult for the write and config tools, which
// return their own typed struct rather than a fence envelope. Same rule: IsError
// stays down, because nothing failed -- the arguments never described a request.
// The struct says it was rejected and why, so a client reading only structured
// output learns both, and the text channel still opens with "Error:" for one
// reading only that.
//
// P is inferred as *T from the constraint, so call sites name the result type
// once: invalidWrite[StoreResult]("content is required").
func invalidWrite[T any, P rejecter[T]](msg string) (*mcp.CallToolResult, T, error) {
	var out T
	P(&out).reject(msg)
	return textResult("Error: "+msg, false), out, nil
}

// rejecter is the pointer-to-T constraint that lets invalidWrite fill in the
// rejection fields without reflection or a type switch. Each write result
// implements it; a new one that forgets to fails to compile at its first
// invalidWrite call rather than silently returning an unmarked zero value.
type rejecter[T any] interface {
	*T
	reject(msg string)
}

func (r *StoreResult) reject(msg string)          { r.Status = statusInvalidInput; r.Error = msg }
func (r *DeleteResult) reject(msg string)         { r.Status = statusInvalidInput; r.Error = msg }
func (r *SupersedeResult) reject(msg string)      { r.Status = statusInvalidInput; r.Error = msg }
func (r *ConfirmResult) reject(msg string)        { r.Status = statusInvalidInput; r.Error = msg }
func (r *UpdateResult) reject(msg string)         { r.Status = statusInvalidInput; r.Error = msg }
func (r *TaskCreateResult) reject(msg string)     { r.Status = statusInvalidInput; r.Error = msg }
func (r *TaskUpdateResult) reject(msg string)     { r.Status = statusInvalidInput; r.Error = msg }
func (r *LinkResult) reject(msg string)           { r.Status = statusInvalidInput; r.Error = msg }
func (r *UnlinkResult) reject(msg string)         { r.Status = statusInvalidInput; r.Error = msg }
func (r *UpdateLinkResult) reject(msg string)     { r.Status = statusInvalidInput; r.Error = msg }
func (r *RateContextResult) reject(msg string)    { r.Status = statusInvalidInput; r.Error = msg }
func (r *StoreBatchResult) reject(msg string)     { r.Error = msg }
func (r *RerankSettingsResult) reject(msg string) { r.Error = msg }

// invalidInputResult reports a caller-side mistake -- a required argument missing, or a
// combination of arguments that never formed a request memstore could act on.
//
// It deliberately does not set IsError. IsError means memstore failed at something it
// was asked to do, and a client that sees it may show the text content and discard the
// structured channel, which throws away the framing that says what was wrong. Nothing
// failed here, so nothing is owed to the error channel; the framing carries the whole
// message on both channels, and the text still begins "Error:" for a text-only reader.
func invalidInputResult(text string) (*mcp.CallToolResult, fence.Envelope, error) {
	return noticeResult(text, false)
}

// curatedFactResult converts a stored fact to its result form for memory_curate_context.
// Score is zero on both curation paths: the curator ranks by selection rather than by
// a comparable score, and the fallback returns candidates in list order.
func curatedFactResult(f memstore.Fact) FactResult {
	return FactResult{
		ID:             f.ID,
		Subject:        f.Subject,
		Category:       f.Category,
		Kind:           f.Kind,
		Subsystem:      f.Subsystem,
		Content:        f.Content,
		Score:          0,
		UseCount:       f.UseCount,
		ConfirmedCount: f.ConfirmedCount,
		Metadata:       decodeMetadata(f.Metadata),
	}
}

// citableFacts collects the fact ids a model may cite from one or more result
// sections. Passed to Seal, they are rendered into the framing outside the fence --
// the sealed payload holds ids too, but those are stored text and carry no more
// authority than the content around them.
func citableFacts(sections ...[]FactResult) []int64 {
	var ids []int64
	for _, s := range sections {
		for _, f := range s {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// citableHistory is citableFacts for memory_history, whose entries are revisions of
// one fact chain rather than FactResults.
func citableHistory(entries []HistoryEntry) []int64 {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// citableTasks is citableFacts for memory_task_list. Task rows are facts, so their
// ids cite the same way.
func citableTasks(tasks []TaskResult) []int64 {
	ids := make([]int64, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

// citableLinks collects the ids reachable from memory_get_links: the anchor fact and
// every neighbor. Link ids are deliberately excluded -- a link is not a memory, and
// citing one as a fact would point at an edge rather than at anything the model read.
func citableLinks(anchor int64, links []LinkEntry) []int64 {
	ids := []int64{anchor}
	for _, l := range links {
		if l.NeighborID != 0 {
			ids = append(ids, l.NeighborID)
		}
	}
	return ids
}
