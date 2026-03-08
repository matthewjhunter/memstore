// Package httpclient provides a memstore.Store implementation that talks to
// a memstored daemon over HTTP. It is the daemon-mode client for memstore-mcp
// and the CLI.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
)

// Client implements memstore.Store by calling the memstored HTTP API.
type Client struct {
	base   string // e.g. "http://cube:8230"
	apiKey string
	http   *http.Client
}

// New creates a client pointing at the given memstored base URL.
// If apiKey is non-empty, it is sent as Bearer token on every request.
func New(baseURL, apiKey string) *Client {
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		apiKey: apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Insert(ctx context.Context, f memstore.Fact) (int64, error) {
	body := map[string]any{
		"content":   f.Content,
		"subject":   f.Subject,
		"category":  f.Category,
		"kind":      f.Kind,
		"subsystem": f.Subsystem,
	}
	if f.Metadata != nil {
		var m map[string]any
		json.Unmarshal(f.Metadata, &m)
		body["metadata"] = m
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := c.post(ctx, "/v1/facts", body, &result); err != nil {
		return 0, err
	}
	return result.ID, nil
}

func (c *Client) InsertBatch(ctx context.Context, facts []memstore.Fact) error {
	// Insert one at a time — the daemon doesn't have a batch endpoint yet.
	for _, f := range facts {
		if _, err := c.Insert(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Get(ctx context.Context, id int64) (*memstore.Fact, error) {
	var f memstore.Fact
	err := c.get(ctx, fmt.Sprintf("/v1/facts/%d", id), &f)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &f, nil
}

func (c *Client) List(ctx context.Context, opts memstore.QueryOpts) ([]memstore.Fact, error) {
	q := url.Values{}
	if opts.Subject != "" {
		q.Set("subject", opts.Subject)
	}
	if opts.Category != "" {
		q.Set("category", opts.Category)
	}
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	if opts.Subsystem != "" {
		q.Set("subsystem", opts.Subsystem)
	}
	if !opts.OnlyActive {
		q.Set("active", "false")
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	var facts []memstore.Fact
	if err := c.get(ctx, "/v1/facts?"+q.Encode(), &facts); err != nil {
		return nil, err
	}
	return facts, nil
}

func (c *Client) Delete(ctx context.Context, id int64) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("/v1/facts/%d", id), nil, nil)
}

func (c *Client) UpdateMetadata(ctx context.Context, id int64, patch map[string]any) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("/v1/facts/%d/metadata", id), patch, nil)
}

func (c *Client) Supersede(ctx context.Context, oldID, newID int64) error {
	return c.post(ctx, fmt.Sprintf("/v1/facts/%d/supersede", oldID), map[string]any{"new_id": newID}, nil)
}

func (c *Client) Confirm(ctx context.Context, id int64) error {
	return c.post(ctx, fmt.Sprintf("/v1/facts/%d/confirm", id), nil, nil)
}

func (c *Client) Touch(ctx context.Context, ids []int64) error {
	return c.post(ctx, "/v1/facts/touch", map[string]any{"ids": ids}, nil)
}

func (c *Client) Exists(ctx context.Context, content, subject string) (bool, error) {
	var result struct {
		Exists bool `json:"exists"`
	}
	err := c.post(ctx, "/v1/facts/exists", map[string]any{"content": content, "subject": subject}, &result)
	if err != nil {
		return false, err
	}
	return result.Exists, nil
}

func (c *Client) ActiveCount(ctx context.Context) (int64, error) {
	var result struct {
		Count int64 `json:"count"`
	}
	if err := c.get(ctx, "/v1/facts/count", &result); err != nil {
		return 0, err
	}
	return result.Count, nil
}

func (c *Client) BySubject(ctx context.Context, subject string, onlyActive bool) ([]memstore.Fact, error) {
	return c.List(ctx, memstore.QueryOpts{Subject: subject, OnlyActive: onlyActive})
}

func (c *Client) History(ctx context.Context, id int64, subject string) ([]memstore.HistoryEntry, error) {
	var entries []memstore.HistoryEntry
	var path string
	if id > 0 {
		path = fmt.Sprintf("/v1/facts/%d/history", id)
	} else {
		path = "/v1/history/" + url.PathEscape(subject)
	}
	if err := c.get(ctx, path, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (c *Client) Search(ctx context.Context, query string, opts memstore.SearchOpts) ([]memstore.SearchResult, error) {
	body := searchBody(query, opts)
	var results []memstore.SearchResult
	if err := c.post(ctx, "/v1/search", body, &results); err != nil {
		return nil, err
	}
	return results, nil
}

func (c *Client) SearchBatch(ctx context.Context, queries []string, opts memstore.SearchOpts) ([][]memstore.SearchResult, error) {
	// Execute sequentially — daemon doesn't have batch search endpoint yet.
	results := make([][]memstore.SearchResult, len(queries))
	for i, q := range queries {
		r, err := c.Search(ctx, q, opts)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	return results, nil
}

func (c *Client) SearchFTS(ctx context.Context, query string, opts memstore.SearchOpts) ([]memstore.SearchResult, error) {
	body := searchBody(query, opts)
	var results []memstore.SearchResult
	if err := c.post(ctx, "/v1/search/fts", body, &results); err != nil {
		return nil, err
	}
	return results, nil
}

func (c *Client) ListSubsystems(ctx context.Context, subject string) ([]string, error) {
	q := ""
	if subject != "" {
		q = "?subject=" + url.QueryEscape(subject)
	}
	var subs []string
	if err := c.get(ctx, "/v1/subsystems"+q, &subs); err != nil {
		return nil, err
	}
	return subs, nil
}

// Embedding methods are no-ops on the client — the daemon handles embeddings.

func (c *Client) NeedingEmbedding(_ context.Context, _ int) ([]memstore.Fact, error) {
	return nil, nil
}

func (c *Client) SetEmbedding(_ context.Context, _ int64, _ []float32) error {
	return nil
}

func (c *Client) EmbedFacts(_ context.Context, _ int) (int, error) {
	return 0, nil
}

// --- Links ---

func (c *Client) LinkFacts(ctx context.Context, sourceID, targetID int64, linkType string, bidirectional bool, label string, metadata map[string]any) (int64, error) {
	body := map[string]any{
		"source_id":     sourceID,
		"target_id":     targetID,
		"link_type":     linkType,
		"bidirectional": bidirectional,
		"label":         label,
		"metadata":      metadata,
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := c.post(ctx, "/v1/links", body, &result); err != nil {
		return 0, err
	}
	return result.ID, nil
}

func (c *Client) GetLink(ctx context.Context, linkID int64) (*memstore.Link, error) {
	var link memstore.Link
	if err := c.get(ctx, fmt.Sprintf("/v1/links/%d", linkID), &link); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &link, nil
}

func (c *Client) GetLinks(ctx context.Context, factID int64, direction memstore.LinkDirection, linkTypes ...string) ([]memstore.Link, error) {
	q := url.Values{}
	switch direction {
	case memstore.LinkOutbound:
		q.Set("direction", "outbound")
	case memstore.LinkInbound:
		q.Set("direction", "inbound")
	}
	if len(linkTypes) > 0 {
		q.Set("types", strings.Join(linkTypes, ","))
	}
	var links []memstore.Link
	if err := c.get(ctx, fmt.Sprintf("/v1/facts/%d/links?%s", factID, q.Encode()), &links); err != nil {
		return nil, err
	}
	return links, nil
}

func (c *Client) UpdateLink(ctx context.Context, linkID int64, label string, metadata map[string]any) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("/v1/links/%d", linkID), map[string]any{
		"label":    label,
		"metadata": metadata,
	}, nil)
}

func (c *Client) DeleteLink(ctx context.Context, linkID int64) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("/v1/links/%d", linkID), nil, nil)
}

// LearnFile sends a single source file to the server for learning.
func (c *Client) LearnFile(ctx context.Context, opts memstore.LearnFileOpts) (*memstore.LearnFileResult, error) {
	body := map[string]any{
		"subject":      opts.Subject,
		"file_path":    opts.FilePath,
		"content":      opts.Content,
		"content_hash": opts.ContentHash,
		"module_path":  opts.ModulePath,
		"package_name": opts.PackageName,
		"force":        opts.Force,
	}
	if opts.SessionID != "" {
		body["session_id"] = opts.SessionID
	}
	var result memstore.LearnFileResult
	if err := c.post(ctx, "/v1/learn", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// LearnFinalize triggers session-level synthesis (package/repo facts, containment
// links, cross-file links) for facts accumulated during a learn session.
func (c *Client) LearnFinalize(ctx context.Context, opts memstore.LearnFinalizeOpts) (*memstore.LearnFinalizeResult, error) {
	var result memstore.LearnFinalizeResult
	if err := c.post(ctx, "/v1/learn/finalize", opts, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetPendingHints returns unconsumed context hints matching sessionID or cwd (OR semantics).
// Either may be empty; pass both for maximum coverage.
func (c *Client) GetPendingHints(ctx context.Context, sessionID, cwd string) ([]memstore.ContextHint, error) {
	var hints []memstore.ContextHint
	q := url.Values{}
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	if cwd != "" {
		q.Set("cwd", cwd)
	}
	if err := c.get(ctx, "/v1/context/hints?"+q.Encode(), &hints); err != nil {
		return nil, err
	}
	return hints, nil
}

// MarkHintConsumed marks a context hint as consumed so it is not re-injected.
func (c *Client) MarkHintConsumed(ctx context.Context, hintID int64) error {
	return c.post(ctx, fmt.Sprintf("/v1/context/hints/%d/consume", hintID), nil, nil)
}

// RecordInjection records that a ref was injected into a session (dedup log).
func (c *Client) RecordInjection(ctx context.Context, sessionID, refID, refType string) error {
	return c.post(ctx, "/v1/context/injections", map[string]any{
		"session_id": sessionID,
		"ref_id":     refID,
		"ref_type":   refType,
	}, nil)
}

// RecordFeedback posts context feedback to the daemon. Implements the minimal
// subset of memstore.SessionStore needed by memstore-mcp for memory_rate_context.
func (c *Client) RecordFeedback(ctx context.Context, fb memstore.ContextFeedback) error {
	return c.post(ctx, "/v1/context/feedback", map[string]any{
		"ref_id":     fb.RefID,
		"ref_type":   fb.RefType,
		"session_id": fb.SessionID,
		"score":      fb.Score,
		"reason":     fb.Reason,
	}, nil)
}

// PostSessionHook forwards a raw Claude Code Stop hook payload to the daemon.
func (c *Client) PostSessionHook(ctx context.Context, rawPayload json.RawMessage) error {
	return c.do(ctx, "POST", "/v1/sessions/hook", rawPayload, nil)
}

// PostSessionTranscript forwards a JSONL session transcript to the daemon.
func (c *Client) PostSessionTranscript(ctx context.Context, sessionID, cwd, content string) error {
	return c.post(ctx, "/v1/sessions/transcript", map[string]any{
		"session_id": sessionID,
		"cwd":        cwd,
		"content":    content,
	}, nil)
}

// Close is a no-op for the HTTP client — there is no local resource to release.
func (c *Client) Close() error { return nil }

// --- HTTP helpers ---

func (c *Client) get(ctx context.Context, path string, result any) error {
	return c.do(ctx, "GET", path, nil, result)
}

func (c *Client) post(ctx context.Context, path string, body, result any) error {
	return c.do(ctx, "POST", path, body, result)
}

func (c *Client) do(ctx context.Context, method, path string, body, result any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("memstored request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&apiErr)
		return &HTTPError{Code: resp.StatusCode, Message: apiErr.Error}
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func searchBody(query string, opts memstore.SearchOpts) map[string]any {
	body := map[string]any{"query": query}
	if opts.Subject != "" {
		body["subject"] = opts.Subject
	}
	if opts.Category != "" {
		body["category"] = opts.Category
	}
	if opts.Kind != "" {
		body["kind"] = opts.Kind
	}
	if opts.Subsystem != "" {
		body["subsystem"] = opts.Subsystem
	}
	if opts.MaxResults > 0 {
		body["limit"] = opts.MaxResults
	}
	if opts.FTSWeight > 0 {
		body["fts_weight"] = opts.FTSWeight
	}
	if opts.VecWeight > 0 {
		body["vec_weight"] = opts.VecWeight
	}
	return body
}

// HTTPError represents an error response from the daemon.
type HTTPError struct {
	Code    int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("memstored %d: %s", e.Code, e.Message)
}

func isNotFound(err error) bool {
	if he, ok := err.(*HTTPError); ok {
		return he.Code == http.StatusNotFound
	}
	return false
}
