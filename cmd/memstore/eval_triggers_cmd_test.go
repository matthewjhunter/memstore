package main

import "testing"

func TestMatchFilePattern(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		filePath string
		want     bool
	}{
		{
			name:     "exact suffix match",
			pattern:  "internal/feeds/fetcher.go",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     true,
		},
		{
			name:     "double star at end",
			pattern:  "internal/feeds/**",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     true,
		},
		{
			name:     "double star nested",
			pattern:  "internal/feeds/**",
			filePath: "/home/user/go/src/project/internal/feeds/sub/deep.go",
			want:     true,
		},
		{
			name:     "double star with suffix",
			pattern:  "internal/**/*.go",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     true,
		},
		{
			name:     "single star in filename",
			pattern:  "internal/feeds/*.go",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     true,
		},
		{
			name:     "no match wrong directory",
			pattern:  "internal/auth/**",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     false,
		},
		{
			name:     "no match wrong extension",
			pattern:  "internal/feeds/*.py",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     false,
		},
		{
			name:     "double star at start",
			pattern:  "**/*.go",
			filePath: "/home/user/go/src/project/internal/feeds/fetcher.go",
			want:     true,
		},
		{
			name:     "double star only",
			pattern:  "**",
			filePath: "/home/user/go/src/project/anything.go",
			want:     true,
		},
		{
			name:     "simple filename",
			pattern:  "Makefile",
			filePath: "/home/user/project/Makefile",
			want:     true,
		},
		{
			name:     "cmd subpackage pattern",
			pattern:  "cmd/**",
			filePath: "/home/user/go/src/project/cmd/memstore/main.go",
			want:     true,
		},
		{
			name:     "relative path exact",
			pattern:  "cmd/memstore/main.go",
			filePath: "/home/user/go/src/project/cmd/memstore/main.go",
			want:     true,
		},
		{
			name:     "double star middle with extension filter",
			pattern:  "cmd/**/main.go",
			filePath: "/home/user/go/src/project/cmd/memstore/main.go",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchFilePattern(tt.pattern, tt.filePath)
			if got != tt.want {
				t.Errorf("matchFilePattern(%q, %q) = %v, want %v", tt.pattern, tt.filePath, got, tt.want)
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
