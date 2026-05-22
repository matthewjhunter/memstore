package httpapi_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	_ "modernc.org/sqlite"
)

// poisonEmbedder fails on any input containing the poison marker, succeeds
// otherwise. Mirrors the real "input length exceeds context length" failure
// mode but without needing a real LLM.
type poisonEmbedder struct {
	dim    int
	poison string
}

func (p *poisonEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	for _, t := range texts {
		if strings.Contains(t, p.poison) {
			return nil, errors.New("input length exceeds context length")
		}
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, p.dim)
		for j := range out[i] {
			out[i][j] = 0.1
		}
	}
	return out, nil
}

func (p *poisonEmbedder) Model() string { return "poison" }

func (p *poisonEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "poison", Dim: p.dim}
}

// TestEmbedQueue_PoisonPillDoesNotBlockOthers verifies the queue keeps making
// progress on healthy facts even when one fact's embed call fails — the
// regression we hit in prod where a single oversized fact stalled the entire
// queue forever.
func TestEmbedQueue_PoisonPillDoesNotBlockOthers(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	emb := &poisonEmbedder{dim: 4, poison: "POISON"}
	store, err := memstore.NewSQLiteStore(db, emb, "test")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()

	// Insert poison fact first so it sits at the head of the queue (NeedingEmbedding orders by id).
	poisonID, err := store.Insert(ctx, memstore.Fact{
		Content: "POISON content", Subject: "bad", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert poison: %v", err)
	}
	healthyID, err := store.Insert(ctx, memstore.Fact{
		Content: "healthy content", Subject: "good", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert healthy: %v", err)
	}

	q := httpapi.NewEmbedQueue(store, emb, 0, 32)
	q.ProcessOnce()

	healthy, err := store.Get(ctx, healthyID)
	if err != nil {
		t.Fatalf("Get healthy: %v", err)
	}
	if len(healthy.Embedding) == 0 {
		t.Error("healthy fact has no embedding — queue stalled on poison")
	}

	poisoned, err := store.Get(ctx, poisonID)
	if err != nil {
		t.Fatalf("Get poison: %v", err)
	}
	if len(poisoned.Embedding) != 0 {
		t.Error("poison fact unexpectedly got an embedding")
	}
}

// permanentEmbedder fails permanently on any input containing the marker. It
// counts attempts so the test can prove a quarantined fact is not re-embedded.
type permanentEmbedder struct {
	dim      int
	marker   string
	attempts map[string]int
}

func (p *permanentEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if p.attempts == nil {
		p.attempts = map[string]int{}
	}
	for _, t := range texts {
		if strings.Contains(t, p.marker) {
			p.attempts[p.marker]++
			return nil, &embedding.PermanentError{
				Err:     errors.New("input exceeds the maximum context length"),
				TooLong: true,
			}
		}
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, p.dim)
	}
	return out, nil
}

func (p *permanentEmbedder) Model() string { return "perm" }
func (p *permanentEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "perm", Dim: p.dim}
}

// TestEmbedQueue_QuarantinesPermanentFailure verifies a fact whose embed fails
// permanently is marked and stops being re-fetched — the fix for the 46k-error
// retry loop where over-length facts were re-attempted every poll forever.
func TestEmbedQueue_QuarantinesPermanentFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	emb := &permanentEmbedder{dim: 4, marker: "UNEMBEDDABLE"}
	store, err := memstore.NewSQLiteStore(db, emb, "test")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()

	badID, err := store.Insert(ctx, memstore.Fact{
		Content: "UNEMBEDDABLE content", Subject: "bad", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// First poll: the fact fails permanently and should be quarantined.
	q := httpapi.NewEmbedQueue(store, emb, 0, 32)
	q.ProcessOnce()

	if got := emb.attempts[emb.marker]; got != 1 {
		t.Fatalf("expected 1 embed attempt on first poll, got %d", got)
	}

	// It must no longer surface as needing embedding.
	pending, err := store.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("NeedingEmbedding: %v", err)
	}
	for _, f := range pending {
		if f.ID == badID {
			t.Fatal("quarantined fact still returned by NeedingEmbedding")
		}
	}

	// A second poll must not re-attempt it — this is the loop that produced
	// 46k errors over three days.
	q.ProcessOnce()
	if got := emb.attempts[emb.marker]; got != 1 {
		t.Errorf("quarantined fact was re-embedded: %d attempts, want 1", got)
	}
}
