package memstore

import (
	"path"
	"strings"
)

// MatchFilePattern checks if filePath matches a trigger's glob pattern.
// Patterns are relative (e.g. "internal/feeds/**"). The function tries
// matching against successive suffixes of the absolute file path.
// Supports ** as "match any path segments".
func MatchFilePattern(pattern, filePath string) bool {
	pattern = path.Clean(pattern)
	filePath = path.Clean(filePath)

	parts := strings.Split(filePath, "/")
	for i := range parts {
		suffix := strings.Join(parts[i:], "/")
		if globMatch(pattern, suffix) {
			return true
		}
	}
	return false
}

// globMatch matches a pattern against a path string, supporting ** for
// matching zero or more directory segments.
func globMatch(pattern, name string) bool {
	if !strings.Contains(pattern, "**") {
		matched, _ := path.Match(pattern, name)
		return matched
	}

	prefix, suffix, _ := strings.Cut(pattern, "**")
	prefix = strings.TrimSuffix(prefix, "/")
	suffix = strings.TrimPrefix(suffix, "/")

	if prefix == "" && suffix == "" {
		return true
	}

	if prefix == "" {
		parts := strings.Split(name, "/")
		for i := range parts {
			tail := strings.Join(parts[i:], "/")
			if globMatch(suffix, tail) {
				return true
			}
		}
		return false
	}

	if suffix == "" {
		parts := strings.Split(name, "/")
		for i := 1; i <= len(parts); i++ {
			head := strings.Join(parts[:i], "/")
			matched, _ := path.Match(prefix, head)
			if matched {
				return true
			}
		}
		return false
	}

	parts := strings.Split(name, "/")
	for i := 1; i <= len(parts); i++ {
		head := strings.Join(parts[:i], "/")
		matched, _ := path.Match(prefix, head)
		if matched {
			for j := i; j <= len(parts); j++ {
				tail := strings.Join(parts[j:], "/")
				if globMatch(suffix, tail) {
					return true
				}
			}
		}
	}
	return false
}
