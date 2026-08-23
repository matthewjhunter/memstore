package main

import (
	"slices"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
)

// The default is asserted against httpapi.Identity.Allows rather than against
// the string "read", so this fails if either the CLI default or the
// enforcement rule drifts. Checking the literal alone would keep passing if
// Allows started implying write.
func TestDefaultIssueScopesAreReadOnly(t *testing.T) {
	id := httpapi.Identity{Scopes: splitCSV(defaultIssueScopes)}

	if !id.Allows(memstore.ScopeRead) {
		t.Errorf("default-scoped token cannot read; scopes = %v", id.Scopes)
	}
	for _, scope := range []string{memstore.ScopeWrite, memstore.ScopeAdmin, memstore.ScopeIngest} {
		if id.Allows(scope) {
			t.Errorf("default-scoped token allows %s; least privilege means read only", scope)
		}
	}
}

// The empty scope set must keep meaning read+write. Tokens issued before scope
// enforcement carry it, and the new issuance default must not be confused with
// a change to that back-compat rule -- tightening it would break deployments
// running on pre-enforcement tokens.
func TestEmptyScopesStillImplyReadWrite(t *testing.T) {
	id := httpapi.Identity{}

	if !id.Allows(memstore.ScopeRead) || !id.Allows(memstore.ScopeWrite) {
		t.Error("empty scope set lost its read+write grant; pre-enforcement tokens would break")
	}
	if id.Allows(memstore.ScopeIngest) {
		t.Error("empty scope set implies ingest; ingest must be granted explicitly")
	}
}

// A non-empty default means the issued scopes are always worth printing, which
// the receipt relies on to tell the holder what they got.
func TestDefaultIssueScopesAreNotEmpty(t *testing.T) {
	if got := splitCSV(defaultIssueScopes); len(got) == 0 || slices.Contains(got, "") {
		t.Errorf("defaultIssueScopes %q parses to %v; must be a real scope list", defaultIssueScopes, got)
	}
}
