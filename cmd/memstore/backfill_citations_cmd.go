package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// runBackfillCitations asks the daemon to read the [fact N] citations already
// in the caller's recorded sessions: the history from before the daemon read
// them on transcript upload (docs/citation-feedback.md, step 2). Safe to run
// again; citations already recorded are skipped.
func runBackfillCitations(args []string) {
	if len(args) > 0 {
		log.Fatalf("backfill-citations takes no arguments, got %q", strings.Join(args, " "))
	}
	if cliConfig.Remote == "" {
		log.Fatal("backfill-citations requires a remote memstored (set remote in config)")
	}

	client, err := newRemoteClient()
	if err != nil {
		log.Fatalf("backfill-citations: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := client.BackfillCitations(ctx)
	if err != nil {
		log.Fatalf("backfill-citations: %v", err)
	}

	fmt.Printf("Done: %d sessions read, %d citations found, %d recorded, %d rejected (no active fact with that id)\n",
		result.Sessions, result.Found, result.Recorded, result.Rejected)
}
