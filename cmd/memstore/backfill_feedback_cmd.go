package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// The command takes no flags. It still accepts args so the dispatch in main.go
// stays uniform, and rejects them rather than ignoring them: silently
// discarding `--pg ...` would leave the caller believing an option took effect.
func runBackfillFeedback(args []string) {
	if len(args) > 0 {
		log.Fatalf("backfill-feedback takes no arguments, got %q", strings.Join(args, " "))
	}
	if cliConfig.Remote == "" {
		log.Fatal("backfill-feedback requires a remote memstored (set remote in config)")
	}

	client, err := newRemoteClient()
	if err != nil {
		log.Fatalf("backfill-feedback: %v", err)
	}

	fmt.Println("Backfilling fact feedback scores from historical sessions...")
	fmt.Println("This sends one LLM call per session -- may take several minutes.")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	result, err := client.BackfillFeedback(ctx)
	if err != nil {
		log.Fatalf("backfill-feedback: %v", err)
	}

	fmt.Printf("Done: %d sessions processed, %d fact ratings written, %d errors\n",
		result.Sessions, result.Rated, result.Errors)
}
