package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

// Recall returns a notice listing each recalled fact by id, with stored text
// cleaned of anything a terminal acts on: the prompt hook prints it as is.
func TestRecallNotice(t *testing.T) {
	h, store, _ := newTestHandlerWithRecall(t)
	seedFacts(t, store)
	id, err := store.Insert(context.Background(), memstore.Fact{
		Content:  "Quokka deploys run from the ansible directory\x1b[2J\xe2\x80\xae",
		Subject:  "quokka",
		Category: "project",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{"prompt": "where do quokka deploys run"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recall: status %d", resp.StatusCode)
	}
	var r struct {
		Notice string `json:"notice"`
	}
	decodeJSON(t, resp, &r)

	if !strings.HasPrefix(r.Notice, "memstore: recalled for this prompt\n") {
		t.Errorf("notice header:\n%s", r.Notice)
	}
	if want := fmt.Sprintf("[fact %d] Quokka deploys run from the ansible directory[2J", id); !strings.Contains(r.Notice, want) {
		t.Errorf("notice missing %q:\n%q", want, r.Notice)
	}
	if strings.ContainsAny(r.Notice, "\x1b\u202e") {
		t.Errorf("notice carries terminal control characters: %q", r.Notice)
	}
}

// Rendered hints come with a notice listing each hint shown, by hint id.
func TestHintRenderNotice(t *testing.T) {
	ss := &hintSessionStore{hints: []memstore.ContextHint{
		{ID: 41, CWD: "/work/repo", HintText: "The session was tracing a token refresh bug.\x1b[2J", CreatedAt: time.Now()},
	}}
	code, out := getRenderedHints(t, ss, "session_id=s-1&cwd=/work/repo")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	want := "memstore: session notes for this prompt\n  [hint 41] The session was tracing a token refresh bug.[2J"
	if out.Notice != want {
		t.Errorf("notice = %q, want %q", out.Notice, want)
	}
}
