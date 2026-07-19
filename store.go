package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MaxContentLength bounds Fact.Content. Sized to fit comfortably inside the
// 2048-token context window of the embedding models we use (nomic-embed-text,
// embeddinggemma) at typical English byte/token ratios. Enforced at the DB
// level by both pgstore and sqlite stores.
const MaxContentLength = 8000

// ScreenMode selects how the model half of injection screening participates in writes.
//
// The regex screen is not part of this choice: it runs inline on every write in every
// mode, and nothing enters the store without it.
type ScreenMode string

const (
	// ScreenModeOff runs no model screen. Regex is the whole screen and a clean write
	// is readable immediately. The default, and the only sensible setting where no
	// screening worker runs.
	ScreenModeOff ScreenMode = "off"

	// ScreenModeObserve screens with the model but gates nothing. Writes are readable
	// at once and the verdict lands in the findings table.
	//
	// This is what to run before enforcing: it produces the model's opinion on real
	// traffic with no user-visible change, so a false-positive rate can be measured on
	// live writes rather than guessed at.
	ScreenModeObserve ScreenMode = "observe"

	// ScreenModeGate holds writes unreadable until the model clears them, and blocks
	// what the policy rejects.
	//
	// Note the latency this introduces: a fact is invisible from the moment it is
	// written until the worker screens it -- one tick plus one model call, so roughly
	// 30-60s at the default interval. That is harmless for a store read minutes or
	// sessions later, and will break anything that writes a fact and reads it back
	// immediately, tests especially.
	ScreenModeGate ScreenMode = "gate"
)

// ParseScreenMode validates a mode string, defaulting an empty value to off.
func ParseScreenMode(s string) (ScreenMode, error) {
	switch ScreenMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ScreenModeOff:
		return ScreenModeOff, nil
	case ScreenModeObserve:
		return ScreenModeObserve, nil
	case ScreenModeGate:
		return ScreenModeGate, nil
	default:
		return "", fmt.Errorf("memstore: unknown screen mode %q (want off, observe, or gate)", s)
	}
}

// ScreenState is a fact's position in the injection-screening lifecycle.
//
// Screening runs asynchronously: a write lands durable but unreadable, and a
// background worker decides its fate afterwards. This value is what makes
// "unreadable" true, and it is enforced in SQL on every fact read rather than by a
// caller-supplied option. Unlike OnlyActive, which callers legitimately turn off to
// inspect superseded history, there is no query that may see unscreened content.
type ScreenState string

const (
	// ScreenPending is a write awaiting its screen. Not readable.
	ScreenPending ScreenState = "pending"

	// ScreenClean passed screening. Readable.
	ScreenClean ScreenState = "clean"

	// ScreenBlocked failed screening. Not readable. The row is retained rather than
	// deleted so a block can be reviewed and, if it turns out to be a false
	// positive, released.
	ScreenBlocked ScreenState = "blocked"

	// ScreenAbandoned could not be screened after repeated attempts. Not readable.
	// The content is kept for review but never served and never retried -- see the
	// worker's MaxAttempts.
	ScreenAbandoned ScreenState = "abandoned"

	// ScreenScreening passed the regex screen and is readable, with a model screen
	// still outstanding. Readable.
	//
	// This is observe mode: the model verdict is recorded as a finding but gates
	// nothing, so a write is never held out of reads waiting for it. It satisfies the
	// rule that nothing enters unscreened -- regex screened it -- while producing the
	// model's opinion on live traffic at no cost to availability.
	//
	// It provides no protection FROM the model screen. A fact the model would rate 10
	// is readable the whole time. That is the trade being made deliberately: this mode
	// exists to measure, not to defend.
	ScreenScreening ScreenState = "screening"

	// ScreenRegexClean passed the regex screen with no model screen available.
	// Readable.
	//
	// This is a weaker guarantee than ScreenClean and is recorded as its own state so
	// nothing pretends otherwise: regex is defeated by paraphrase, so a regex-clean
	// fact has been checked against known phrasings and nothing more. It exists
	// because the alternative in a model-less deployment is admitting content with no
	// screen at all, and a weak screen beats none.
	ScreenRegexClean ScreenState = "regex-clean"

	// ScreenGrandfathered predates screening. Readable.
	//
	// Facts already in the store when screening was introduced are readable on
	// arrival. The alternative -- defaulting the existing corpus to pending -- would
	// make every memory disappear the moment the migration ran and stay gone for
	// hours while the backlog drained, which is a worse failure than the one
	// screening prevents. They stay distinguishable from ScreenClean precisely
	// because they have not actually been checked, and the backfill works through
	// them.
	ScreenGrandfathered ScreenState = "grandfathered"
)

// ScreenReadableSQL returns the predicate restricting a query to readable facts.
// prefix is the table alias including its dot ("f." in joined queries), or "".
//
// This is appended unconditionally wherever facts are selected, next to the namespace
// filter. Pairing it with namespace is deliberate: namespace is the one predicate
// every fact query already has, so "did you filter the namespace?" and "did you filter
// unscreened content?" become the same question, and a new query cannot quietly
// acquire one without the other.
//
// The states are inlined rather than parameterized because this string is concatenated
// into queries built with positional placeholders, where injecting extra arguments
// would renumber everything after it. They are compile-time constants, not input.
func ScreenReadableSQL(prefix string) string {
	return " AND " + prefix + "screen_state IN ('clean','regex-clean','grandfathered','screening')"
}

// ScreenNotRejectedSQL matches facts that have not been rejected -- readable ones and
// those still awaiting a verdict, but not blocked or abandoned.
//
// This is for the internal paths that should act on a fact before it is readable:
// embedding it so it is searchable the moment it clears, and the duplicate check,
// which must see a pending write to avoid queueing the same content twice. Neither
// returns content to a caller.
func ScreenNotRejectedSQL(prefix string) string {
	return " AND " + prefix + "screen_state NOT IN ('blocked','abandoned')"
}

// Fact represents a single factual claim in the knowledge store.
type Fact struct {
	ID              int64
	Namespace       string          // partition key; set automatically by the store on insert
	UserID          int64           `json:"user_id,omitempty"` // owning user; set automatically from the store's resolved identity
	Content         string          // the factual claim
	Subject         string          // topic of the fact (not ownership -- see UserID)
	Category        string          // freeform: "character", "preference", "identity", etc.
	Kind            string          // structural type: convention | failure_mode | invariant | pattern | decision | trigger | task | ""
	Subsystem       string          // optional project subsystem (e.g. "feeds", "auth"); scopes facts within a subject
	Metadata        json.RawMessage // domain-specific extensions (nullable)
	SupersededBy    *int64          // points to replacing fact
	SupersededAt    *time.Time      // when supersession occurred
	ConfirmedCount  int             // explicit "I verified this is accurate" count
	LastConfirmedAt *time.Time      // when last confirmed
	UseCount        int             // auto-incremented when retrieved via search
	LastUsedAt      *time.Time      // when last retrieved
	Embedding       []float32       // nil until computed
	CreatedAt       time.Time
}

// MetadataFilter applies a condition on a JSON metadata field.
// The Key is a top-level field name in the metadata JSON object.
// Supported operators: "=", "!=", "<", "<=", ">", ">=".
// Value is compared using SQLite's json_extract(); rows with NULL
// metadata or missing keys are excluded by comparison semantics
// unless IncludeNull is set.
type MetadataFilter struct {
	Key         string // JSON field name (e.g., "chapter", "is_draft")
	Op          string // comparison operator
	Value       any    // value to compare against
	IncludeNull bool   // if true, also match rows where Key is absent or metadata is NULL
}

// RerankMode selects how a second-stage cross-encoder rerank score is fused
// into the final ranking. A threshold (SearchOpts.RerankThreshold) applies
// independently in every enabled mode.
type RerankMode string

const (
	// RerankOff disables reranking. The zero value, so rerank is opt-in.
	RerankOff RerankMode = ""
	// RerankBalanced fuses rerank with the first-stage score:
	// Combined = RerankWeight*rerank + (1-RerankWeight)*firstStage. The first
	// stage (FTS + vector) still votes, so a strong keyword/semantic/feedback
	// signal can outweigh a middling rerank score.
	RerankBalanced RerankMode = "balanced"
	// RerankDominant makes the cross-encoder authoritative for ordering: the
	// rerank score is the rank key, with the first-stage score only breaking
	// ties. Best when the first stage surfaces wrong facts.
	RerankDominant RerankMode = "dominant"
	// RerankGate leaves the first-stage order intact and uses rerank only to
	// filter: it reorders nothing, but RerankThreshold still drops low-relevance
	// facts. Smallest behavioral change; attacks false positives, not ordering.
	RerankGate RerankMode = "gate"
)

// Enabled reports whether the mode triggers reranking. "off" (any case) and the
// empty string are disabled.
func (m RerankMode) Enabled() bool {
	switch strings.ToLower(string(m)) {
	case "", "off":
		return false
	default:
		return true
	}
}

// ParseRerankMode validates and normalizes a mode string (case-insensitive).
// "" and "off" map to RerankOff; an unknown value is an error.
func ParseRerankMode(s string) (RerankMode, error) {
	switch m := RerankMode(strings.ToLower(strings.TrimSpace(s))); m {
	case RerankOff, "off":
		return RerankOff, nil
	case RerankBalanced, RerankDominant, RerankGate:
		return m, nil
	default:
		return RerankOff, fmt.Errorf("memstore: unknown rerank mode %q (want off|balanced|dominant|gate)", s)
	}
}

// SearchOpts controls search behavior.
type SearchOpts struct {
	MaxResults      int                      // default 20
	Subject         string                   // filter by subject (empty = all)
	Category        string                   // filter (empty = all)
	Kind            string                   // filter by kind (empty = all)
	Subsystem       string                   // filter by subsystem (empty = all)
	OnlyActive      bool                     // exclude superseded
	AllNamespaces   bool                     // search across all namespaces (ignores Namespaces field)
	Namespaces      []string                 // search only these namespaces; empty means the store's own namespace
	MetadataFilters []MetadataFilter         // filter on metadata JSON fields
	CreatedAfter    *time.Time               // exclude facts created before this time
	CreatedBefore   *time.Time               // exclude facts created after this time
	DecayHalfLife   time.Duration            // if >0, default exponential time decay for combined scores
	CategoryDecay   map[string]time.Duration // per-category half-life overrides; 0 = no decay for that category
	FTSWeight       float64                  // default 0.6
	VecWeight       float64                  // default 0.4
	// RerankMode selects the second-stage rerank fusion algorithm. Empty (or
	// RerankOff) disables rerank entirely — the engine never calls the reranker
	// unless a mode is set, so callers that don't want rerank (e.g. background
	// extraction) simply leave it off. Requires a reranker on the store.
	RerankMode RerankMode
	// RerankCandidates is how many top first-stage results are sent to the
	// reranker for rescoring; 0 uses the default (40) when rerank is enabled.
	// Larger pools improve recall at the cost of per-query rerank latency.
	RerankCandidates int
	// RerankWeight is rerank's share of the fused score in RerankBalanced mode,
	// in [0,1]: Combined = RerankWeight*rerankScore + (1-RerankWeight)*firstStage.
	// 0 uses the default (0.7). Ignored by RerankDominant and RerankGate.
	RerankWeight float64
	// RerankDocBytes, when > 0, truncates each candidate document to this many
	// bytes before the cross-encoder scores it (passed through as the reranker's
	// MaxDocumentBytes). Rerank latency is superlinear in document length, so
	// this is the per-call latency lever; 0 falls back to the reranker model's
	// registered budget. Truncation ranks on each document's lead content.
	RerankDocBytes int
	// RerankThreshold, when > 0, drops any reranked fact whose normalized [0,1]
	// rerank score is below it — the "don't surface wrong context" filter. It
	// applies in every rerank mode and only when rerank actually ran (a degraded
	// backend never filters, so an outage cannot empty the result set). Facts
	// outside the reranked pool are excluded when a threshold is set, since the
	// reranker did not vouch for them.
	RerankThreshold float64
}

// SearchResult holds a fact with its relevance scores.
type SearchResult struct {
	Fact     Fact
	FTSScore float64
	VecScore float64
	// RerankScore is the reranker's normalized [0,1] relevance for this fact,
	// set only when a reranker rescored it; 0 otherwise.
	RerankScore float64
	Combined    float64
}

// QueryOpts controls filtering for List queries.
type QueryOpts struct {
	Subject         string           // filter by subject (empty = all)
	Category        string           // filter by category (empty = all)
	Kind            string           // filter by kind (empty = all)
	Subsystem       string           // filter by subsystem (empty = all)
	OnlyActive      bool             // exclude superseded
	Namespaces      []string         // list only these namespaces; empty means the store's own namespace
	MetadataFilters []MetadataFilter // filter on metadata JSON fields
	CreatedAfter    *time.Time       // exclude facts created before this time
	CreatedBefore   *time.Time       // exclude facts created after this time
	Limit           int              // max results (0 = no limit)
	IDs             []int64          // fetch only these specific fact IDs (empty = no filter)
}

// HistoryEntry wraps a Fact with its position in a supersession chain.
type HistoryEntry struct {
	Fact        Fact
	Position    int // 0-based, oldest first
	ChainLength int
}

// LinkDirection controls which edges are returned by GetLinks.
type LinkDirection int

const (
	// LinkOutbound returns edges where the fact is the source, plus bidirectional
	// edges where the fact is the target (i.e. all edges traversable FROM this fact).
	LinkOutbound LinkDirection = iota
	// LinkInbound returns edges where the fact is the target, plus bidirectional
	// edges where the fact is the source (i.e. all edges that can reach this fact).
	LinkInbound
	// LinkBoth returns all edges that touch this fact regardless of directionality.
	LinkBoth
)

// Link is a directed graph edge between two facts.
type Link struct {
	ID            int64
	SourceID      int64
	TargetID      int64
	LinkType      string          // e.g. "passage", "event", "entrance", "reference"
	Bidirectional bool            // if true, traversable in both directions
	Label         string          // human-readable description
	Metadata      json.RawMessage // domain-specific properties (nullable)
	CreatedAt     time.Time
}

// Store provides fact storage with hybrid FTS5+vector search.
type Store interface {
	// Writes
	Insert(ctx context.Context, f Fact) (int64, error)
	InsertBatch(ctx context.Context, facts []Fact) error
	Supersede(ctx context.Context, oldID, newID int64) error
	Confirm(ctx context.Context, id int64) error
	Touch(ctx context.Context, ids []int64) error // bump use_count for retrieved facts
	Delete(ctx context.Context, id int64) error
	// UpdateMetadata merges a patch into the fact's existing metadata JSON.
	// Keys with non-nil values are set; keys with nil values are deleted.
	// Does not trigger FTS re-index or re-embedding.
	UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error

	// Reads
	Get(ctx context.Context, id int64) (*Fact, error)
	List(ctx context.Context, opts QueryOpts) ([]Fact, error)
	BySubject(ctx context.Context, subject string, onlyActive bool) ([]Fact, error)
	Exists(ctx context.Context, content, subject string) (bool, error)
	ActiveCount(ctx context.Context) (int64, error)
	// History returns the supersession chain for a fact. If id > 0, it walks
	// the chain containing that fact. If subject is non-empty (and id == 0),
	// it returns all facts for that subject ordered by creation time.
	History(ctx context.Context, id int64, subject string) ([]HistoryEntry, error)

	// Hybrid search (FTS5 + vector); requires an embedder.
	Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)
	// SearchBatch shares a single batched embedding call across queries.
	SearchBatch(ctx context.Context, queries []string, opts SearchOpts) ([][]SearchResult, error)
	// SearchFTS performs FTS5-only search without requiring an embedder.
	// Results are ranked by BM25 score. Useful when Ollama is unavailable
	// or when low-latency retrieval is required (e.g. hook contexts).
	SearchFTS(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)

	// ListSubsystems returns all distinct non-empty subsystem values,
	// optionally filtered by subject (empty = all subjects).
	ListSubsystems(ctx context.Context, subject string) ([]string, error)

	// Embedding pipeline
	NeedingEmbedding(ctx context.Context, limit int) ([]Fact, error)
	SetEmbedding(ctx context.Context, id int64, emb []float32) error
	// MarkEmbedFailed quarantines a fact whose embedding failed permanently
	// so NeedingEmbedding stops returning it. reason is stored for diagnostics.
	MarkEmbedFailed(ctx context.Context, id int64, reason string) error
	EmbedFacts(ctx context.Context, batchSize int) (int, error)

	// Links — explicit graph edges between facts.
	// LinkFacts creates a directed edge from sourceID to targetID.
	// If bidirectional is true, the edge is traversable in both directions.
	// linkType is a short discriminator string (e.g. "passage", "event").
	// label is a human-readable description of the edge (may be empty).
	// metadata holds domain-specific properties (may be nil).
	LinkFacts(ctx context.Context, sourceID, targetID int64, linkType string, bidirectional bool, label string, metadata map[string]any) (int64, error)
	// GetLink retrieves a single link by ID. Returns (nil, nil) when no link
	// with that ID is visible in the caller's scope (absent, or owned by another
	// user) -- matching Get's not-found contract. A non-nil error signals a real
	// failure (query/scan), never a plain miss.
	GetLink(ctx context.Context, linkID int64) (*Link, error)
	// GetLinks returns edges touching factID filtered by direction.
	// If linkTypes is non-empty, only edges with a matching link_type are returned.
	GetLinks(ctx context.Context, factID int64, direction LinkDirection, linkTypes ...string) ([]Link, error)
	// UpdateLink patches the label and/or metadata of an existing link.
	// Pass an empty label to leave it unchanged. Metadata keys with nil values are deleted.
	UpdateLink(ctx context.Context, linkID int64, label string, metadata map[string]any) error
	// DeleteLink removes a link by ID.
	DeleteLink(ctx context.Context, linkID int64) error

	Close() error
}

// UserScoper is implemented by backends that support per-user scoping.
// ForUser returns a store whose every read and write is scoped to the
// given user. Backends without multi-user support (SQLite) do not
// implement it.
type UserScoper interface {
	ForUser(userID int64) (Store, error)
}

// TermCounter is optionally implemented by stores that support IDF-based
// keyword extraction. TermDocCounts returns the document frequency for each
// term and the total number of active documents in the store's namespace.
type TermCounter interface {
	TermDocCounts(ctx context.Context, terms []string) (counts map[string]int, totalDocs int, err error)
}

// Generator produces text completions from a prompt.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
	// Model returns the model identifier used for generation (e.g. "qwen2.5:7b").
	Model() string
}

// JSONGenerator is optionally implemented by generators that support
// structured JSON output mode for more reliable parsing.
type JSONGenerator interface {
	Generator
	GenerateJSON(ctx context.Context, prompt string) (string, error)
}

// JSONSchemaGenerator is optionally implemented by generators that support
// schema-constrained JSON output (OpenAI structured outputs / json_schema).
// Stronger than json_object: enums, required fields, and additionalProperties
// constraints can be enforced server-side, eliminating whole classes of
// format-lapse failure. Whether the constraint is actually enforced depends
// on the upstream model and proxy; even when treated as a hint it tightens
// generation noticeably.
type JSONSchemaGenerator interface {
	Generator
	// GenerateJSONSchema asks the model to return JSON matching the given
	// schema. name is a short identifier passed through to the API (used by
	// some providers for caching and telemetry). schema is a JSON Schema
	// object (typically map[string]any).
	GenerateJSONSchema(ctx context.Context, prompt, name string, schema any) (string, error)
}
