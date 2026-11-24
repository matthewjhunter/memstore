package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"
)

// runBackfillSessions asks the daemon to give sessions it holds as text only
// -- everything uploaded before raw transcripts were kept -- the session and
// compaction records a raw upload leaves (docs/session-archive.md, step 5).
// A compaction summary survives in the text archive as an ordinary turn, and
// that is what is recovered. Safe to run again: a session already recorded
// is skipped.
func runBackfillSessions(args []string) {
	fs := flag.NewFlagSet("backfill-sessions", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "count what would be recorded and write nothing")
	fs.Parse(args)
	if fs.NArg() > 0 {
		log.Fatalf("backfill-sessions takes no arguments, got %q", fs.Args())
	}
	if cliConfig.Remote == "" {
		log.Fatal("backfill-sessions requires a remote memstored (set remote in config)")
	}
	client, err := newRemoteClient()
	if err != nil {
		log.Fatalf("backfill-sessions: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := client.BackfillSessionArchive(ctx, *dryRun)
	if err != nil {
		log.Fatalf("backfill-sessions: %v", err)
	}
	if res.DryRun {
		fmt.Printf("%d text-only sessions read; %d hold a compaction summary, %d summaries in all; dry run, nothing written\n",
			res.Sessions, res.Compacted, res.Compactions)
		return
	}
	fmt.Printf("%d text-only sessions read; %d hold a compaction summary, %d summaries in all; %d sessions recorded; %d facts refused by write screening; %d errors\n",
		res.Sessions, res.Compacted, res.Compactions, res.Written, res.Screened, res.Errors)
}
