package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/matthewjhunter/memstore/httpclient"
)

func runBackfillFeedback(args []string) {
	if cliConfig.Remote == "" {
		log.Fatal("backfill-feedback requires a remote memstored (set remote in config)")
	}

	client := httpclient.New(cliConfig.Remote, cliConfig.APIKey)

	fmt.Println("Backfilling fact feedback scores from historical sessions...")
	fmt.Println("This sends one LLM call per session — may take several minutes.")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	result, err := client.BackfillFeedback(ctx)
	if err != nil {
		log.Fatalf("backfill-feedback: %v", err)
	}

	fmt.Printf("Done: %d sessions processed, %d fact ratings written, %d errors\n",
		result.Sessions, result.Rated, result.Errors)
}
