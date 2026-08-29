package main

import "fmt"

// Search arm selection for `memstore search`.
//
// The daemon runs the vector arm server-side, so hybrid costs the client
// nothing and is the default; fts is the explicit keyword-only choice.
const (
	modeAuto   = "auto"
	modeHybrid = "hybrid"
	modeFTS    = "fts"
)

const searchModeUsage = "search arm: auto|hybrid|fts. " +
	"auto and hybrid use FTS+vector search on the daemon; fts forces keyword-only."

// resolveSearchMode decides which search arm to use. auto and hybrid are the
// same request against a daemon; they stay distinct names so a script that
// asked for hybrid explicitly keeps working.
func resolveSearchMode(mode string) (hybrid bool, err error) {
	switch mode {
	case modeFTS:
		return false, nil
	case modeHybrid, modeAuto:
		return true, nil
	default:
		return false, fmt.Errorf("unknown --search mode %q: want %s, %s, or %s",
			mode, modeAuto, modeHybrid, modeFTS)
	}
}
