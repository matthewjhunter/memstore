package chunk_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNoLLMReachableFromChunkers enforces the no-LLM-in-ingest pillar from
// docs/document-corpus.md as an import-graph property: nothing in the chunk
// package tree may reach an LLM client or the network. The root memstore
// package carries OpenAIGenerator, so it is forbidden outright; so is
// anything that could open a connection. If this test fails, the fix is to
// move the offending code out of the chunkers, not to widen the list.
func TestNoLLMReachableFromChunkers(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	// Exact package matches.
	forbidden := map[string]string{
		"github.com/matthewjhunter/memstore": "the root package carries an LLM client (OpenAIGenerator)",
		"net":                                "chunkers must not be able to open connections",
		"net/http":                           "chunkers must not be able to speak HTTP",
		"os/exec":                            "chunkers must not run subprocesses",
	}
	// Prefix matches for whole dependency trees.
	forbiddenPrefixes := map[string]string{
		"github.com/openai/":                     "LLM client library",
		"github.com/matthewjhunter/go-embedding": "embedding client reaches the network",
	}

	for _, dep := range strings.Fields(string(out)) {
		if reason, ok := forbidden[dep]; ok {
			t.Errorf("chunk package tree imports %s: %s", dep, reason)
		}
		for prefix, reason := range forbiddenPrefixes {
			if strings.HasPrefix(dep, prefix) {
				t.Errorf("chunk package tree imports %s: %s", dep, reason)
			}
		}
	}
}
