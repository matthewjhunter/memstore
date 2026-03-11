package memstore

import (
	"context"
	"encoding/json"
	"time"
)

// Fact represents a single factual claim in the knowledge store.
type Fact struct {
	ID              int64
	Namespace       string          // partition key; set automatically by the store on insert
	Content         string          // the factual claim
	Subject         string          // entity being described
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
}

// SearchResult holds a fact with its relevance scores.
type SearchResult struct {
	Fact     Fact
	FTSScore float64
	VecScore float64
	Combined float64
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
	EmbedFacts(ctx context.Context, batchSize int) (int, error)

	// Links — explicit graph edges between facts.
	// LinkFacts creates a directed edge from sourceID to targetID.
	// If bidirectional is true, the edge is traversable in both directions.
	// linkType is a short discriminator string (e.g. "passage", "event").
	// label is a human-readable description of the edge (may be empty).
	// metadata holds domain-specific properties (may be nil).
	LinkFacts(ctx context.Context, sourceID, targetID int64, linkType string, bidirectional bool, label string, metadata map[string]any) (int64, error)
	// GetLink retrieves a single link by ID. Returns an error if not found.
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
