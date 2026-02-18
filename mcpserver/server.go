// Package mcpserver provides an MCP (Model Context Protocol) server that
// exposes a memstore-backed persistent memory system as MCP tools. It is
// designed to give Claude (or any MCP client) durable, searchable memory
// across sessions via hybrid FTS5 + vector search.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MemoryServer bridges MCP tool calls to a memstore.Store.
type MemoryServer struct {
	store    memstore.Store
	embedder memstore.Embedder
}

// NewMemoryServer creates a server backed by the given store and embedder.
// The embedder is used to compute embeddings at insert time so search always
// works. Both parameters are required.
func NewMemoryServer(store memstore.Store, embedder memstore.Embedder) *MemoryServer {
	return &MemoryServer{store: store, embedder: embedder}
}

// --- Input types (MCP SDK infers JSON schemas from struct tags) ---

// StoreInput is the input schema for the memory_store tool.
type StoreInput struct {
	Content    string         `json:"content" jsonschema:"the factual claim or memory to store"`
	Subject    string         `json:"subject" jsonschema:"the entity this fact is about (e.g. a person or project)"`
	Category   string         `json:"category,omitempty" jsonschema:"fact category: preference, identity, project, capability, relationship, world, or note (default: note)"`
	Metadata   map[string]any `json:"metadata,omitempty" jsonschema:"optional key-value metadata to attach"`
	Supersedes *int64         `json:"supersedes,omitempty" jsonschema:"ID of an existing fact that this new fact replaces (preserves history unlike delete)"`
}

// SearchInput is the input schema for the memory_search tool.
type SearchInput struct {
	Query             string         `json:"query" jsonschema:"natural language search query"`
	Subject           string         `json:"subject,omitempty" jsonschema:"filter results to a specific subject entity"`
	Category          string         `json:"category,omitempty" jsonschema:"filter results to a specific category"`
	Limit             int            `json:"limit,omitempty" jsonschema:"maximum number of results (default 10)"`
	IncludeSuperseded bool           `json:"include_superseded,omitempty" jsonschema:"if true, include superseded facts in results (tagged with [SUPERSEDED])"`
	Metadata          map[string]any `json:"metadata,omitempty" jsonschema:"filter by metadata fields (equality match, e.g. {\"source\": \"conversation\"})"`
}

// ListInput is the input schema for the memory_list tool.
type ListInput struct {
	Subject  string         `json:"subject,omitempty" jsonschema:"filter by subject entity"`
	Category string         `json:"category,omitempty" jsonschema:"filter by category"`
	Limit    int            `json:"limit,omitempty" jsonschema:"maximum number of results (default 20)"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"filter by metadata fields (equality match, e.g. {\"source\": \"conversation\"})"`
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

// StatusInput is the input schema for the memory_status tool.
type StatusInput struct{}

// --- Tool registration ---

// Register adds all memory tools to the given MCP server.
func (ms *MemoryServer) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_store",
		Description: `Store a fact or memory. Persists across sessions with automatic embedding for semantic search. Use this whenever you learn something worth remembering about the user, their projects, preferences, or any durable knowledge.

Store aggressively — it is better to store something and supersede it later than to lose it. Good candidates: user preferences, project decisions, technical choices, names, relationships, workflow habits, things the user corrects you on, environment details.

Conventions:
- subject: lowercase, singular entity name (e.g. "matthew", "memstore", "home-server"). This is the primary lookup key — be consistent.
- category: one of preference, identity, project, capability, relationship, world, note. Use "note" as the catch-all.
- metadata: use for attribution (source), confidence, temporal bounds (valid_from/valid_until), or any structured data.
- supersedes: pass the ID of the fact this replaces. The old fact is preserved in history. Always prefer superseding over deleting.`,
	}, ms.HandleStore)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_search",
		Description: `Search stored memories using hybrid full-text and semantic search. Returns ranked results with relevance scores. Use this to recall information from previous sessions.

Search early and often — check what you already know before asking the user to repeat themselves. Search at the start of a conversation if the user's identity or project context is unclear.

Set include_superseded=true when you need to understand how a fact has changed over time, or to find old information that may have been prematurely superseded.`,
	}, ms.HandleSearch)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_list",
		Description: `Browse stored memories with optional subject and category filters. Unlike search, this does not require a query — use it to see what you know about a topic.

Use this when you want a complete picture of a subject rather than matching a specific query. Good for: "what do I know about this user?", "what preferences are stored?", getting an overview before a task.`,
	}, ms.HandleList)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_delete",
		Description: `Delete a specific memory by its ID. Use this to remove outdated or incorrect information.

Prefer memory_supersede or memory_store with the 'supersedes' parameter instead — these preserve the old fact in history. Only delete facts that are genuinely wrong or harmful, not just outdated.`,
	}, ms.HandleDelete)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_supersede",
		Description: `Mark an existing fact as superseded by a newer fact. Both facts must already exist. The old fact is preserved in history but excluded from normal search results.

Use this when you discover a stored fact is outdated and you've already stored the replacement. For a single-step "store and supersede", use memory_store with the supersedes parameter instead.`,
	}, ms.HandleSupersede)

	mcp.AddTool(s, &mcp.Tool{
		Name: "memory_history",
		Description: `Show the supersession history for a fact (by ID) or all facts for a subject (by subject). Reveals how knowledge has evolved over time, including superseded facts with their replacement chain.

Use by ID to trace a specific fact's lineage. Use by subject for a complete audit of everything stored about an entity.`,
	}, ms.HandleHistory)

	mcp.AddTool(s, &mcp.Tool{
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
}

// --- Handlers ---

func (ms *MemoryServer) HandleStore(ctx context.Context, _ *mcp.CallToolRequest, input StoreInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(input.Content) == "" {
		return textResult("Error: content is required", true), nil, nil
	}
	if strings.TrimSpace(input.Subject) == "" {
		return textResult("Error: subject is required", true), nil, nil
	}

	category := strings.TrimSpace(input.Category)
	if category == "" {
		category = "note"
	}

	// Dedup check.
	exists, err := ms.store.Exists(ctx, input.Content, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error checking for duplicates: %v", err), true), nil, nil
	}
	if exists {
		return textResult("Already stored (duplicate).", false), nil, nil
	}

	// Compute embedding.
	emb, err := memstore.Single(ctx, ms.embedder, input.Content)
	if err != nil {
		return textResult(fmt.Sprintf("Error computing embedding: %v", err), true), nil, nil
	}

	fact := memstore.Fact{
		Content:   input.Content,
		Subject:   input.Subject,
		Category:  category,
		Embedding: emb,
	}
	if len(input.Metadata) > 0 {
		metaJSON, err := json.Marshal(input.Metadata)
		if err != nil {
			return textResult(fmt.Sprintf("Error encoding metadata: %v", err), true), nil, nil
		}
		fact.Metadata = metaJSON
	}

	id, err := ms.store.Insert(ctx, fact)
	if err != nil {
		return textResult(fmt.Sprintf("Error storing fact: %v", err), true), nil, nil
	}

	msg := fmt.Sprintf("Stored (id=%d, subject=%q, category=%q).", id, input.Subject, category)

	// Handle supersession after successful insert.
	if input.Supersedes != nil {
		if err := ms.store.Supersede(ctx, *input.Supersedes, id); err != nil {
			msg += fmt.Sprintf(" Warning: supersession of fact %d failed: %v", *input.Supersedes, err)
		} else {
			msg += fmt.Sprintf(" Superseded fact %d.", *input.Supersedes)
		}
	}

	return textResult(msg, false), nil, nil
}

func (ms *MemoryServer) HandleSearch(ctx context.Context, _ *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(input.Query) == "" {
		return textResult("Error: query is required", true), nil, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	opts := memstore.SearchOpts{
		MaxResults:      limit,
		Subject:         input.Subject,
		Category:        input.Category,
		OnlyActive:      !input.IncludeSuperseded,
		MetadataFilters: metadataFilters(input.Metadata),
		// Stable facts (preference, identity) don't decay.
		// Ephemeral notes get 30-day half-life.
		CategoryDecay: map[string]time.Duration{
			"note": 720 * time.Hour, // 30 days
		},
	}

	results, err := ms.store.Search(ctx, input.Query, opts)
	if err != nil {
		return textResult(fmt.Sprintf("Error searching: %v", err), true), nil, nil
	}

	if len(results) == 0 {
		return textResult("No matching memories found.", false), nil, nil
	}

	// Auto-touch: bump use_count for all returned facts.
	ids := make([]int64, len(results))
	for i, r := range results {
		ids[i] = r.Fact.ID
	}
	_ = ms.store.Touch(ctx, ids) // best-effort; don't fail the search

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] (id=%d, score=%.3f, used=%d, confirmed=%d) %s | %s",
			i+1, r.Fact.ID, r.Combined,
			r.Fact.UseCount+1, r.Fact.ConfirmedCount, // +1 because Touch just ran
			r.Fact.Subject, r.Fact.Category)
		if r.Fact.SupersededBy != nil {
			fmt.Fprintf(&b, " [SUPERSEDED by %d]", *r.Fact.SupersededBy)
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "    %s\n", r.Fact.Content)
		if len(r.Fact.Metadata) > 0 && string(r.Fact.Metadata) != "null" {
			fmt.Fprintf(&b, "    metadata: %s\n", string(r.Fact.Metadata))
		}
		fmt.Fprintln(&b)
	}

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleList(ctx context.Context, _ *mcp.CallToolRequest, input ListInput) (*mcp.CallToolResult, any, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}

	opts := memstore.QueryOpts{
		Subject:         input.Subject,
		Category:        input.Category,
		OnlyActive:      true,
		Limit:           limit,
		MetadataFilters: metadataFilters(input.Metadata),
	}

	facts, err := ms.store.List(ctx, opts)
	if err != nil {
		return textResult(fmt.Sprintf("Error listing: %v", err), true), nil, nil
	}

	if len(facts) == 0 {
		return textResult("No memories found.", false), nil, nil
	}

	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "[id=%d, used=%d, confirmed=%d] %s | %s | %s\n",
			f.ID, f.UseCount, f.ConfirmedCount,
			f.Subject, f.Category, f.CreatedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "  %s\n", f.Content)
		if len(f.Metadata) > 0 && string(f.Metadata) != "null" {
			fmt.Fprintf(&b, "  metadata: %s\n", string(f.Metadata))
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "%d memories listed.", len(facts))

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleDelete(ctx context.Context, _ *mcp.CallToolRequest, input DeleteInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 {
		return textResult("Error: id must be a positive integer", true), nil, nil
	}

	err := ms.store.Delete(ctx, input.ID)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Deleted memory %d.", input.ID), false), nil, nil
}

func (ms *MemoryServer) HandleStatus(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, any, error) {
	count, err := ms.store.ActiveCount(ctx)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	// Get subject and category breakdown.
	facts, err := ms.store.List(ctx, memstore.QueryOpts{OnlyActive: true})
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	subjects := make(map[string]int)
	categories := make(map[string]int)
	for _, f := range facts {
		subjects[f.Subject]++
		categories[f.Category]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Active memories: %d\n\n", count)

	if len(categories) > 0 {
		fmt.Fprintln(&b, "By category:")
		for cat, n := range categories {
			fmt.Fprintf(&b, "  %s: %d\n", cat, n)
		}
		fmt.Fprintln(&b)
	}

	if len(subjects) > 0 {
		fmt.Fprintln(&b, "By subject:")
		for subj, n := range subjects {
			fmt.Fprintf(&b, "  %s: %d\n", subj, n)
		}
	}

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleSupersede(ctx context.Context, _ *mcp.CallToolRequest, input SupersedeInput) (*mcp.CallToolResult, any, error) {
	if input.OldID <= 0 || input.NewID <= 0 {
		return textResult("Error: both old_id and new_id must be positive integers", true), nil, nil
	}
	if input.OldID == input.NewID {
		return textResult("Error: old_id and new_id must be different", true), nil, nil
	}

	// Validate both facts exist.
	oldFact, err := ms.store.Get(ctx, input.OldID)
	if err != nil {
		return textResult(fmt.Sprintf("Error looking up fact %d: %v", input.OldID, err), true), nil, nil
	}
	if oldFact == nil {
		return textResult(fmt.Sprintf("Error: fact %d not found", input.OldID), true), nil, nil
	}
	if oldFact.SupersededBy != nil {
		return textResult(fmt.Sprintf("Error: fact %d is already superseded by fact %d", input.OldID, *oldFact.SupersededBy), true), nil, nil
	}

	newFact, err := ms.store.Get(ctx, input.NewID)
	if err != nil {
		return textResult(fmt.Sprintf("Error looking up fact %d: %v", input.NewID, err), true), nil, nil
	}
	if newFact == nil {
		return textResult(fmt.Sprintf("Error: fact %d not found", input.NewID), true), nil, nil
	}

	if err := ms.store.Supersede(ctx, input.OldID, input.NewID); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	return textResult(fmt.Sprintf("Superseded fact %d with fact %d.\n  Old: %s\n  New: %s",
		input.OldID, input.NewID, oldFact.Content, newFact.Content), false), nil, nil
}

func (ms *MemoryServer) HandleHistory(ctx context.Context, _ *mcp.CallToolRequest, input HistoryInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 && strings.TrimSpace(input.Subject) == "" {
		return textResult("Error: provide either id or subject", true), nil, nil
	}

	entries, err := ms.store.History(ctx, input.ID, input.Subject)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	if len(entries) == 0 {
		return textResult("No history found.", false), nil, nil
	}

	var b strings.Builder
	for _, e := range entries {
		status := "ACTIVE"
		if e.Fact.SupersededBy != nil {
			status = fmt.Sprintf("SUPERSEDED by %d", *e.Fact.SupersededBy)
		}
		fmt.Fprintf(&b, "[%d/%d] (id=%d) %s | %s | %s | %s\n",
			e.Position+1, e.ChainLength, e.Fact.ID, e.Fact.Subject, e.Fact.Category,
			status, e.Fact.CreatedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "  %s\n", e.Fact.Content)
		if len(e.Fact.Metadata) > 0 && string(e.Fact.Metadata) != "null" {
			fmt.Fprintf(&b, "  metadata: %s\n", string(e.Fact.Metadata))
		}
		fmt.Fprintln(&b)
	}

	return textResult(b.String(), false), nil, nil
}

func (ms *MemoryServer) HandleConfirm(ctx context.Context, _ *mcp.CallToolRequest, input ConfirmInput) (*mcp.CallToolResult, any, error) {
	if input.ID <= 0 {
		return textResult("Error: id must be a positive integer", true), nil, nil
	}

	if err := ms.store.Confirm(ctx, input.ID); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err), true), nil, nil
	}

	// Re-fetch to show the updated count.
	fact, err := ms.store.Get(ctx, input.ID)
	if err != nil || fact == nil {
		return textResult(fmt.Sprintf("Confirmed fact %d.", input.ID), false), nil, nil
	}

	return textResult(fmt.Sprintf("Confirmed fact %d (count=%d). %s",
		input.ID, fact.ConfirmedCount, fact.Content), false), nil, nil
}

// metadataFilters converts a map[string]any (from MCP input) to memstore.MetadataFilter
// equality conditions.
func metadataFilters(m map[string]any) []memstore.MetadataFilter {
	if len(m) == 0 {
		return nil
	}
	filters := make([]memstore.MetadataFilter, 0, len(m))
	for k, v := range m {
		filters = append(filters, memstore.MetadataFilter{Key: k, Op: "=", Value: v})
	}
	return filters
}

// textResult builds a CallToolResult with a single text content block.
func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: isError,
	}
}
