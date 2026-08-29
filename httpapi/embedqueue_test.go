package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/teststore"
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
	emb := &poisonEmbedder{dim: 4, poison: "POISON"}
	store := teststore.New(t, emb, "test")
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
	emb := &permanentEmbedder{dim: 4, marker: "UNEMBEDDABLE"}
	store := teststore.New(t, emb, "test")
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

// llamaServer fakes a llama-server (Lemonade) embed endpoint behind the real
// OpenAI-compatible client: any input over fitsAt bytes is rejected with the
// HTTP 500 "input (N tokens) is too large to process" body llama-server
// returns when a request exceeds its physical batch size. Anything smaller
// gets a vector of the dimension the test store was opened with.
func llamaServer(t *testing.T, fitsAt int, dim int) (*httptest.Server, *llamaCounts) {
	t.Helper()
	counts := &llamaCounts{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts.requests++
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, in := range req.Input {
			if len(in) > fitsAt {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":500,"message":"input (1234 tokens) is too large to process. increase the physical batch size","type":"server_error"}}`))
				return
			}
		}
		counts.accepted++
		vec := make([]float32, dim)
		for i := range vec {
			vec[i] = 0.1
		}
		type datum struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		data := make([]datum, len(req.Input))
		for i := range data {
			data[i] = datum{Embedding: vec, Index: i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, counts
}

type llamaCounts struct{ requests, accepted int }

func openAIEmbedderFor(t *testing.T, srv *httptest.Server) embedding.Embedder {
	t.Helper()
	e, err := embedding.New(embedding.Config{
		Backend: embedding.BackendOpenAI, BaseURL: srv.URL, Model: "embeddinggemma",
	})
	if err != nil {
		t.Fatalf("embedding.New: %v", err)
	}
	return e
}

// TestEmbedQueue_LlamaServer500TooLarge_ShrinksAndEmbeds is the 2026-08-28
// production case: llama-server rejected 14 facts with HTTP 500 rather than
// 4xx, the client treated that as transient, and the queue re-attempted them
// on every pass. The rejection must now drive the adaptive shrink so the
// fact ends up embedded.
func TestEmbedQueue_LlamaServer500TooLarge_ShrinksAndEmbeds(t *testing.T) {
	srv, counts := llamaServer(t, 700, teststore.VecDim)
	emb := openAIEmbedderFor(t, srv)
	store := teststore.New(t, emb, "test")
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content: strings.Repeat("dense identifier text ", 60), Subject: "big", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	q := httpapi.NewEmbedQueue(store, emb, 0, 32)
	q.ProcessOnce()

	pending, err := store.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("NeedingEmbedding: %v", err)
	}
	for _, f := range pending {
		if f.ID == id {
			t.Fatalf("fact still unembedded after %d requests; the 500 was retried instead of shrunk", counts.requests)
		}
	}
	// Absent from the queue could also mean quarantined; an accepted request
	// proves the shrunk input was actually embedded.
	if counts.accepted == 0 {
		t.Fatalf("fact left the queue without an accepted embed request (%d requests): quarantined instead of shrunk", counts.requests)
	}
	if counts.requests < 2 {
		t.Errorf("expected a reject followed by an accepted request, got %d", counts.requests)
	}
}

// TestEmbedQueue_LlamaServer500TooLarge_QuarantinesWhenNeverFits covers the
// other half: when shrinking reaches the floor and the backend still says
// too large, the fact is quarantined instead of being retried on every pass.
func TestEmbedQueue_LlamaServer500TooLarge_QuarantinesWhenNeverFits(t *testing.T) {
	srv, counts := llamaServer(t, 0, teststore.VecDim)
	emb := openAIEmbedderFor(t, srv)
	store := teststore.New(t, emb, "test")
	ctx := context.Background()

	id, err := store.Insert(ctx, memstore.Fact{
		Content: strings.Repeat("dense identifier text ", 60), Subject: "big", Category: "test",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	q := httpapi.NewEmbedQueue(store, emb, 0, 32)
	q.ProcessOnce()
	after := counts.requests

	pending, err := store.NeedingEmbedding(ctx, 100)
	if err != nil {
		t.Fatalf("NeedingEmbedding: %v", err)
	}
	for _, f := range pending {
		if f.ID == id {
			t.Fatal("never-fitting fact still returned by NeedingEmbedding; it was not quarantined")
		}
	}

	q.ProcessOnce()
	if counts.requests != after {
		t.Errorf("quarantined fact was re-attempted: %d more requests", counts.requests-after)
	}
}
