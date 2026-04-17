package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/matthewjhunter/memstore"
)

// resolveAbsPath resolves p to an absolute path, relative to the current working
// directory if p is relative.
func resolveAbsPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// printContextFact prints a fact in the hook context format.
func printContextFact(f memstore.Fact) {
	fmt.Printf("[id=%d] %s | %s", f.ID, f.Subject, f.Category)
	if f.Kind != "" {
		fmt.Printf(" | kind=%s", f.Kind)
	}
	if f.Subsystem != "" {
		fmt.Printf(" | subsystem=%s", f.Subsystem)
	}
	fmt.Println()
	fmt.Printf("  %s\n", f.Content)
	fmt.Println()
}
