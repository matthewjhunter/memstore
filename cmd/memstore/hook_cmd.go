package main

import (
	"flag"

	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/internal/hookcapture"
)

// runHook handles Claude Code's Stop hook.
//
// It reads the hook payload from stdin, archives it, tracks per-session state,
// and drains one pending transcript upload. The work is in internal/hookcapture;
// this is the command that names it.
//
// It moved here from `memstore-mcp --hook`, which had made the Stop hook the
// last thing on a machine that needed the MCP binary installed locally. Nothing
// about hook capture was ever MCP: it is an HTTP client posting to the daemon,
// which is what this CLI already is.
func runHook(args []string) {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	remote := fs.String("remote", cliConfig.Remote, "memstored URL (empty = do nothing)")
	transcript := fs.String("transcript", "", "upload this JSONL transcript and exit, instead of reading a hook event from stdin")
	fs.Parse(args)

	opts := hookcapture.Options{
		Remote:  *remote,
		APIKey:  cliConfig.APIKey,
		TLS:     httpclient.ClientOptionsFromConfig(cliConfig),
		Respawn: []string{"hook", "--transcript"},
	}
	if *transcript != "" {
		opts.RunTranscript(*transcript)
		return
	}
	opts.Run()
}
