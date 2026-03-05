package memstore

import "testing"

func TestMatchFilePattern(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		filePath string
		want     bool
	}{
		{"exact suffix match", "internal/feeds/fetcher.go", "/home/user/go/src/project/internal/feeds/fetcher.go", true},
		{"double star at end", "internal/feeds/**", "/home/user/go/src/project/internal/feeds/fetcher.go", true},
		{"double star nested", "internal/feeds/**", "/home/user/go/src/project/internal/feeds/sub/deep.go", true},
		{"double star with suffix", "internal/**/*.go", "/home/user/go/src/project/internal/feeds/fetcher.go", true},
		{"single star in filename", "internal/feeds/*.go", "/home/user/go/src/project/internal/feeds/fetcher.go", true},
		{"no match wrong directory", "internal/auth/**", "/home/user/go/src/project/internal/feeds/fetcher.go", false},
		{"no match wrong extension", "internal/feeds/*.py", "/home/user/go/src/project/internal/feeds/fetcher.go", false},
		{"double star at start", "**/*.go", "/home/user/go/src/project/internal/feeds/fetcher.go", true},
		{"double star only", "**", "/home/user/go/src/project/anything.go", true},
		{"simple filename", "Makefile", "/home/user/project/Makefile", true},
		{"cmd subpackage pattern", "cmd/**", "/home/user/go/src/project/cmd/memstore/main.go", true},
		{"relative path exact", "cmd/memstore/main.go", "/home/user/go/src/project/cmd/memstore/main.go", true},
		{"double star middle with extension filter", "cmd/**/main.go", "/home/user/go/src/project/cmd/memstore/main.go", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchFilePattern(tt.pattern, tt.filePath)
			if got != tt.want {
				t.Errorf("MatchFilePattern(%q, %q) = %v, want %v", tt.pattern, tt.filePath, got, tt.want)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"simple exact", "foo.go", "foo.go", true},
		{"simple star", "*.go", "foo.go", true},
		{"simple star miss", "*.go", "foo.py", false},
		{"nested star", "feeds/*.go", "feeds/fetcher.go", true},
		{"doublestar suffix", "feeds/**", "feeds/fetcher.go", true},
		{"doublestar deep", "feeds/**", "feeds/sub/deep.go", true},
		{"doublestar prefix", "**/fetcher.go", "feeds/fetcher.go", true},
		{"doublestar both", "cmd/**/main.go", "cmd/memstore/main.go", true},
		{"doublestar both deep", "cmd/**/main.go", "cmd/a/b/main.go", true},
		{"doublestar all", "**", "anything/at/all", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := globMatch(tt.pattern, tt.path)
			if got != tt.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}
