package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
)

// The file-context notice names the file and lists every trigger and fact the
// block carries, by id, so the user can look each one up.
func TestTriggerNotice(t *testing.T) {
	got := triggerNotice("/w/memstore/pgstore/store.go", triggerGroups())
	if !strings.HasPrefix(got, "memstore: file context for store.go\n") {
		t.Errorf("header:\n%s", got)
	}
	for _, want := range []string{"[fact 40] trigger: Editing pgstore", "[fact 41] scanFact", "[fact 42] ", "[fact 50] trigger: ", "[fact 51] "} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
	if triggerNotice("/w/x.go", nil) != "" {
		t.Error("no groups should give no notice")
	}
}

func TestTasksNotice(t *testing.T) {
	tasks := []memstore.Fact{
		taskFact(12, "Fix recall auth. The long design notes that follow.", map[string]string{"status": "pending"}),
		taskFact(13, "Rotate keys", map[string]string{"status": "in_progress"}),
	}
	got := tasksNotice("memstore", tasks)
	want := "memstore: open tasks for memstore\n  [task 12] Fix recall auth.\n  [task 13] Rotate keys"
	if got != want {
		t.Errorf("notice =\n%s\nwant\n%s", got, want)
	}
	if tasksNotice("memstore", nil) != "" {
		t.Error("no tasks should give no notice")
	}
}

// --format hook is one JSON object: the block for the model, and the notice
// for the user when notices are on. With nothing to inject it prints nothing,
// which the hooks already read as "no context".
func TestWriteHookOutput(t *testing.T) {
	decode := func(b []byte) map[string]string {
		t.Helper()
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("not JSON: %q: %v", b, err)
		}
		return m
	}
	var buf bytes.Buffer
	if err := writeHookOutput(&buf, "CTX", "NOTE", true); err != nil {
		t.Fatal(err)
	}
	if m := decode(buf.Bytes()); m["context"] != "CTX" || m["notice"] != "NOTE" {
		t.Errorf("notices on: %v", m)
	}

	buf.Reset()
	if err := writeHookOutput(&buf, "CTX", "NOTE", false); err != nil {
		t.Fatal(err)
	}
	if m := decode(buf.Bytes()); m["context"] != "CTX" || m["notice"] != "" {
		t.Errorf("notices off: %v", m)
	}

	buf.Reset()
	if err := writeHookOutput(&buf, "", "NOTE", true); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing to inject should print nothing, got %q", buf.String())
	}
}
