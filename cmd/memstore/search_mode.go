package main

import (
	"fmt"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// newLocalEmbedder builds an embedder from the environment for local-mode
// hybrid search. Its error is returned rather than fatal: whether an
// unbuildable embedder is fatal depends on the search mode, which is
// resolveSearchMode's call to make.
func newLocalEmbedder() (embedding.Embedder, error) {
	cfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("embedder config: %w", err)
	}
	emb, err := embedding.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create embedder: %w", err)
	}
	return emb, nil
}

// Search arm selection for `memstore search`.
//
// The default is auto rather than a fixed arm because the right answer depends
// on how the CLI is configured, not on what the caller remembered to type.
// Against a daemon the vector search runs server-side and costs the client
// nothing, so keyword-only results there are strictly worse for no saving. In
// local mode it depends on whether an embedder can be built at all.
const (
	modeAuto   = "auto"
	modeHybrid = "hybrid"
	modeFTS    = "fts"
)

const searchModeUsage = "search arm: auto|hybrid|fts. " +
	"auto uses hybrid FTS+vector wherever it is available (always, against a daemon) " +
	"and falls back to fts locally when no embedder can be built. " +
	"hybrid forces it and fails if unavailable; fts forces keyword-only."

// resolveSearchMode decides which search arm to use.
//
// embErr is the result of trying to build a local embedder, and is consulted
// only in local mode -- against a daemon the embedder lives server-side, so a
// local embedder is never built and its configuration is irrelevant.
//
// The asymmetry between auto and hybrid is deliberate. Auto is a preference,
// so it degrades and returns a note explaining why. Hybrid is an instruction,
// so it fails: quietly returning keyword-only results to someone who asked for
// semantic search is how an empty result set gets mistaken for an empty
// corpus.
func resolveSearchMode(mode string, remote bool, embErr error) (hybrid bool, note string, err error) {
	switch mode {
	case modeFTS:
		return false, "", nil

	case modeHybrid:
		if !remote && embErr != nil {
			return false, "", fmt.Errorf(
				"--search %s needs a local embedder and none could be built: %w\n"+
					"Configure one (MEMSTORE_EMBED_*) or use --search %s for keyword-only search",
				modeHybrid, embErr, modeFTS)
		}
		return true, "", nil

	case modeAuto:
		if !remote && embErr != nil {
			return false, fmt.Sprintf(
				"no local embedder (%v); falling back to --search %s (keyword-only)", embErr, modeFTS), nil
		}
		return true, "", nil

	default:
		return false, "", fmt.Errorf("unknown --search mode %q: want %s, %s, or %s",
			mode, modeAuto, modeHybrid, modeFTS)
	}
}
