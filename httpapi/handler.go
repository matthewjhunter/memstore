// Package httpapi provides an HTTP/JSON API handler that wraps a memstore.Store.
// It is used by the memstored daemon and can be mounted on any HTTP server.
//
// Architectural boundary: httpapi must not import pgstore or any other concrete
// store implementation. All storage is accessed through memstore interfaces
// (Store, SessionStore, Embedder, Generator). Composition happens in cmd/memstored.
package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/matthewjhunter/memstore"
)

// Handler serves the memstore HTTP API.
type Handler struct {
	store         memstore.Store
	embedder      memstore.Embedder
	generator     memstore.Generator
	sessionCtx    *SessionContext
	learnSessions *LearnSessionStore
	sessionStore  memstore.SessionStore
	extractQueue  *ExtractQueue
	apiKey        string // empty = auth disabled
	mux           *http.ServeMux
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

// WithLearnSessions enables cross-file learn session tracking.
func WithLearnSessions(ls *LearnSessionStore) HandlerOpt {
	return func(h *Handler) { h.learnSessions = ls }
}

// WithSessionStore enables persistence of Claude Code session events.
func WithSessionStore(ss memstore.SessionStore) HandlerOpt {
	return func(h *Handler) { h.sessionStore = ss }
}

// WithExtractQueue enables post-session fact extraction.
func WithExtractQueue(eq *ExtractQueue) HandlerOpt {
	return func(h *Handler) { h.extractQueue = eq }
}

// New creates an API handler backed by the given store.
// If apiKey is non-empty, requests must include Authorization: Bearer <key>.
func New(store memstore.Store, embedder memstore.Embedder, apiKey string, opts ...HandlerOpt) *Handler {
	h := &Handler{
		store:    store,
		embedder: embedder,
		apiKey:   apiKey,
		mux:      http.NewServeMux(),
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
	if h.apiKey != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != h.apiKey {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) registerRoutes() {
	h.mux.HandleFunc("GET /v1/health", h.handleHealth)

	h.mux.HandleFunc("POST /v1/facts", h.handleInsert)
	h.mux.HandleFunc("GET /v1/facts/{id}", h.handleGet)
	h.mux.HandleFunc("GET /v1/facts", h.handleList)
	h.mux.HandleFunc("DELETE /v1/facts/{id}", h.handleDelete)
	h.mux.HandleFunc("PATCH /v1/facts/{id}/metadata", h.handleUpdateMetadata)
	h.mux.HandleFunc("POST /v1/facts/{id}/supersede", h.handleSupersede)
	h.mux.HandleFunc("POST /v1/facts/{id}/confirm", h.handleConfirm)
	h.mux.HandleFunc("POST /v1/facts/touch", h.handleTouch)
	h.mux.HandleFunc("POST /v1/facts/exists", h.handleExists)
	h.mux.HandleFunc("GET /v1/facts/count", h.handleActiveCount)
	h.mux.HandleFunc("GET /v1/facts/{id}/history", h.handleHistoryByID)
	h.mux.HandleFunc("GET /v1/history/{subject}", h.handleHistoryBySubject)

	h.mux.HandleFunc("POST /v1/search", h.handleSearch)
	h.mux.HandleFunc("POST /v1/search/fts", h.handleSearchFTS)

	h.mux.HandleFunc("GET /v1/subsystems", h.handleListSubsystems)

	h.mux.HandleFunc("POST /v1/links", h.handleLinkFacts)
	h.mux.HandleFunc("GET /v1/links/{id}", h.handleGetLink)
	h.mux.HandleFunc("GET /v1/facts/{id}/links", h.handleGetLinks)
	h.mux.HandleFunc("PATCH /v1/links/{id}", h.handleUpdateLink)
	h.mux.HandleFunc("DELETE /v1/links/{id}", h.handleDeleteLink)

	h.mux.HandleFunc("POST /v1/generate", h.handleGenerate)
	h.mux.HandleFunc("POST /v1/generate/json", h.handleGenerateJSON)

	h.mux.HandleFunc("POST /v1/recall", h.handleRecall)
	h.mux.HandleFunc("POST /v1/context/touch", h.handleContextTouch)

	h.mux.HandleFunc("POST /v1/learn", h.handleLearn)
	h.mux.HandleFunc("POST /v1/learn/finalize", h.handleLearnFinalize)

	h.mux.HandleFunc("POST /v1/sessions/hook", h.handleSessionHook)
	h.mux.HandleFunc("POST /v1/sessions/transcript", h.handleSessionTranscript)

	h.mux.HandleFunc("POST /v1/context/hints", h.handleStoreHint)
	h.mux.HandleFunc("GET /v1/context/hints", h.handleGetHints)
	h.mux.HandleFunc("POST /v1/context/hints/{id}/consume", h.handleConsumeHint)
	h.mux.HandleFunc("POST /v1/context/injections", h.handleRecordInjection)
	h.mux.HandleFunc("POST /v1/context/feedback", h.handleRecordFeedback)
}

// --- Health ---

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	count, err := h.store.ActiveCount(r.Context())
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

	id, err := h.store.Insert(r.Context(), f)
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
	f, err := h.store.Get(r.Context(), id)
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

	facts, err := h.store.List(r.Context(), opts)
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
	if err := h.store.Delete(r.Context(), id); err != nil {
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
	if err := h.store.UpdateMetadata(r.Context(), id, patch); err != nil {
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
	if err := h.store.Supersede(r.Context(), oldID, input.NewID); err != nil {
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
	if err := h.store.Confirm(r.Context(), id); err != nil {
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
	if err := h.store.Touch(r.Context(), input.IDs); err != nil {
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
	exists, err := h.store.Exists(r.Context(), input.Content, input.Subject)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"exists": exists})
}

func (h *Handler) handleActiveCount(w http.ResponseWriter, r *http.Request) {
	count, err := h.store.ActiveCount(r.Context())
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
	entries, err := h.store.History(r.Context(), id, "")
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
	entries, err := h.store.History(r.Context(), 0, subject)
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
	results, err := h.store.Search(r.Context(), input.Query, input.opts())
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
	results, err := h.store.SearchFTS(r.Context(), input.Query, input.opts())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

type searchRequest struct {
	Query     string  `json:"query"`
	Subject   string  `json:"subject"`
	Category  string  `json:"category"`
	Kind      string  `json:"kind"`
	Subsystem string  `json:"subsystem"`
	Limit     int     `json:"limit"`
	FTSWeight float64 `json:"fts_weight"`
	VecWeight float64 `json:"vec_weight"`
}

func (s *searchRequest) opts() memstore.SearchOpts {
	o := memstore.SearchOpts{
		Subject:    s.Subject,
		Category:   s.Category,
		Kind:       s.Kind,
		Subsystem:  s.Subsystem,
		MaxResults: s.Limit,
		FTSWeight:  s.FTSWeight,
		VecWeight:  s.VecWeight,
	}
	return o
}

// --- Subsystems ---

func (h *Handler) handleListSubsystems(w http.ResponseWriter, r *http.Request) {
	subject := r.URL.Query().Get("subject")
	subs, err := h.store.ListSubsystems(r.Context(), subject)
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
	id, err := h.store.LinkFacts(r.Context(), input.SourceID, input.TargetID, input.LinkType, input.Bidirectional, input.Label, input.Metadata)
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
	link, err := h.store.GetLink(r.Context(), id)
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
	links, err := h.store.GetLinks(r.Context(), factID, dir, linkTypes...)
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
	if err := h.store.UpdateLink(r.Context(), id, input.Label, input.Metadata); err != nil {
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
	if err := h.store.DeleteLink(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Learn ---

func (h *Handler) handleLearn(w http.ResponseWriter, r *http.Request) {
	if h.generator == nil {
		writeError(w, http.StatusServiceUnavailable, "generator not configured (set --gen-model)")
		return
	}
	var input struct {
		Subject     string `json:"subject"`      // project name (e.g. "herald")
		FilePath    string `json:"file_path"`    // relative path (e.g. "internal/feeds/parser.go")
		Content     string `json:"content"`      // file source code
		ContentHash string `json:"content_hash"` // SHA256 for dedup; skip if unchanged
		ModulePath  string `json:"module_path"`  // Go module path (e.g. "github.com/matthewjhunter/herald")
		PackageName string `json:"package_name"` // Go package name
		Force       bool   `json:"force"`        // re-learn even if hash unchanged
		SessionID   string `json:"session_id"`   // optional; enables cross-file linking via finalize
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.Subject == "" || input.FilePath == "" || input.Content == "" {
		writeError(w, http.StatusBadRequest, "subject, file_path, and content are required")
		return
	}

	learner := memstore.NewCodebaseLearner(h.store, h.embedder, h.generator)
	result, err := learner.LearnFile(r.Context(), memstore.LearnFileOpts{
		Subject:     input.Subject,
		FilePath:    input.FilePath,
		Content:     input.Content,
		ContentHash: input.ContentHash,
		ModulePath:  input.ModulePath,
		PackageName: input.PackageName,
		Force:       input.Force,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Record learned facts in the session for cross-file linking.
	if h.learnSessions != nil && input.SessionID != "" && !result.Skipped {
		var refs []learnedFactRef
		surface := "file"
		ext := strings.ToLower(filepath.Ext(input.FilePath))
		if ext == ".md" || ext == ".markdown" {
			surface = "doc"
		}
		if result.FileFactID != 0 {
			refs = append(refs, learnedFactRef{
				FactID:  result.FileFactID,
				Surface: surface,
				RelPath: input.FilePath,
				Subject: input.Subject,
			})
		}
		for _, symID := range result.SymbolIDs {
			refs = append(refs, learnedFactRef{
				FactID:  symID,
				Surface: "symbol",
				RelPath: input.FilePath,
				Subject: input.Subject,
			})
		}
		h.learnSessions.Record(input.SessionID, input.Subject, refs)
	}

	writeJSON(w, http.StatusOK, result)
}

// --- Helpers ---

func readJSON(r *http.Request, w http.ResponseWriter, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
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
