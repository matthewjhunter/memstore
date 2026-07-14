package main

import (
	"flag"
	"slices"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// newTestFlagSet builds a flag set shaped like issue-token's: a couple of
// value flags and one boolean, so the permutation logic is exercised against
// both kinds.
func newTestFlagSet() (*flag.FlagSet, *string, *string, *bool) {
	fs := flag.NewFlagSet("issue-token", flag.ContinueOnError)
	user := fs.String("user", "", "user")
	scopes := fs.String("scopes", "", "scopes")
	force := fs.Bool("force", false, "force")
	return fs, user, scopes, force
}

func TestParseAdminArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantPos    []string
		wantUser   string
		wantScopes string
		wantForce  bool
	}{
		{
			name:       "flags before positional",
			args:       []string{"--user", "matthew", "--scopes", "admin", "matthew@kraken"},
			wantPos:    []string{"matthew@kraken"},
			wantUser:   "matthew",
			wantScopes: "admin",
		},
		{
			name:       "flags after positional",
			args:       []string{"matthew@kraken", "--user", "matthew", "--scopes", "admin"},
			wantPos:    []string{"matthew@kraken"},
			wantUser:   "matthew",
			wantScopes: "admin",
		},
		{
			name:       "flags interleaved with positional",
			args:       []string{"--user", "matthew", "matthew@kraken", "--scopes", "admin"},
			wantPos:    []string{"matthew@kraken"},
			wantUser:   "matthew",
			wantScopes: "admin",
		},
		{
			name:      "boolean flag after positional",
			args:      []string{"matthew@kraken", "--force"},
			wantPos:   []string{"matthew@kraken"},
			wantForce: true,
		},
		{
			name:     "equals form after positional",
			args:     []string{"matthew@kraken", "--user=matthew"},
			wantPos:  []string{"matthew@kraken"},
			wantUser: "matthew",
		},
		{
			name:    "no flags",
			args:    []string{"matthew@kraken"},
			wantPos: []string{"matthew@kraken"},
		},
		{
			name:    "no args",
			args:    nil,
			wantPos: nil,
		},
		{
			name:     "terminator makes the rest positional",
			args:     []string{"--user", "matthew", "--", "--not-a-flag"},
			wantPos:  []string{"--not-a-flag"},
			wantUser: "matthew",
		},
		{
			name:     "positional order is preserved",
			args:     []string{"one", "--user", "matthew", "two", "--", "three"},
			wantPos:  []string{"one", "two", "three"},
			wantUser: "matthew",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs, user, scopes, force := newTestFlagSet()
			pos, err := parseAdminArgs(fs, tt.args)
			if err != nil {
				t.Fatalf("parseAdminArgs(%q): %v", tt.args, err)
			}
			if !slices.Equal(pos, tt.wantPos) {
				t.Errorf("positional = %q, want %q", pos, tt.wantPos)
			}
			if *user != tt.wantUser {
				t.Errorf("--user = %q, want %q", *user, tt.wantUser)
			}
			if *scopes != tt.wantScopes {
				t.Errorf("--scopes = %q, want %q", *scopes, tt.wantScopes)
			}
			if *force != tt.wantForce {
				t.Errorf("--force = %v, want %v", *force, tt.wantForce)
			}
		})
	}
}

func TestParseAdminArgs_unknownFlag(t *testing.T) {
	fs, _, _, _ := newTestFlagSet()
	fs.SetOutput(discard{})
	if _, err := parseAdminArgs(fs, []string{"matthew@kraken", "--nope"}); err == nil {
		t.Fatal("expected an error for an unknown flag, got nil")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestDefaultAdminNamespace(t *testing.T) {
	saved := cliConfig
	t.Cleanup(func() { cliConfig = saved })

	// The daemon's own default (memstore.DefaultConfig) is what admin commands
	// must target when the operator has not configured a namespace -- an empty
	// default is what made list-users report "No users in namespace """ on a
	// live deployment whose rows live in "default".
	cliConfig = memstore.AppConfig{}
	if got, want := defaultAdminNamespace(), memstore.DefaultConfig().Namespace; got != want {
		t.Errorf("defaultAdminNamespace() with unset config = %q, want %q", got, want)
	}
	if got := defaultAdminNamespace(); got == "" {
		t.Error("defaultAdminNamespace() must never return the empty namespace")
	}

	cliConfig = memstore.AppConfig{Namespace: "tenant-a"}
	if got, want := defaultAdminNamespace(), "tenant-a"; got != want {
		t.Errorf("defaultAdminNamespace() with configured namespace = %q, want %q", got, want)
	}
}
