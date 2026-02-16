package memstore

import (
	"context"
	"encoding/json"
	"time"
)

// Fact represents a single factual claim in the knowledge store.
type Fact struct {
	ID           int64
	Namespace    string          // partition key; set automatically by the store on insert
	Content      string          // the factual claim
	Subject      string          // entity being described
	Category     string          // freeform: "character", "preference", "identity", etc.
	Metadata     json.RawMessage // domain-specific extensions (nullable)
	SupersededBy *int64          // points to replacing fact
	SupersededAt *time.Time      // when supersession occurred
	Embedding    []float32       // nil until computed
	CreatedAt    time.Time
}

// MetadataFilter applies a condition on a JSON metadata field.
// The Key is a top-level field name in the metadata JSON object.
// Supported operators: "=", "!=", "<", "<=", ">", ">=".
// Value is compared using SQLite's json_extract(); rows with NULL
// metadata or missing keys are excluded by comparison semantics.
type MetadataFilter struct {
	Key   string // JSON field name (e.g., "chapter", "is_draft")
	Op    string // comparison operator
	Value any    // value to compare against
}

// SearchOpts controls search behavior.
type SearchOpts struct {
	MaxResults      int              // default 20
	Category        string           // filter (empty = all)
	OnlyActive      bool             // exclude superseded
	Namespaces      []string         // search only these namespaces; empty means the store's own namespace
	MetadataFilters []MetadataFilter // filter on metadata JSON fields
	FTSWeight       float64          // default 0.6
	VecWeight       float64          // default 0.4
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
	OnlyActive      bool             // exclude superseded
	Namespaces      []string         // list only these namespaces; empty means the store's own namespace
	MetadataFilters []MetadataFilter // filter on metadata JSON fields
	Limit           int              // max results (0 = no limit)
}

// Store provides fact storage with hybrid FTS5+vector search.
type Store interface {
	// Writes
	Insert(ctx context.Context, f Fact) (int64, error)
	InsertBatch(ctx context.Context, facts []Fact) error
	Supersede(ctx context.Context, oldID, newID int64) error
	Delete(ctx context.Context, id int64) error

	// Reads
	Get(ctx context.Context, id int64) (*Fact, error)
	List(ctx context.Context, opts QueryOpts) ([]Fact, error)
	BySubject(ctx context.Context, subject string, onlyActive bool) ([]Fact, error)
	Exists(ctx context.Context, content, subject string) (bool, error)
	ActiveCount(ctx context.Context) (int64, error)

	// Hybrid search (FTS5 + vector, degrades to FTS-only if no embedder)
	Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)

	// Embedding pipeline
	NeedingEmbedding(ctx context.Context, limit int) ([]Fact, error)
	SetEmbedding(ctx context.Context, id int64, emb []float32) error
	EmbedFacts(ctx context.Context, batchSize int) (int, error)

	Close() error
}
