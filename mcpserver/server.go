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
	Content  string         `json:"content" jsonschema:"the factual claim or memory to store"`
	Subject  string         `json:"subject" jsonschema:"the entity this fact is about (e.g. a person or project)"`
	Category string         `json:"category,omitempty" jsonschema:"fact category: preference, identity, project, capability, relationship, world, or note (default: note)"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"optional key-value metadata to attach"`
}

// SearchInput is the input schema for the memory_search tool.
type SearchInput struct {
	Query    string `json:"query" jsonschema:"natural language search query"`
	Subject  string `json:"subject,omitempty" jsonschema:"filter results to a specific subject entity"`
	Category string `json:"category,omitempty" jsonschema:"filter results to a specific category"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 10)"`
}

// ListInput is the input schema for the memory_list tool.
type ListInput struct {
	Subject  string `json:"subject,omitempty" jsonschema:"filter by subject entity"`
	Category string `json:"category,omitempty" jsonschema:"filter by category"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 20)"`
}

// DeleteInput is the input schema for the memory_delete tool.
type DeleteInput struct {
	ID int64 `json:"id" jsonschema:"the fact ID to delete"`
}

// StatusInput is the input schema for the memory_status tool.
type StatusInput struct{}

// --- Tool registration ---

// Register adds all memory tools to the given MCP server.
func (ms *MemoryServer) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_store",
		Description: "Store a fact or memory. Persists across sessions with automatic embedding for semantic search. Use this whenever you learn something worth remembering about the user, their projects, preferences, or any durable knowledge.",
	}, ms.HandleStore)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_search",
		Description: "Search stored memories using hybrid full-text and semantic search. Returns ranked results with relevance scores. Use this to recall information from previous sessions.",
	}, ms.HandleSearch)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_list",
		Description: "Browse stored memories with optional subject and category filters. Unlike search, this does not require a query — use it to see what you know about a topic.",
	}, ms.HandleList)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_delete",
		Description: "Delete a specific memory by its ID. Use this to remove outdated or incorrect information.",
	}, ms.HandleDelete)

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

	return textResult(fmt.Sprintf("Stored (id=%d, subject=%q, category=%q).", id, input.Subject, category), false), nil, nil
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
		MaxResults: limit,
		Subject:    input.Subject,
		Category:   input.Category,
		OnlyActive: true,
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

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] (id=%d, score=%.3f) %s | %s\n",
			i+1, r.Fact.ID, r.Combined, r.Fact.Subject, r.Fact.Category)
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
		Subject:    input.Subject,
		Category:   input.Category,
		OnlyActive: true,
		Limit:      limit,
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
		fmt.Fprintf(&b, "[id=%d] %s | %s | %s\n",
			f.ID, f.Subject, f.Category, f.CreatedAt.Format("2006-01-02 15:04"))
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

// textResult builds a CallToolResult with a single text content block.
func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: isError,
	}
}
