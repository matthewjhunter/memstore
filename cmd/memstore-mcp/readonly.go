package main

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/mcpserver"
)

// whoAmIQuerier is the slice of httpclient.Client this file needs, so the
// resolution logic can be tested without a daemon.
type whoAmIQuerier interface {
	WhoAmI(ctx context.Context) (memstore.WhoAmIResponse, error)
}

// whoAmITimeout bounds the startup query. It runs before the server serves
// anything, so a hung daemon would otherwise hang the session at launch. On
// timeout the flag value stands.
const whoAmITimeout = 5 * time.Second

// applyTokenScopes queries the daemon for the caller's effective permissions
// and returns the read-only decision. It logs to stderr, which is where a
// stdio MCP server's diagnostics belong -- stdout carries the protocol.
func applyTokenScopes(ctx context.Context, q whoAmIQuerier, flagReadOnly bool) bool {
	ctx, cancel := context.WithTimeout(ctx, whoAmITimeout)
	defer cancel()

	who, err := q.WhoAmI(ctx)
	switch {
	case err != nil && !flagReadOnly:
		log.Printf("memstore-mcp: could not read token scopes (%v); "+
			"registering write tools as configured. Pass --read-only to force retrieval-only.", err)
	case err == nil && !flagReadOnly && !slices.Contains(who.Allows, memstore.ScopeWrite):
		log.Printf("memstore-mcp: token %q lacks the write scope (allows: %v); "+
			"registering retrieval tools only.", who.Name, who.Allows)
	}
	return mcpserver.ResolveReadOnly(flagReadOnly, who, err)
}
