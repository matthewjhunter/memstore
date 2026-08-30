package memstore

import "context"

// Capability-typed store handles.
//
// The Store interface is the union of everything a backend can do, which makes
// it the wrong thing to hand a request handler: a handler holding a Store can
// write whether or not its caller was entitled to, and the only thing standing
// between the two is a check somebody remembered to write. These interfaces
// split that union by what the caller is allowed to do, so the entitlement is
// carried by the handle's type rather than asserted alongside it. A handler
// given a ReadableStore cannot insert a fact -- not "must not", cannot: the
// method is not in its type.
//
// The identity is bound into the handle too. UserScoper.ForUser already returns
// a store whose every read and write is scoped to one user, and httpapi builds
// one per request; these narrow what that call hands back rather than
// introducing a second mechanism.
//
// What this does and does not guarantee. It guarantees that no handler can
// obtain more authority than it was given, because the only way to widen a
// handle is to ask the scoper for a wider one and the scoper refuses. It does
// not guarantee the process cannot write -- daemon startup legitimately
// constructs a full store, and must. The property is that authority does not
// leak sideways into request handling, which is where it would actually be
// abused.

// Principal names the caller a store handle is scoped to.
//
// Write carries the answer to "may this caller mutate content", not the rule
// that produced it. The scope policy lives with the transport that
// authenticated the caller (httpapi.Identity.Allows, where admin implies write,
// an empty scope set means the legacy pre-enforcement token, and ingest is
// implied by nothing). Duplicating that policy here would be a second
// implementation to keep in step; carrying its answer is not.
//
// Expansion point: memstore has users and no organisations -- tier 3 shipped
// users only, and the group/role columns it once reserved were deliberately not
// built rather than guess at a sharing model. When organisations exist this
// struct grows a field and no call site changes, which is the reason the
// scopers take a Principal instead of a bare user id.
type Principal struct {
	// UserID is the owning user. Zero means the unscoped legacy path, which
	// the scopers treat as "no narrowing available".
	UserID int64

	// Write reports whether this caller may mutate stored content.
	Write bool
}

// ReadableStore is retrieval, plus the telemetry that records it.
//
// Touch is here rather than on WritableStore, which looks wrong until you ask
// who needs it: it bumps use_count for facts a caller was actually served, and
// a retrieval-only session has to be able to record that or its reads are
// invisible to every metric built on use_count -- including the prune predicate
// that decides what to forget. It writes a counter about a read, never content,
// so granting it costs a read-only caller nothing it did not already have.
//
// It stayed a method rather than moving inside Search for a concrete reason:
// Search is also called by the recall pipeline (httpapi/recall.go) and by
// extraction's dedup query, neither of which is a use. Bumping the counter
// inside Search would silently count both, conflating recall injections with
// tool-call usage -- exactly the separation #158 was written to establish.
type ReadableStore interface {
	Get(ctx context.Context, id int64) (*Fact, error)
	List(ctx context.Context, opts QueryOpts) ([]Fact, error)
	BySubject(ctx context.Context, subject string, onlyActive bool) ([]Fact, error)
	Exists(ctx context.Context, content, subject string) (bool, error)
	ActiveCount(ctx context.Context) (int64, error)
	Breakdown(ctx context.Context) (FactBreakdown, error)
	History(ctx context.Context, id int64, subject string) ([]HistoryEntry, error)
	Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)
	SearchBatch(ctx context.Context, queries []string, opts SearchOpts) ([][]SearchResult, error)
	SearchFTS(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)
	ListSubsystems(ctx context.Context, subject string) ([]string, error)
	GetLink(ctx context.Context, linkID int64) (*Link, error)
	GetLinks(ctx context.Context, factID int64, direction LinkDirection, linkTypes ...string) ([]Link, error)

	// Touch bumps use_count for facts the caller was served. See the type
	// comment for why read telemetry lives on the read handle.
	Touch(ctx context.Context, ids []int64) error
}

// WritableStore adds the content mutations to ReadableStore. Everything on it
// changes what memstore knows; nothing on it is maintenance.
type WritableStore interface {
	ReadableStore

	Insert(ctx context.Context, f Fact) (int64, error)
	InsertBatch(ctx context.Context, facts []Fact) error
	Supersede(ctx context.Context, oldID, newID int64) error
	Confirm(ctx context.Context, id int64) error
	Delete(ctx context.Context, id int64) error
	UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error
	LinkFacts(ctx context.Context, sourceID, targetID int64, linkType string, bidirectional bool, label string, metadata map[string]any) (int64, error)
	UpdateLink(ctx context.Context, linkID int64, label string, metadata map[string]any) error
	DeleteLink(ctx context.Context, linkID int64) error
}

// EmbedStore is the embedding pipeline's write surface, deliberately separate
// from WritableStore.
//
// These are writes, but not writes about what memstore knows: they replace
// vectors, quarantine facts whose embedding failed, and drain the embed queue.
// Folding them into WritableStore would mean anything that can store a fact can
// also rewrite another fact's vectors or mark it permanently unembeddable --
// authority the MCP surface has never needed and should not be handed by
// default. The embed worker holds this; request handlers do not.
type EmbedStore interface {
	NeedingEmbedding(ctx context.Context, limit int) ([]Fact, error)
	SetEmbedding(ctx context.Context, id int64, emb []float32) error
	SetFactVectors(ctx context.Context, id int64, v FactVectors) error
	FactChunks(ctx context.Context, id int64) ([]FactChunk, error)
	MarkEmbedFailed(ctx context.Context, id int64, reason string) error
	EmbedFacts(ctx context.Context, batchSize int) (int, error)
}

// StoreScoper mints capability-typed handles for a principal. It is the single
// place a writable handle can come from, which is what makes the split worth
// having: one admission decision, in one function, instead of an authorization
// check at every call site that might mutate something.
//
// WritableFor returns ErrNotPermitted when the principal may not write. That is
// a refusal to issue authority, not a failed operation -- the caller never gets
// far enough to attempt one.
type StoreScoper interface {
	ReadableFor(p Principal) (ReadableStore, error)
	WritableFor(p Principal) (WritableStore, error)
}

// ReadOnly narrows a handle so the narrowing survives a type assertion.
//
// Without it the split is advisory: every backend satisfies WritableStore, so a
// handler holding a ReadableStore backed by a concrete store could assert its
// way back up to the full interface and write. Wrapping promotes only
// ReadableStore's method set, so that assertion fails -- the handle does not
// merely decline to expose the writes, it does not have them.
//
// Scopers return wrapped handles. Callers should not unwrap; there is no
// exported way to, which is the point.
func ReadOnly(r ReadableStore) ReadableStore { return readOnly{r} }

type readOnly struct{ ReadableStore }

// DocumentSearcher is the read-only half of DocumentStore: FTS over chunk
// content, with none of the methods that create or remove documents.
//
// It exists because the corpus has exactly one model-facing capability --
// search -- and that capability has to survive the narrowing ReadOnly
// performs, while everything else in DocumentStore must not.
type DocumentSearcher interface {
	SearchDocumentChunks(ctx context.Context, query string, opts DocumentSearchOpts) ([]DocumentSearchResult, error)
}

// DocumentSearcherOf returns a store's document-search capability, or false
// when the backend carries no corpus.
//
// It looks through ReadOnly, which is the whole reason it exists. ReadOnly
// promotes only ReadableStore's method set precisely so a read handle cannot
// assert its way back to the writes -- correct, and it takes document search
// down with it, leaving a read server unable to reach the corpus at all. That
// is most of production.
//
// This is not the unwrap ReadOnly's comment warns against. The result is
// wrapped the same way and for the same reason: returning the concrete store
// typed as DocumentSearcher would leave the full DocumentStore one assertion
// away, handing back the writes ReadOnly had just removed. Wrapping promotes
// only the search method, so the reader gets the one capability it is
// entitled to and no route to the rest.
func DocumentSearcherOf(s any) (DocumentSearcher, bool) {
	if ro, ok := s.(readOnly); ok {
		s = ro.ReadableStore
	}
	ds, ok := s.(DocumentSearcher)
	if !ok {
		return nil, false
	}
	return searchOnly{ds}, true
}

type searchOnly struct{ DocumentSearcher }

// AdminStore is a handle that may act for principals other than its own. It is
// the typed replacement for today's service scope, which is a store carrying
// user id 0 and a convention about what that means -- a magic value that has
// already produced one real bug, where the service-scope branch of LinkFacts
// stamped user_id 0 onto a row whose foreign key required a real user.
//
// It embeds StoreScoper rather than adding cross-user methods because that is
// precisely the extra authority: an admin does not read other users' facts
// directly, it mints a handle for the user it is acting on behalf of, and every
// row that handle writes names that user.
type AdminStore interface {
	WritableStore
	StoreScoper
}
