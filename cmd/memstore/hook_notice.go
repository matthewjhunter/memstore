package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/matthewjhunter/memstore"
)

// hookOutput is what `--format hook` prints: the block for the model's context
// and the notice for the user. The hooks pass them on as additionalContext and
// systemMessage.
type hookOutput struct {
	Context string `json:"context"`
	Notice  string `json:"notice,omitempty"`
}

// writeHookOutput prints one hookOutput, with the notice only when notices are
// on. With no context it prints nothing, which the hooks read as "nothing to
// inject".
func writeHookOutput(w io.Writer, context, notice string, notices bool) error {
	if context == "" {
		return nil
	}
	out := hookOutput{Context: context}
	if notices {
		out.Notice = notice
	}
	return json.NewEncoder(w).Encode(out)
}

// triggerNotice lists each trigger and fact the file context carries.
func triggerNotice(filePath string, groups []triggerGroup) string {
	var items []memstore.NoticeItem
	for _, g := range groups {
		items = append(items, memstore.NoticeItem{Kind: "fact", ID: g.trigger.ID, Text: "trigger: " + g.trigger.Content})
		for _, f := range g.facts {
			items = append(items, memstore.NoticeItem{Kind: "fact", ID: f.ID, Text: f.Content})
		}
	}
	return memstore.FormatNotice("file context for "+filepath.Base(filePath), items)
}

// tasksNotice lists the tasks the startup list carries, by title.
func tasksNotice(project string, tasks []memstore.Fact) string {
	items := make([]memstore.NoticeItem, len(tasks))
	for i, f := range tasks {
		items[i] = memstore.NoticeItem{Kind: "task", ID: f.ID, Text: memstore.TaskTitle(f.Content)}
	}
	return memstore.FormatNotice("open tasks for "+project, items)
}

// runHookNotices prints "on" or "off": whether hooks show notices. The prompt
// hook asks, because it talks to the daemon directly and this keeps one parser
// for config.toml.
func runHookNotices(_ []string) {
	if cliConfig.HookNotices {
		fmt.Println("on")
	} else {
		fmt.Println("off")
	}
}
