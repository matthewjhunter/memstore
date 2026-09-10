package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/teststore"
)

// topicEmbedder maps text to one of four orthogonal unit vectors by keyword,
// so the cosine between two texts is exactly 1 (same topic) or 0. It records
// what it was asked to embed.
type topicEmbedder struct {
	fail  bool
	texts []string
}

func (e *topicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.texts = append(e.texts, texts...)
	if e.fail {
		return nil, errors.New("embedder down")
	}
	out := make([][]float32, len(texts))
	for i, s := range texts {
		v := make([]float32, 4)
		switch {
		case strings.Contains(s, "dice"):
			v[0] = 1
		case strings.Contains(s, "deploy"):
			v[1] = 1
		case strings.Contains(s, "herald"):
			v[2] = 1
		default:
			v[3] = 1
		}
		out[i] = v
	}
	return out, nil
}

func (e *topicEmbedder) Model() string { return "topic" }

func (e *topicEmbedder) Fingerprint() embedding.Fingerprint {
	return embedding.Fingerprint{Model: "topic", Dim: 4}
}

func postRenderedHints(t *testing.T, ss *hintSessionStore, emb embedding.Embedder, body map[string]any, opts ...httpapi.HandlerOpt) (int, renderedHints) {
	t.Helper()
	store := teststore.New(t, &mockEmbedder{dim: 4}, "test")
	h := httpapi.New(store, emb, "", append([]httpapi.HandlerOpt{httpapi.WithSessionStore(ss)}, opts...)...)
	resp := doJSON(t, h, "POST", "/v1/context/hints/render", body)
	var out renderedHints
	if resp.StatusCode == http.StatusOK {
		decodeJSON(t, resp, &out)
	} else {
		resp.Body.Close()
	}
	return resp.StatusCode, out
}

func gateHints() []memstore.ContextHint {
	now := time.Now()
	return []memstore.ContextHint{
		{ID: 1, CWD: "/w/osg", HintText: "The deploy of the promotion task was pending.", CreatedAt: now},
		{ID: 2, CWD: "/w/osg", HintText: "The dice parser rejected modifiers on 3d6+2.", CreatedAt: now},
		{ID: 3, CWD: "/w/osg", HintText: "herald feed polling was rescheduled.", CreatedAt: now},
	}
}

// TestHintGateKeepsOnlyRelevantHints pins option 2 from #221: a hint reaches the
// prompt only when it is about what the prompt is about. The misfires in #221
// were accurate notes about the wrong work; relevance to the prompt is the check
// that stops them.
func TestHintGateKeepsOnlyRelevantHints(t *testing.T) {
	emb := &topicEmbedder{}
	code, out := postRenderedHints(t, &hintSessionStore{hints: gateHints()}, emb, map[string]any{
		"session_id": "s-1", "cwd": "/w/osg", "prompt": "why does the dice parser reject 3d6+2 now",
	})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(out.IDs) != 1 || out.IDs[0] != 2 {
		t.Errorf("ids = %v, want [2]: only the hint about the prompt's topic", out.IDs)
	}
	if !strings.Contains(out.Context, "dice parser") || strings.Contains(out.Context, "deploy") {
		t.Errorf("context carries the wrong hints:\n%s", out.Context)
	}

	// The prompt is embedded as a query and the hints as documents, the same
	// asymmetric pairing recall uses; mixing the two sides degrades scores
	// silently.
	if len(emb.texts) == 0 || emb.texts[0] != memstore.FactQueryText("topic", "why does the dice parser reject 3d6+2 now") {
		t.Errorf("prompt was not embedded as a query: %q", emb.texts)
	}
	wantDoc := memstore.FactEmbedText("topic", "", "The dice parser rejected modifiers on 3d6+2.")
	found := false
	for _, s := range emb.texts[1:] {
		found = found || s == wantDoc
	}
	if !found {
		t.Errorf("hints were not embedded as documents: %q", emb.texts)
	}
}

func TestHintGateRanksBySimilarity(t *testing.T) {
	code, out := postRenderedHints(t, &hintSessionStore{hints: gateHints()}, &topicEmbedder{}, map[string]any{
		"cwd": "/w/osg", "prompt": "the dice parser is still failing on modifiers", "limit": 2,
	}, httpapi.WithHintMinSimilarity(0))
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(out.IDs) != 2 || out.IDs[0] != 2 {
		t.Errorf("ids = %v, want the dice hint first of two", out.IDs)
	}
}

// Every failure shows nothing: a wrong hint can misdirect work, and a skipped
// one only waits for the next prompt.
func TestHintGateFailsSilent(t *testing.T) {
	emb := &topicEmbedder{}
	code, out := postRenderedHints(t, &hintSessionStore{hints: gateHints()}, emb, map[string]any{"cwd": "/w/osg", "prompt": "   "})
	if code != http.StatusOK || len(out.IDs) != 0 || out.Context != "" {
		t.Errorf("empty prompt: status %d, %+v; want 200 and nothing", code, out)
	}
	if len(emb.texts) != 0 {
		t.Errorf("embedder called for an empty prompt: %q", emb.texts)
	}

	code, out = postRenderedHints(t, &hintSessionStore{hints: gateHints()}, &topicEmbedder{fail: true}, map[string]any{
		"cwd": "/w/osg", "prompt": "why does the dice parser reject 3d6+2 now",
	})
	if code != http.StatusOK || len(out.IDs) != 0 {
		t.Errorf("embedder failure: status %d, %+v; want 200 and nothing", code, out)
	}

	store := teststore.New(t, &mockEmbedder{dim: 4}, "test")
	h := httpapi.New(store, nil, "", httpapi.WithSessionStore(&hintSessionStore{hints: gateHints()}))
	resp := doJSON(t, h, "POST", "/v1/context/hints/render", map[string]any{"cwd": "/w/osg", "prompt": "the dice parser again"})
	decodeJSON(t, resp, &out)
	if len(out.IDs) != 0 {
		t.Errorf("no embedder: %v shown; with nothing to score against, show nothing", out.IDs)
	}
}

func TestHintGateBadRequests(t *testing.T) {
	for _, body := range []map[string]any{
		{"prompt": "the dice parser again"},
		{"cwd": "/w/osg", "prompt": "the dice parser again", "limit": -1},
	} {
		if code, _ := postRenderedHints(t, &hintSessionStore{}, &topicEmbedder{}, body); code != http.StatusBadRequest {
			t.Errorf("body %v: want 400, got %d", body, code)
		}
	}
}
