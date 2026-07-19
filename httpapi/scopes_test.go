package httpapi

import "testing"

func TestIdentityAllows(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   map[string]bool
	}{
		{
			// Tokens issued before scope enforcement carry no scopes. They keep
			// working for facts, and must NOT silently acquire ingest.
			name:   "empty scopes get the legacy read+write grant, never ingest",
			scopes: nil,
			want: map[string]bool{
				ScopeRead: true, ScopeWrite: true,
				ScopeAdmin: false, ScopeIngest: false,
			},
		},
		{
			// The critical case: the MCP server's token is typically admin, and
			// the whole provenance design rests on the model being unable to
			// reach ingest. Admin must not be a way around that.
			name:   "admin implies read and write but NOT ingest",
			scopes: []string{ScopeAdmin},
			want: map[string]bool{
				ScopeRead: true, ScopeWrite: true,
				ScopeAdmin: true, ScopeIngest: false,
			},
		},
		{
			name:   "explicit ingest grants only ingest",
			scopes: []string{ScopeIngest},
			want: map[string]bool{
				ScopeRead: false, ScopeWrite: false,
				ScopeAdmin: false, ScopeIngest: true,
			},
		},
		{
			name:   "read only",
			scopes: []string{ScopeRead},
			want: map[string]bool{
				ScopeRead: true, ScopeWrite: false,
				ScopeAdmin: false, ScopeIngest: false,
			},
		},
		{
			name:   "write does not imply read",
			scopes: []string{ScopeWrite},
			want: map[string]bool{
				ScopeRead: false, ScopeWrite: true,
				ScopeAdmin: false, ScopeIngest: false,
			},
		},
		{
			name:   "an ingest client can also be granted read explicitly",
			scopes: []string{ScopeRead, ScopeIngest},
			want: map[string]bool{
				ScopeRead: true, ScopeWrite: false,
				ScopeAdmin: false, ScopeIngest: true,
			},
		},
		{
			name:   "unknown scopes grant nothing",
			scopes: []string{"superuser", "root"},
			want: map[string]bool{
				ScopeRead: false, ScopeWrite: false,
				ScopeAdmin: false, ScopeIngest: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := Identity{Scopes: tc.scopes}
			for scope, want := range tc.want {
				if got := id.Allows(scope); got != want {
					t.Errorf("Allows(%q) = %v, want %v (scopes=%v)",
						scope, got, want, tc.scopes)
				}
			}
		})
	}
}

// The legacy single-key path builds an Identity with no scopes. It must behave
// like any other unscoped token: usable for facts, powerless for ingest.
func TestLegacyIdentityCannotIngest(t *testing.T) {
	id := Identity{Name: "legacy", Source: "legacy"}
	if !id.Allows(ScopeWrite) {
		t.Error("legacy identity should retain write")
	}
	if id.Allows(ScopeIngest) {
		t.Error("legacy identity must not be able to ingest")
	}
}
