package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

// openInMemStore opens an in-memory SQLite store with a nil embedder for CLI tests.
func openInMemStore(t *testing.T) memstore.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := memstore.NewSQLiteStore(db, nil, "test")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return store
}

func TestWriteTasksText_empty(t *testing.T) {
	var buf bytes.Buffer
	writeTasksText(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("expected empty output for nil facts, got %q", buf.String())
	}

	buf.Reset()
	writeTasksText(&buf, []memstore.Fact{})
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty facts, got %q", buf.String())
	}
}

func TestWriteTasksText_tasks(t *testing.T) {
	meta := map[string]any{"priority": "high", "project": "myproject"}
	raw, _ := json.Marshal(meta)

	facts := []memstore.Fact{
		{ID: 1, Content: "Fix the login bug", Metadata: raw},
	}

	var buf bytes.Buffer
	writeTasksText(&buf, facts)
	out := buf.String()

	if !strings.Contains(out, "[MEMSTORE - Pending Tasks]") {
		t.Errorf("missing header in output: %q", out)
	}
	if !strings.Contains(out, "[high]") {
		t.Errorf("expected [high] prefix for high priority task: %q", out)
	}
	if !strings.Contains(out, "Fix the login bug") {
		t.Errorf("expected content in output: %q", out)
	}
	if !strings.Contains(out, "(project: myproject)") {
		t.Errorf("expected project suffix in output: %q", out)
	}
}

func TestWriteTasksText_normalPriority(t *testing.T) {
	meta := map[string]any{"priority": "normal"}
	raw, _ := json.Marshal(meta)

	facts := []memstore.Fact{
		{ID: 2, Content: "Do something", Metadata: raw},
	}

	var buf bytes.Buffer
	writeTasksText(&buf, facts)
	out := buf.String()

	if strings.Contains(out, "[high]") {
		t.Errorf("normal priority should not produce [high] prefix: %q", out)
	}
	if !strings.Contains(out, "Do something") {
		t.Errorf("expected content in output: %q", out)
	}
}

func TestWriteFactsText(t *testing.T) {
	facts := []memstore.Fact{
		{ID: 42, Subject: "matthew", Category: "preference", Content: "prefers dark mode"},
	}

	var buf bytes.Buffer
	writeFactsText(&buf, facts)
	out := buf.String()

	if !strings.Contains(out, "id=42") {
		t.Errorf("expected id in output: %q", out)
	}
	if !strings.Contains(out, "matthew") {
		t.Errorf("expected subject in output: %q", out)
	}
	if !strings.Contains(out, "preference") {
		t.Errorf("expected category in output: %q", out)
	}
	if !strings.Contains(out, "prefers dark mode") {
		t.Errorf("expected content in output: %q", out)
	}
}

func TestWriteJSON(t *testing.T) {
	facts := []memstore.Fact{
		{ID: 1, Subject: "test", Content: "hello"},
	}

	var buf bytes.Buffer
	if err := writeJSON(&buf, facts); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	var out []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, buf.String())
	}
	if len(out) != 1 {
		t.Errorf("expected 1 element, got %d", len(out))
	}
}

func TestOpenStore_notFound(t *testing.T) {
	store, close, err := openStore("/tmp/memstore-cli-test-nonexistent-db-xyz123.db", "default")
	if err != nil {
		t.Fatalf("expected nil error for missing DB, got: %v", err)
	}
	if store != nil {
		t.Error("expected nil store for missing DB")
		close()
	}
}

func TestRunStore_integration(t *testing.T) {
	ctx := t.Context()
	store := openInMemStore(t)

	id, err := store.Insert(ctx, memstore.Fact{
		Subject:  "test-subject",
		Content:  "integration test fact",
		Category: "note",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive id, got %d", id)
	}

	fact, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fact.Content != "integration test fact" {
		t.Errorf("expected content %q, got %q", "integration test fact", fact.Content)
	}
	if fact.Subject != "test-subject" {
		t.Errorf("expected subject %q, got %q", "test-subject", fact.Subject)
	}
}
