package main

// memstore docs search: the CLI half of the corpus read path.
//
// Parity with `memstore search`, which searches facts. The two stay separate
// commands because they search separate indexes and the results mean
// different things: a fact is a claim, a chunk is the verbatim material a
// claim can be checked against.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/matthewjhunter/memstore/httpclient"
)

func runDocs(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: memstore docs search --query <text> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "search":
		runDocsSearch(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown docs subcommand: %q (want: search)\n", args[0])
		os.Exit(2)
	}
}

func runDocsSearch(args []string) {
	fs := flag.NewFlagSet("docs search", flag.ExitOnError)
	query := fs.String("query", "", "search text (required)")
	limit := fs.Int("limit", 10, "maximum chunks to return")
	repoURL := fs.String("repo", "", "restrict to one repo identity")
	prefix := fs.String("path-prefix", "", "restrict to documents under this path prefix")
	basename := fs.String("basename", "", "restrict to documents with this exact basename")
	lang := fs.String("lang", "", "restrict to one language (markdown, go, ...)")
	format := fs.String("format", "text", "output format: text|json")
	fs.Parse(args)

	if strings.TrimSpace(*query) == "" {
		fmt.Fprintln(os.Stderr, "docs search: --query is required")
		os.Exit(2)
	}

	client, err := newRemoteClient()
	if err != nil || cliConfig.Remote == "" {
		log.Fatalf("docs search: needs a daemon (set remote in config.toml or MEMSTORE_REMOTE)")
	}
	res, err := client.SearchDocuments(context.Background(), httpclient.DocSearchRequest{
		Query: *query, MaxResults: *limit, RepoURL: *repoURL,
		PathPrefix: *prefix, Basename: *basename, Lang: *lang,
	})
	if err != nil {
		log.Fatalf("docs search: %v", err)
	}

	if *format == "json" {
		if err := writeJSON(os.Stdout, res); err != nil {
			log.Fatalf("docs search: %v", err)
		}
		return
	}
	if len(res.Results) == 0 {
		fmt.Println("no documents matched")
		return
	}
	for i, r := range res.Results {
		// The trust marker is not decoration: everything ingested from a URL
		// is untrusted regardless of who ingested it, so most of this corpus
		// is material that should be read as data.
		trust := "trusted"
		if !r.Trusted {
			trust = "UNTRUSTED"
		}
		fmt.Printf("[%d] chunk=%d score=%.3f %s\n    %s\n", i+1, r.Chunk.ID, r.Score, trust, r.Citation())
		for _, line := range strings.Split(strings.TrimRight(r.Chunk.Content, "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}
}
