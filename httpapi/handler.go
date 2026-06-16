// Package httpapi provides an HTTP/JSON API handler that wraps a memstore.Store.
// It is used by the memstored daemon and can be mounted on any HTTP server.
//
// Architectural boundary: httpapi must not import pgstore or any other concrete
// store implementation. All storage is accessed through memstore interfaces
// (Store, SessionStore, Embedder, Generator). Composition happens in cmd/memstored.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infodancer/smoke"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// TokenVerifier resolves a presented bearer token to an Identity. It is the
// integration seam between httpapi and any concrete token store (currently
// pgstore.TokenStore). Returning a non-nil error means "401, do not auth";
// callers must not leak the underlying reason in the response body.
type TokenVerifier interface {
	VerifyToken(ctx context.Context, token string) (Identity, error)
}

// scopedStoreKey and scopedSessionKey are unexported context keys that carry the
// per-request scoped store and session store resolved in ServeHTTP. Using
// distinct named types prevents collisions with other context values.
type scopedStoreKey struct{}
type scopedSessionKey struct{}

// storeFromCtx returns the per-request scoped store set by ServeHTTP, or the
// handler's base store when the key is absent (e.g. in tests that bypass
// ServeHTTP).
func storeFromCtx(ctx context.Context, base memstore.Store) memstore.Store {
	if s, ok := ctx.Value(scopedStoreKey{}).(memstore.Store); ok {
		return s
	}
	return base
}

// sessionFromCtx returns the per-request scoped session store set by ServeHTTP,
// or the handler's base session store when the key is absent.
func sessionFromCtx(ctx context.Context, base memstore.SessionStore) memstore.SessionStore {
	if s, ok := ctx.Value(scopedSessionKey{}).(memstore.SessionStore); ok {
		return s
	}
	return base
}

// Handler serves the memstore HTTP API.
type Handler struct {
	store        memstore.Store
	embedder     embedding.Embedder
	generator    memstore.Generator
	sessionCtx   *SessionContext
	sessionStore memstore.SessionStore
	extractQueue *ExtractQueue
	apiKey       string        // legacy single-key fallback (empty = no legacy check)
	tokens       TokenVerifier // multi-token path (nil = no token store wired up)
	mux          *smoke.Mux    // records a route spec per registration for smoke coverage

	reranker        embedding.Reranker // nil = recall stays first-stage only
	rerankMode      memstore.RerankMode
	rerankThreshold float64
	rerankPoolSize  int // search candidate pool cap; 0 = built-in default
	recallPoolSize  int // recall candidate pool cap; 0 = built-in default
	rerankDocBytes  int // search per-doc truncation budget; 0 = built-in default
	recallDocBytes  int // recall per-doc truncation budget; 0 = built-in default

	maxBodyBytes int64 // cap applied to every request body; default 64 MB
}

// HandlerOpt configures optional Handler fields.
type HandlerOpt func(*Handler)

// WithGenerator sets the LLM generator for the /v1/generate endpoints.
func WithGenerator(g memstore.Generator) HandlerOpt {
	return func(h *Handler) { h.generator = g }
}

// WithSessionContext sets the session context tracker for the /v1/recall endpoint.
func WithSessionContext(sc *SessionContext) HandlerOpt {
	return func(h *Handler) { h.sessionCtx = sc }
}

// WithSessionStore enables persistence of Claude Code session events.
func WithSessionStore(ss memstore.SessionStore) HandlerOpt {
	return func(h *Handler) { h.sessionStore = ss }
}

// WithExtractQueue enables post-session fact extraction.
func WithExtractQueue(eq *ExtractQueue) HandlerOpt {
	return func(h *Handler) { h.extractQueue = eq }
}

// WithReranker enables second-stage reranking, using the given reranker under
// the supplied policy. Mode/Threshold drive the /v1/recall pipeline; Candidates
// caps the per-request search default pool and RecallCandidates caps the recall
// pool (separate because recall runs per-prompt under a tight budget). A
// disabled mode (RerankOff) leaves recall first-stage only even if a reranker is
// passed. The reranker should be the same instance set on the store.
func WithReranker(rr embedding.Reranker, pol memstore.RerankPolicy) HandlerOpt {
	return func(h *Handler) {
		h.reranker = rr
		h.rerankMode = pol.Mode
		h.rerankThreshold = pol.Threshold
		h.rerankPoolSize = pol.Candidates
		h.recallPoolSize = pol.RecallCandidates
		h.rerankDocBytes = pol.DocBytes
		h.recallDocBytes = pol.RecallDocBytes
	}
}

// WithTokenVerifier enables bearer-token auth backed by the given verifier
// (typically a pgstore.TokenStore). When set, requests must carry a valid
// token; the legacy single-key check is bypassed.
func WithTokenVerifier(v TokenVerifier) HandlerOpt {
	return func(h *Handler) { h.tokens = v }
}

// WithMaxBodyBytes caps the request body size accepted by any endpoint.
func WithMaxBodyBytes(n int64) HandlerOpt {
	return func(h *Handler) { h.maxBodyBytes = n }
}

// New creates an API handler backed by the given store.
// If apiKey is non-empty, requests must include Authorization: Bearer <key>.
func New(store memstore.Store, embedder embedding.Embedder, apiKey string, opts ...HandlerOpt) *Handler {
	h := &Handler{
		store:        store,
		embedder:     embedder,
		apiKey:       apiKey,
		mux:          smoke.NewMux(),
		maxBodyBytes: 64 << 20,
	}
	for _, opt := range opts {
		opt(h)
	}
	h.registerRoutes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health endpoint is unauthenticated for monitoring.
	if r.URL.Path == "/v1/health" {
		h.mux.ServeHTTP(w, r)
		return
	}

	// Auth dispatch: token verifier wins over legacy single key, both opt-in.
	switch {
	case h.tokens != nil:
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		id, err := h.tokens.VerifyToken(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		r = r.WithContext(WithIdentity(r.Context(), id))

	case h.apiKey != "":
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		// Constant-time compare to avoid leaking the configured key via timing.
		// HasPrefix is a fast structural check, not a secret-dependent branch.
		if !strings.HasPrefix(auth, "Bearer ") ||
			subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		r = r.WithContext(WithIdentity(r.Context(), Identity{Name: "legacy", Source: "legacy"}))
	}

	// Resolve per-request scoped store and session store at the auth boundary,
	// once, before dispatch. When the backend implements UserScoper and the
	// identity carries a non-zero UserID, every handler sees a store whose reads
	// and writes are locked to that user. A ForUser failure (user not found in
	// DB) is a 500 -- it should not happen for a valid token, and falling back to
	// another user's data is worse than an error.
	//
	// When the backend is not a UserScoper (e.g. SQLite in tests), or UserID is 0
	// (legacy single-key path), both keys are set to the handler's base stores so
	// storeFromCtx / sessionFromCtx are always total.
	id, _ := IdentityFromContext(r.Context())
	scopedStore := h.store
	if us, ok := h.store.(memstore.UserScoper); ok && id.UserID != 0 {
		s, err := us.ForUser(id.UserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store scoping failed")
			return
		}
		scopedStore = s
	}
	scopedSess := h.sessionStore
	if us, ok := h.sessionStore.(memstore.SessionUserScoper); ok && id.UserID != 0 {
		s, err := us.ForUser(id.UserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session store scoping failed")
			return
		}
		scopedSess = s
	}
	ctx := context.WithValue(r.Context(), scopedStoreKey{}, scopedStore)
	ctx = context.WithValue(ctx, scopedSessionKey{}, scopedSess)
	r = r.WithContext(ctx)

	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	}
	h.mux.ServeHTTP(w, r)
}

// registerRoutes wires every API route into the mux and records a smoke
// RouteSpec for each. The specs drive two checks:
//
//   - a coverage gate (TestSmokeManifestCoverage) that fails when a route is
//     added without enough metadata to probe it (a path param with no example,
//     and no skip); and
//   - an in-process probe (TestSmokeProbeReadRoutes) that exercises every
//     read route against a live handler.
//
// Conventions: GET reads carry Example values for their path params (the probe
// seeds matching fixtures with id 1). State-changing routes are marked Write()
// -- recorded as mutating and skipped, so reads-only probes never fire them and
// the gate treats them as intentional. POST-shaped reads that need a JSON body
// (search, exists, recall, generate) are Skipped with a reason until phase-2
// body probes exist. GET /v1/context/hints needs a query param the path-based
// prober can't supply, so it is skipped too.
func (h *Handler) registerRoutes() {
	h.mux.HandleFunc("GET /v1/health", h.handleHealth)

	h.mux.HandleFunc("POST /v1/facts", h.handleInsert, smoke.Write())
	h.mux.HandleFunc("GET /v1/facts/{id}", h.handleGet, smoke.Example("id", "1"))
	h.mux.HandleFunc("GET /v1/facts", h.handleList)
	h.mux.HandleFunc("DELETE /v1/facts/{id}", h.handleDelete, smoke.Write())
	h.mux.HandleFunc("PATCH /v1/facts/{id}/metadata", h.handleUpdateMetadata, smoke.Write())
	h.mux.HandleFunc("POST /v1/facts/{id}/supersede", h.handleSupersede, smoke.Write())
	h.mux.HandleFunc("POST /v1/facts/{id}/confirm", h.handleConfirm, smoke.Write())
	h.mux.HandleFunc("POST /v1/facts/touch", h.handleTouch, smoke.Write())
	h.mux.HandleFunc("POST /v1/facts/exists", h.handleExists, smoke.Skip("POST read; needs a JSON body (phase 2)"))
	h.mux.HandleFunc("GET /v1/facts/count", h.handleActiveCount)
	h.mux.HandleFunc("GET /v1/facts/{id}/history", h.handleHistoryByID, smoke.Example("id", "1"))
	h.mux.HandleFunc("GET /v1/history/{subject}", h.handleHistoryBySubject, smoke.Example("subject", "smoke"))

	h.mux.HandleFunc("POST /v1/search", h.handleSearch, smoke.Skip("POST read; needs a JSON body (phase 2)"))
	h.mux.HandleFunc("POST /v1/search/fts", h.handleSearchFTS, smoke.Skip("POST read; needs a JSON body (phase 2)"))

	h.mux.HandleFunc("GET /v1/subsystems", h.handleListSubsystems)

	h.mux.HandleFunc("POST /v1/links", h.handleLinkFacts, smoke.Write())
	h.mux.HandleFunc("GET /v1/links/{id}", h.handleGetLink, smoke.Example("id", "1"))
	h.mux.HandleFunc("GET /v1/facts/{id}/links", h.handleGetLinks, smoke.Example("id", "1"))
	h.mux.HandleFunc("PATCH /v1/links/{id}", h.handleUpdateLink, smoke.Write())
	h.mux.HandleFunc("DELETE /v1/links/{id}", h.handleDeleteLink, smoke.Write())

	h.mux.HandleFunc("POST /v1/generate", h.handleGenerate, smoke.Skip("calls the LLM; needs a JSON body (phase 2)"))
	h.mux.HandleFunc("POST /v1/generate/json", h.handleGenerateJSON, smoke.Skip("calls the LLM; needs a JSON body (phase 2)"))

	h.mux.HandleFunc("POST /v1/recall", h.handleRecall, smoke.Skip("POST read; needs a JSON body (phase 2)"))
	h.mux.HandleFunc("POST /v1/context/touch", h.handleContextTouch, smoke.Write())

	h.mux.HandleFunc("POST /v1/sessions/hook", h.handleSessionHook, smoke.Write())
	h.mux.HandleFunc("POST /v1/sessions/transcript", h.handleSessionTranscript, smoke.Write())

	h.mux.HandleFunc("POST /v1/context/hints", h.handleStoreHint, smoke.Write())
	h.mux.HandleFunc("GET /v1/context/hints", h.handleGetHints, smoke.Skip("needs a session_id or cwd query param; not path-probeable"))
	h.mux.HandleFunc("POST /v1/context/hints/{id}/consume", h.handleConsumeHint, smoke.Write())
	h.mux.HandleFunc("POST /v1/context/injections", h.handleRecordInjection, smoke.Write())
	h.mux.HandleFunc("POST /v1/context/feedback", h.handleRecordFeedback, smoke.Write())
	h.mux.HandleFunc("POST /v1/context/backfill-feedback", h.handleBackfillFeedback, smoke.Write())
}

// Manifest returns the smoke route manifest recorded at registration time:
// every route the handler serves, with its probe metadata and completeness.
// It feeds the coverage gate and the in-process probe.
func (h *Handler) Manifest() smoke.Manifest {
	return h.mux.Registry().Manifest()
}

// --- Health ---

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	count, err := storeFromCtx(r.Context(), h.store).ActiveCount(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unhealthy",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "healthy",
		"facts":  count,
	})
}

// --- Fact CRUD ---

func (h *Handler) handleInsert(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content   string         `json:"content"`
		Subject   string         `json:"subject"`
		Category  string         `json:"category"`
		Kind      string         `json:"kind"`
		Subsystem string         `json:"subsystem"`
		Metadata  map[string]any `json:"metadata"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.Content == "" || input.Subject == "" {
		writeError(w, http.StatusBadRequest, "content and subject are required")
		return
	}

	f := memstore.Fact{
		Content:   input.Content,
		Subject:   input.Subject,
		Category:  input.Category,
		Kind:      input.Kind,
		Subsystem: input.Subsystem,
	}
	if input.Metadata != nil {
		raw, _ := json.Marshal(input.Metadata)
		f.Metadata = raw
	}

	id, err := storeFromCtx(r.Context(), h.store).Insert(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	f, err := storeFromCtx(r.Context(), h.store).Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if f == nil {
		writeError(w, http.StatusNotFound, "fact not found")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := memstore.QueryOpts{
		Subject:    q.Get("subject"),
		Category:   q.Get("category"),
		Kind:       q.Get("kind"),
		Subsystem:  q.Get("subsystem"),
		OnlyActive: q.Get("active") != "false",
	}
	if v := q.Get("limit"); v != "" {
		n, _ := strconv.Atoi(v)
		opts.Limit = n
	}
	if v := q.Get("metadata_filters"); v != "" {
		if err := json.Unmarshal([]byte(v), &opts.MetadataFilters); err != nil {
			writeError(w, http.StatusBadRequest, "invalid metadata_filters: "+err.Error())
			return
		}
	}
	if v := q.Get("ids"); v != "" {
		for part := range strings.SplitSeq(v, ",") {
			n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid ids: "+err.Error())
				return
			}
			opts.IDs = append(opts.IDs, n)
		}
	}
	if v := q.Get("created_after"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid created_after: "+err.Error())
			return
		}
		opts.CreatedAfter = &t
	}
	if v := q.Get("created_before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid created_before: "+err.Error())
			return
		}
		opts.CreatedBefore = &t
	}

	facts, err := storeFromCtx(r.Context(), h.store).List(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, facts)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	if err := storeFromCtx(r.Context(), h.store).Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) handleUpdateMetadata(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	var patch map[string]any
	if !readJSON(r, w, &patch) {
		return
	}
	if err := storeFromCtx(r.Context(), h.store).UpdateMetadata(r.Context(), id, patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *Handler) handleSupersede(w http.ResponseWriter, r *http.Request) {
	oldID, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	var input struct {
		NewID int64 `json:"new_id"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.NewID == 0 {
		writeError(w, http.StatusBadRequest, "new_id is required")
		return
	}
	if err := storeFromCtx(r.Context(), h.store).Supersede(r.Context(), oldID, input.NewID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "superseded"})
}

func (h *Handler) handleConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	if err := storeFromCtx(r.Context(), h.store).Confirm(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (h *Handler) handleTouch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if err := storeFromCtx(r.Context(), h.store).Touch(r.Context(), input.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "touched"})
}

func (h *Handler) handleExists(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content string `json:"content"`
		Subject string `json:"subject"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	exists, err := storeFromCtx(r.Context(), h.store).Exists(r.Context(), input.Content, input.Subject)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"exists": exists})
}

func (h *Handler) handleActiveCount(w http.ResponseWriter, r *http.Request) {
	count, err := storeFromCtx(r.Context(), h.store).ActiveCount(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": count})
}

// --- History ---

func (h *Handler) handleHistoryByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	entries, err := storeFromCtx(r.Context(), h.store).History(r.Context(), id, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *Handler) handleHistoryBySubject(w http.ResponseWriter, r *http.Request) {
	subject := r.PathValue("subject")
	if subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}
	entries, err := storeFromCtx(r.Context(), h.store).History(r.Context(), 0, subject)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// --- Search ---

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	var input searchRequest
	if !readJSON(r, w, &input) {
		return
	}
	if input.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	opts := input.opts()
	// Apply the daemon's configured candidate pool when the request didn't pick
	// one, so RERANK_CANDIDATES governs search without every client sending it.
	// A request that sets rerank_candidates still wins.
	if opts.RerankCandidates <= 0 && h.rerankPoolSize > 0 {
		opts.RerankCandidates = h.rerankPoolSize
	}
	if opts.RerankDocBytes <= 0 && h.rerankDocBytes > 0 {
		opts.RerankDocBytes = h.rerankDocBytes
	}
	results, err := storeFromCtx(r.Context(), h.store).Search(r.Context(), input.Query, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (h *Handler) handleSearchFTS(w http.ResponseWriter, r *http.Request) {
	var input searchRequest
	if !readJSON(r, w, &input) {
		return
	}
	if input.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	results, err := storeFromCtx(r.Context(), h.store).SearchFTS(r.Context(), input.Query, input.opts())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

type searchRequest struct {
	Query            string                    `json:"query"`
	AllNamespaces    bool                      `json:"all_namespaces"`
	Subject          string                    `json:"subject"`
	Category         string                    `json:"category"`
	Kind             string                    `json:"kind"`
	Subsystem        string                    `json:"subsystem"`
	Limit            int                       `json:"limit"`
	FTSWeight        float64                   `json:"fts_weight"`
	VecWeight        float64                   `json:"vec_weight"`
	RerankMode       string                    `json:"rerank_mode"`
	RerankThreshold  float64                   `json:"rerank_threshold"`
	RerankCandidates int                       `json:"rerank_candidates"`
	RerankWeight     float64                   `json:"rerank_weight"`
	RerankDocBytes   int                       `json:"rerank_doc_bytes"`
	OnlyActive       bool                      `json:"only_active"`
	MetadataFilters  []memstore.MetadataFilter `json:"metadata_filters"`
	CreatedAfter     string                    `json:"created_after"`
	CreatedBefore    string                    `json:"created_before"`
}

func (s *searchRequest) opts() memstore.SearchOpts {
	o := memstore.SearchOpts{
		AllNamespaces:    s.AllNamespaces,
		Subject:          s.Subject,
		Category:         s.Category,
		Kind:             s.Kind,
		Subsystem:        s.Subsystem,
		MaxResults:       s.Limit,
		FTSWeight:        s.FTSWeight,
		VecWeight:        s.VecWeight,
		RerankThreshold:  s.RerankThreshold,
		RerankCandidates: s.RerankCandidates,
		RerankWeight:     s.RerankWeight,
		RerankDocBytes:   s.RerankDocBytes,
		OnlyActive:       s.OnlyActive,
		MetadataFilters:  s.MetadataFilters,
	}
	// Lenient: an unrecognized mode disables rerank rather than failing search.
	o.RerankMode, _ = memstore.ParseRerankMode(s.RerankMode)
	if s.CreatedAfter != "" {
		if t, err := time.Parse(time.RFC3339, s.CreatedAfter); err == nil {
			o.CreatedAfter = &t
		}
	}
	if s.CreatedBefore != "" {
		if t, err := time.Parse(time.RFC3339, s.CreatedBefore); err == nil {
			o.CreatedBefore = &t
		}
	}
	return o
}

// --- Subsystems ---

func (h *Handler) handleListSubsystems(w http.ResponseWriter, r *http.Request) {
	subject := r.URL.Query().Get("subject")
	subs, err := storeFromCtx(r.Context(), h.store).ListSubsystems(r.Context(), subject)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, subs)
}

// --- Links ---

func (h *Handler) handleLinkFacts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SourceID      int64          `json:"source_id"`
		TargetID      int64          `json:"target_id"`
		LinkType      string         `json:"link_type"`
		Bidirectional bool           `json:"bidirectional"`
		Label         string         `json:"label"`
		Metadata      map[string]any `json:"metadata"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.SourceID == 0 || input.TargetID == 0 {
		writeError(w, http.StatusBadRequest, "source_id and target_id are required")
		return
	}
	id, err := storeFromCtx(r.Context(), h.store).LinkFacts(r.Context(), input.SourceID, input.TargetID, input.LinkType, input.Bidirectional, input.Label, input.Metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (h *Handler) handleGetLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	link, err := storeFromCtx(r.Context(), h.store).GetLink(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if link == nil {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (h *Handler) handleGetLinks(w http.ResponseWriter, r *http.Request) {
	factID, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	q := r.URL.Query()
	dir := memstore.LinkBoth
	switch q.Get("direction") {
	case "outbound":
		dir = memstore.LinkOutbound
	case "inbound":
		dir = memstore.LinkInbound
	}
	var linkTypes []string
	if v := q.Get("types"); v != "" {
		linkTypes = strings.Split(v, ",")
	}
	links, err := storeFromCtx(r.Context(), h.store).GetLinks(r.Context(), factID, dir, linkTypes...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, links)
}

func (h *Handler) handleUpdateLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	var input struct {
		Label    string         `json:"label"`
		Metadata map[string]any `json:"metadata"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if err := storeFromCtx(r.Context(), h.store).UpdateLink(r.Context(), id, input.Label, input.Metadata); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *Handler) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, w, "id")
	if !ok {
		return
	}
	if err := storeFromCtx(r.Context(), h.store).DeleteLink(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Helpers ---

func readJSON(r *http.Request, w http.ResponseWriter, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (h *Handler) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if h.generator == nil {
		writeError(w, http.StatusServiceUnavailable, "generator not configured")
		return
	}
	var input struct {
		Prompt string `json:"prompt"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	text, err := h.generator.Generate(r.Context(), input.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "model": h.generator.Model()})
}

func (h *Handler) handleGenerateJSON(w http.ResponseWriter, r *http.Request) {
	if h.generator == nil {
		writeError(w, http.StatusServiceUnavailable, "generator not configured")
		return
	}
	jsonGen, ok := h.generator.(memstore.JSONGenerator)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "generator does not support JSON mode")
		return
	}
	var input struct {
		Prompt string `json:"prompt"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	text, err := jsonGen.GenerateJSON(r.Context(), input.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "model": h.generator.Model()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathInt64(r *http.Request, w http.ResponseWriter, name string) (int64, bool) {
	s := r.PathValue(name)
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name+": "+s)
		return 0, false
	}
	return id, true
}
