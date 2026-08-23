package memstore

import (
	"context"
	"fmt"

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
// each one, returning a FactChunk per chunk in ordinal order.
//
// ceiling is a hard byte bound on the rendered text of any single chunk,
// normally the effective input budget of the configured embedder. The subject
// header is charged against it, so the body gets the remainder -- sizing the
// body at the full ceiling and then prepending a header would push every
// request over it.
//
// Either every chunk embeds or none do. A partially embedded fact would look
// complete to the queue (its marker vector is set) and leave a permanent hole
// in the middle of it, so a failure returns no chunks and the fact is retried
// whole.
func EmbedFact(ctx context.Context, e embedding.Embedder, model string, f Fact, ceiling int) ([]FactChunk, error) {
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
		return nil, nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = FactEmbedText(f.Subject, c.Text)
	}

	vecs, err := embedding.EmbedWithRetry(ctx, e, texts)
	if err != nil {
		return nil, fmt.Errorf("memstore: embedding fact %d: %w", f.ID, err)
	}
	if len(vecs) != len(chunks) {
		return nil, fmt.Errorf("memstore: embedding fact %d: got %d vectors for %d chunks",
			f.ID, len(vecs), len(chunks))
	}

	out := make([]FactChunk, len(chunks))
	for i, c := range chunks {
		out[i] = FactChunk{
			Ordinal:   c.Ordinal,
			Vector:    vecs[i],
			ByteStart: c.Start,
			ByteEnd:   c.End,
		}
	}
	return out, nil
}
