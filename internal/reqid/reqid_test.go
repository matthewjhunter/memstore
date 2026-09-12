package reqid_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/matthewjhunter/memstore/internal/reqid"
)

func TestNewIsUniqueAndOpaque(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := reqid.New()
		if !shape.MatchString(id) {
			t.Fatalf("New() = %q, want 16 hex characters", id)
		}
		if seen[id] {
			t.Fatalf("New() repeated %q within 1000 calls", id)
		}
		seen[id] = true
	}
}

func TestContextRoundTrip(t *testing.T) {
	if got := reqid.FromContext(context.Background()); got != "" {
		t.Errorf("a context with no id should report %q, got %q", "", got)
	}
	ctx := reqid.NewContext(context.Background(), "abc123")
	if got := reqid.FromContext(ctx); got != "abc123" {
		t.Errorf("FromContext = %q, want %q", got, "abc123")
	}
}
