package main

import (
	"regexp"
	"strconv"
	"testing"
)

// The prompt hook aborts its own fetches with AbortSignal.timeout so a slow
// daemon yields empty context instead of the CLI killing the hook. That only
// works while every abort fires before the registered hook timeout, so the two
// numbers live in different files but must move together.
func TestPromptHookAbortsBeforeHookTimeout(t *testing.T) {
	var hookTimeoutSec int
	for _, reg := range hookRegistrations {
		if reg.Script == "memstore-prompt.mjs" {
			hookTimeoutSec = reg.Timeout
		}
	}
	if hookTimeoutSec == 0 {
		t.Fatal("memstore-prompt.mjs is not registered")
	}
	// Cold loads of the embedder and reranker on a lemonade backend take
	// several seconds each; 5 s was not enough to absorb one.
	if hookTimeoutSec < 10 {
		t.Errorf("prompt hook timeout = %ds, want >= 10s to absorb a cold model load", hookTimeoutSec)
	}

	src, err := hookFS.ReadFile("hooks/memstore-prompt.mjs")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`AbortSignal\.timeout\((\d+)\)`).FindAllSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("no AbortSignal.timeout found in memstore-prompt.mjs")
	}
	budgetMs := hookTimeoutSec * 1000
	maxMs := 0
	for _, m := range matches {
		ms, _ := strconv.Atoi(string(m[1]))
		if ms >= budgetMs {
			t.Errorf("AbortSignal.timeout(%d) does not fire before the %ds hook timeout", ms, hookTimeoutSec)
		}
		maxMs = max(maxMs, ms)
	}
	// The recall abort should use most of the budget, or raising the hook
	// timeout buys nothing.
	if maxMs < budgetMs-1000 {
		t.Errorf("longest abort %dms leaves more than 1s of the %ds hook budget unused", maxMs, hookTimeoutSec)
	}
}
