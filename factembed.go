package memstore

import (
	"context"
	"fmt"
	"math"

	embedding "github.com/matthewjhunter/go-embedding"
)

// factFields is the metadata header applied to every chunk of a fact.
//
// The subject is re-applied to each chunk rather than only the first. Split the
// rendered text instead and chunk 0 keeps the subject while chunks 1..N are
// anonymous prose with nothing saying which fact they belong to -- no error,
// just worse retrieval on exactly the long facts chunking was meant to rescue.
// SplitRecord enforces that; memstore only has to say what the header is.
func factFields(subject string) []embedding.Field {
	if subject == "" {
		return nil
	}
	return []embedding.Field{{Key: "subject", Value: subject}}
}

// FactEmbedText renders what is sent to the model for one chunk of a fact: the
// document task prefix, the subject header, then the chunk body.
//
// The task prefix is what the model was trained to key off. nomic-embed-text
// expects "search_document:" on stored text and "search_query:" on the query;
// memstore sent neither, so every comparison crossed a boundary the model was
// trained to distinguish, silently. See FactQueryText for the other side.
//
// TaskRetrievalDocument rather than TaskClustering because memstore's primary
// vector use is query-to-fact search, which is asymmetric. Fact-to-fact
// comparison still works, since both operands are document vectors and share a
// space.
func FactEmbedText(model, subject, chunk string) string {
	return embedding.FormatRecordForTask(model, embedding.TaskRetrievalDocument,
		factFields(subject), chunk)
}

// FactQueryText renders a search query under the model's query task, so it is
// comparable with the document vectors FactEmbedText produced.
//
// Getting this wrong is quiet. The two sides are set in different files and
// nothing fails when they disagree -- results are just worse. What must not
// happen again is either side being changed without the other.
func FactQueryText(model, query string) string {
	return embedding.FormatForTask(model, embedding.TaskRetrievalQuery, query)
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
	chunks := factChunksFor(model, f, ceiling)
	if len(chunks) == 0 {
		return FactVectors{}, nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
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

// factChunksFor splits a fact's content into chunks whose *rendered* size fits
// the budget, with the header re-applied to each.
//
// SplitRecord owns the parts that are easy to get wrong: it splits the body
// rather than the rendered text (so late chunks keep their header and their
// offsets stay inside the content), and it charges the header against the
// budget by measuring it rather than assuming a size.
//
// MaxBytes is the ceiling the backend imposes and MaxTokens the retrieval
// target; whichever binds first wins, so the ceiling can only ever make chunks
// smaller.
func factChunksFor(model string, f Fact, ceiling int) []embedding.RenderedChunk {
	return embedding.SplitRecord(model, embedding.TaskRetrievalDocument,
		factFields(f.Subject), f.Content, embedding.SplitOptions{
			MaxBytes:  ceiling,
			MaxTokens: ChunkTargetTokens,
			Overlap:   ChunkOverlapBytes,
			MinBytes:  ChunkMinBytes,
		})
}

// FactVectorsResult is the outcome for one fact in a batched embed,
// index-aligned with the slice EmbedFactsBatch was given.
//
// Empty Vectors with a nil Err means the fact had nothing embeddable in it --
// a deterministic skip, not a failure. A non-nil Err always comes with no
// vectors: a fact embeds completely or not at all, so a partial set is never
// stored and the fact is retried whole.
type FactVectorsResult struct {
	Vectors FactVectors
	Err     error
}

// EmbedFactsBatch embeds several facts in batched requests, returning one
// result per fact in the same order.
//
// This is the throughput path, for bulk work like re-embedding after an import.
// Sending one request per fact costs the full per-request overhead on every
// fact, and the backends serialise per model anyway, so batching is the only
// lever. batchSize counts inputs -- chunks -- rather than facts, since a long
// fact contributes several.
//
// Batching goes through go-embedding's BatchEmbedResults rather than a
// hand-rolled loop for one reason: it falls back to embedding one at a time
// when a batch errors or comes back short, so a single unembeddable fact fails
// alone instead of taking the other batchSize-1 good ones with it.
func EmbedFactsBatch(ctx context.Context, e embedding.Embedder, model string, facts []Fact, ceiling, batchSize int) []FactVectorsResult {
	out := make([]FactVectorsResult, len(facts))

	items := make([]embedding.BatchItem, len(facts))
	spans := make([][]embedding.RenderedChunk, len(facts))
	for i, f := range facts {
		chunks := factChunksFor(model, f, ceiling)
		spans[i] = chunks
		texts := make([]string, len(chunks))
		for j, c := range chunks {
			texts[j] = c.Text
		}
		items[i] = embedding.BatchItem{Texts: texts}
	}

	for i, r := range embedding.BatchEmbedItems(ctx, e, items, batchSize) {
		if r.Err != nil {
			out[i] = FactVectorsResult{Err: fmt.Errorf("memstore: fact %d: %w", facts[i].ID, r.Err)}
			continue
		}
		if len(r.Vectors) == 0 {
			continue // nothing embeddable in the content
		}
		chunks := make([]FactChunk, len(r.Vectors))
		for j, v := range r.Vectors {
			chunks[j] = FactChunk{
				Ordinal:   spans[i][j].Ordinal,
				Vector:    v,
				ByteStart: spans[i][j].Start,
				ByteEnd:   spans[i][j].End,
			}
		}
		out[i] = FactVectorsResult{Vectors: FactVectors{Whole: poolVectors(r.Vectors), Chunks: chunks}}
	}
	return out
}
