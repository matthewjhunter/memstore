package memstore

import (
	"context"
	"fmt"
	"math"

	embedding "github.com/matthewjhunter/go-embedding"
)

// FactEmbedText renders what is actually sent to the model for one chunk: the
// fact's subject, then the chunk body.
//
// The subject is re-applied to every chunk rather than only the first. Split
// the rendered text instead and chunk 0 keeps the subject while chunks 1..N are
// anonymous prose with nothing saying which fact they belong to -- no error,
// just worse retrieval on exactly the long facts chunking was meant to rescue.
func FactEmbedText(subject, chunk string) string {
	if subject == "" {
		return chunk
	}
	return subject + ": " + chunk
}

// EmbedFact splits a fact's content into retrieval-sized chunks and embeds
// each one, plus the whole-fact vector deduplication compares against.
//
// ceiling is a hard byte bound on the rendered text of any single chunk,
// normally the effective input budget of the configured embedder. The subject
// header is charged against it, so the body gets the remainder -- sizing the
// body at the full ceiling and then prepending a header would push every
// request over it.
//
// The whole-fact vector is pooled from the chunk vectors rather than embedded
// from the whole text. Embedding the whole text is not available here: a fact
// that splits is by definition longer than the ceiling, so that request is the
// one a strict backend rejects, and clipping it back under the ceiling would
// discard the tail -- reintroducing the loss chunking removed. Pooling covers
// every chunk, costs no extra request, and is the standard answer for a
// whole-document vector.
//
// Pooling is the right tool here precisely because the question is different
// from retrieval's. Averaging dilutes the specificity a passage lookup needs,
// which is why chunks are stored separately; but "do these two facts say the
// same thing" is a property of the whole fact, and a pooled vector is what
// represents it.
//
// Either every vector embeds or none do. A partially embedded fact would look
// complete to the queue (its marker vector is set) and leave a permanent hole
// in the middle of it, so a failure returns nothing and the fact is retried
// whole.
func EmbedFact(ctx context.Context, e embedding.Embedder, model string, f Fact, ceiling int) (FactVectors, error) {
	// Charge the header to the budget by measuring it rather than assuming it.
	overhead := len(FactEmbedText(f.Subject, ""))
	bodyCeiling := ceiling
	if bodyCeiling > 0 {
		bodyCeiling -= overhead
		if bodyCeiling < 1 {
			// A subject long enough to leave no room for a body is
			// pathological. Embed what fits rather than splitting into
			// slivers; go-embedding still clips to the model's budget.
			bodyCeiling = 1
		}
	}

	chunks := ChunkFact(model, f.Content, bodyCeiling)
	if len(chunks) == 0 {
		return FactVectors{}, nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = FactEmbedText(f.Subject, c.Text)
	}

	vecs, err := embedding.EmbedWithRetry(ctx, e, texts)
	if err != nil {
		return FactVectors{}, fmt.Errorf("memstore: embedding fact %d: %w", f.ID, err)
	}
	if len(vecs) != len(texts) {
		return FactVectors{}, fmt.Errorf("memstore: embedding fact %d: got %d vectors for %d inputs",
			f.ID, len(vecs), len(texts))
	}

	out := FactVectors{Chunks: make([]FactChunk, len(chunks))}
	for i, c := range chunks {
		out.Chunks[i] = FactChunk{
			Ordinal:   c.Ordinal,
			Vector:    vecs[i],
			ByteStart: c.Start,
			ByteEnd:   c.End,
		}
	}
	out.Whole = poolVectors(vecs)
	return out, nil
}

// poolVectors averages unit-normalised vectors into a centroid.
//
// Normalising first stops a chunk with a larger magnitude from dominating the
// result, so the centroid reflects direction -- which is all cosine similarity
// reads -- rather than length. A single vector pools to itself, so a fact that
// did not split keeps exactly its own chunk vector.
func poolVectors(vecs [][]float32) []float32 {
	if len(vecs) == 0 {
		return nil
	}
	if len(vecs) == 1 {
		return vecs[0]
	}
	out := make([]float32, len(vecs[0]))
	for _, v := range vecs {
		if len(v) != len(out) {
			// Ragged dimensions mean the backend changed model mid-batch;
			// there is no meaningful centroid to take.
			return nil
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			continue
		}
		for i, x := range v {
			out[i] += float32(float64(x) / norm)
		}
	}
	for i := range out {
		out[i] /= float32(len(vecs))
	}
	return out
}
