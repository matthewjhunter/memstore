package memstore

import (
	"context"
	"time"
)

// SessionTurn is a single user or assistant text turn extracted from a
// Claude Code JSONL transcript.
type SessionTurn struct {
	SessionID string
	UUID      string
	TurnIndex int
	Role      string // "user" or "assistant"
	Content   string
	CWD       string
	CreatedAt time.Time
}

// ContextHint is a proactively retrieved context suggestion produced by the
// Ollama pipeline and consumed by the next UserPromptSubmit hook injection.
type ContextHint struct {
	ID              int64              `json:"id"`
	SessionID       string             `json:"session_id"`
	CWD             string             `json:"cwd"` // working directory of the generating session, for cross-session lookup
	TurnIndex       int                `json:"turn_index"`
	HintText        string             `json:"hint_text"`
	RefIDs          []string           `json:"ref_ids"`          // selected candidate fact IDs (positives)
	RetrievedIDs    []string           `json:"retrieved_ids"`    // all candidate fact IDs before selection (positives + negatives)
	CandidateScores map[string]float64 `json:"candidate_scores"` // {fact_id_str: vec_score} for all candidates
	SearchQuery     string             `json:"search_query"`     // query used for the Searcher stage
	RankerVersion   string             `json:"ranker_version"`   // pipeline version at generation time
	Relevance       float64            `json:"relevance"`
	Desirability    float64            `json:"desirability"`
	CreatedAt       time.Time          `json:"created_at"`
	ConsumedAt      *time.Time         `json:"consumed_at,omitempty"`
}

// RefType constants for ContextFeedback and RecordInjection.
const (
	RefTypeFact = "fact" // a memstore fact ID
	RefTypeTurn = "turn" // a session turn UUID
	RefTypeHint = "hint" // a context_hints row ID
)

// ContextFeedback is a rating from Claude on a piece of injected context.
// Score is +1 (useful) or -1 (not useful). One rating per ref per session.
type ContextFeedback struct {
	RefID     string // fact ID or session_turn UUID
	RefType   string // RefTypeFact, RefTypeTurn, or RefTypeHint
	SessionID string
	Score     int // +1 or -1
	Reason    string
}

// FeedbackStore is the minimal interface required to record context feedback.
// httpclient.Client implements this, allowing memstore-mcp to use memory_rate_context
// without a full SessionStore.
type FeedbackStore interface {
	RecordFeedback(ctx context.Context, fb ContextFeedback) error
}

// SessionStore persists Claude Code session data: turns, hints, injections, and feedback.
type SessionStore interface {
	// SaveTurns upserts session turns (idempotent on session_id+uuid).
	SaveTurns(ctx context.Context, sessionID string, turns []SessionTurn) error
	// SaveHook appends a raw Stop hook payload.
	SaveHook(ctx context.Context, payload []byte) error

	// StoreHint stores a context hint produced by the Ollama pipeline.
	StoreHint(ctx context.Context, hint ContextHint) (int64, error)
	// GetPendingHints returns unconsumed hints matching sessionID or cwd (OR semantics).
	// Either may be empty; at least one should be non-empty. Results are ordered by
	// relevance×desirability descending.
	GetPendingHints(ctx context.Context, sessionID, cwd string) ([]ContextHint, error)
	// MarkHintConsumed marks a hint as consumed so it is not re-injected.
	MarkHintConsumed(ctx context.Context, hintID int64) error

	// RecordInjection records that a ref was injected into a session (dedup log).
	// rank is the 0-based position of the item in the candidate list; -1 if unknown.
	RecordInjection(ctx context.Context, sessionID, refID, refType string, rank int) error
	// WasInjected returns true if refID+refType was already injected this session.
	WasInjected(ctx context.Context, sessionID, refID, refType string) (bool, error)

	// RecordFeedback stores Claude's rating of an injected context item.
	RecordFeedback(ctx context.Context, fb ContextFeedback) error
}
