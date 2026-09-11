package memstore

import (
	"context"
	"time"
)

// FactCitation is one [fact N] citation the model wrote in a session: evidence
// that a memory shaped an answer (docs/citation-feedback.md). One per session
// and fact -- the first time it was cited.
type FactCitation struct {
	SessionID string
	FactID    int64
	TurnUUID  string    // the assistant turn it was first cited in
	CitedAt   time.Time // that turn's time; zero means "now" when recorded
}

// CitationRecorder is implemented by a session store that keeps citations.
// RecordCitations is idempotent per session and fact, and returns how many of
// the citations were new. The transcript handler records through it when the
// store offers it; a store without it reads transcripts exactly as before.
type CitationRecorder interface {
	RecordCitations(ctx context.Context, cites []FactCitation) (int, error)
}

// CitationSource is implemented by a session store that can serve recorded
// session history to the citation backfill: the sessions whose assistant turns
// contain the citation form, and their turns.
type CitationSource interface {
	CitingSessions(ctx context.Context) ([]string, error)
	GetSessionTurns(ctx context.Context, sessionID string) ([]SessionTurn, error)
}
