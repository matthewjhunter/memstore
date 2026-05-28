package main

import (
	"fmt"

	"github.com/matthewjhunter/memstore"
)

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
