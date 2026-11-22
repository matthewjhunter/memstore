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
	budgetMs := hookTimeoutSec * 1000

	// The overall deadline bounds stdin, CLI spawns and both daemon calls. What
	// is left of the hook timeout must cover node startup and the notices
	// spawn (1.5s max) that runs after it, or the harness kills the hook and
	// discards the output the deadline was meant to save.
	dm := regexp.MustCompile(`DEADLINE_MS = Number\(process\.env\.MEMSTORE_HOOK_DEADLINE_MS\) \|\| (\d+)`).FindSubmatch(src)
	if dm == nil {
		t.Fatal("no DEADLINE_MS default found in memstore-prompt.mjs")
	}
	deadlineMs, _ := strconv.Atoi(string(dm[1]))
	if deadlineMs > budgetMs-2000 {
		t.Errorf("DEADLINE_MS = %d leaves under 2s of the %ds hook timeout for startup and the notices spawn", deadlineMs, hookTimeoutSec)
	}
	if deadlineMs < budgetMs-4000 {
		t.Errorf("DEADLINE_MS = %d leaves more than 4s of the %ds hook timeout unused", deadlineMs, hookTimeoutSec)
	}

	matches := regexp.MustCompile(`AbortSignal\.timeout\((\d+)\)`).FindAllSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("no AbortSignal.timeout found in memstore-prompt.mjs")
	}
	for _, m := range matches {
		ms, _ := strconv.Atoi(string(m[1]))
		if ms >= deadlineMs {
			t.Errorf("AbortSignal.timeout(%d) does not fire before the %dms deadline", ms, deadlineMs)
		}
	}
}
