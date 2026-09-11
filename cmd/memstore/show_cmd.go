package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/matthewjhunter/memstore"
)

// runShow prints one fact by id: the lookup for the ids a hook notice lists.
func runShow(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: memstore show <id>")
		os.Exit(2)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(os.Stderr, "show: %q is not a fact id\n", args[0])
		os.Exit(2)
	}
	store, closeStore, err := openStore()
	if err != nil {
		log.Fatal(err)
	}
	defer closeStore()
	f, err := store.Get(context.Background(), id)
	if err != nil {
		log.Fatalf("show: %v", err)
	}
	if f == nil {
		fmt.Fprintf(os.Stderr, "show: no fact %d\n", id)
		os.Exit(1)
	}
	writeFactDetail(os.Stdout, *f)
}

// writeFactDetail prints a fact for a person at a terminal. Stored text is
// printed with control and format characters removed, so a fact cannot drive
// the terminal it is shown on.
func writeFactDetail(w io.Writer, f memstore.Fact) {
	clean := memstore.SanitizeTerminalText
	fmt.Fprintf(w, "[id=%d] %s | %s | %s", f.ID, clean(f.Subject), clean(f.Category), f.CreatedAt.Format("2006-01-02"))
	if f.Kind != "" {
		fmt.Fprintf(w, " | kind=%s", clean(f.Kind))
	}
	if f.Subsystem != "" {
		fmt.Fprintf(w, " | subsystem=%s", clean(f.Subsystem))
	}
	fmt.Fprintln(w)
	if f.SupersededBy != nil {
		fmt.Fprintf(w, "superseded by %d\n", *f.SupersededBy)
	}
	for line := range strings.SplitSeq(clean(f.Content), "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
}
