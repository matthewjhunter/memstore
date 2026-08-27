package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// runMCPHeaders prints the auth headers for the memstore MCP endpoint as a JSON
// object, for Claude Code's headersHelper.
//
// It exists so the token does not have to be copied anywhere. The alternatives
// both spread the credential: a static header puts it in ~/.claude.json, which
// is not a secrets file and carries session history besides, and a ${VAR}
// reference requires exporting it from a shell profile, which is a second
// plaintext copy in a file that is usually world-readable. The token is already
// in ~/.config/memstore/config.toml at 0600, and this reads it from there, so
// rotating it there is the whole rotation.
//
// Claude Code runs this fresh on every connection and after a 401, and gives it
// ten seconds. Reading one small TOML file is well inside that.
//
// Output contract: a JSON object of string keys to string values on stdout, and
// nothing else. An unconfigured token prints an empty object rather than
// failing -- a daemon running without auth is a legitimate deployment, and a
// helper that errored would make it look like a broken one.
func runMCPHeaders(args []string) {
	fs := flag.NewFlagSet("mcp-headers", flag.ExitOnError)
	fs.Parse(args)

	headers := map[string]string{}
	if cliConfig.APIKey != "" {
		headers["Authorization"] = "Bearer " + cliConfig.APIKey
	}
	out, err := json.Marshal(headers)
	if err != nil {
		// Unreachable for a map[string]string, but never print a partial
		// object: Claude Code would parse it as the full header set.
		fmt.Fprintf(os.Stderr, "memstore mcp-headers: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
