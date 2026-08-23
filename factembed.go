package memstore

import (
	"context"
	"fmt"
	"math"

	embedding "github.com/matthewjhunter/go-embedding"
)

// FactEmbedText renders what is actually sent to the model for one chunk: the
// fact's subject, then the chunk body, under the model's document task prefix.
//
// The subject is re-applied to every chunk rather than only the first. Split
// the rendered text instead and chunk 0 keeps the subject while chunks 1..N are
// anonymous prose with nothing saying which fact they belong to -- no error,
// just worse retrieval on exactly the long facts chunking was meant to rescue.
//
// The task prefix is what the model was trained to key off. nomic-embed-text
// expects "search_document:" on stored text and "search_query:" on the query;
// memstore sent neither, so every comparison crossed a boundary the model was
// trained to distinguish, silently. See FactQueryText for the other side.
//
// TaskRetrievalDocument rather than TaskClustering because memstore's primary
// vector use is query-to-fact search, which is asymmetric retrieval. herald
// picks clustering for the same library because its primary use is
// article-to-article grouping -- different primary use, different right answer.
// Fact-to-fact comparison (deduplication, supersession) still works here
// because both operands are document vectors, so they share a space.
//
// An unregistered model yields a passthrough prompter, so the text is returned
// unprefixed rather than mangled; EmbedConfigFromEnv defaults StrictModel on so
// that case is loud at startup instead of silent here.
func FactEmbedText(model, subject, chunk string) string {
	text := chunk
	if subject != "" {
		text = subject + ": " + chunk
	}
	return embedding.FormatForTask(model, embedding.TaskRetrievalDocument, text)
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
	chunks, _ := factChunksFor(model, f, ceiling)
	if len(chunks) == 0 {
		return FactVectors{}, nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = FactEmbedText(model, f.Subject, c.Text)
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
// the ceiling. The task prefix and subject header are charged against it by
// measuring the rendering of an empty body rather than assuming a size -- size
// the body at the full ceiling and then prepend a header and every request goes
// over it.
func factChunksFor(model string, f Fact, ceiling int) ([]embedding.Chunk, int) {
	overhead := len(FactEmbedText(model, f.Subject, ""))
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
	return ChunkFact(model, f.Content, bodyCeiling), overhead
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

	// Flatten every fact's chunks into one input slice, remembering which fact
	// each came from so the vectors can be zipped back afterwards.
	var texts []string
	var spans []FactChunk
	var owner []int
	for i, f := range facts {
		chunks, _ := factChunksFor(model, f, ceiling)
		for _, c := range chunks {
			texts = append(texts, FactEmbedText(model, f.Subject, c.Text))
			spans = append(spans, FactChunk{Ordinal: c.Ordinal, ByteStart: c.Start, ByteEnd: c.End})
			owner = append(owner, i)
		}
	}
	if len(texts) == 0 {
		return out
	}

	res, err := embedding.BatchEmbedResults(ctx, e, texts, batchSize, nil)
	if len(res) != len(texts) {
		// Every input failed, or the helper broke its index-alignment contract.
		// Either way there is nothing to zip; fail each fact with the cause.
		if err == nil {
			err = fmt.Errorf("memstore: embedding facts: got %d results for %d inputs", len(res), len(texts))
		}
		for _, i := range owner {
			out[i].Err = err
		}
		return out
	}

	for j, r := range res {
		i := owner[j]
		if out[i].Err != nil {
			continue
		}
		if r.Err != nil {
			// One failed chunk fails the whole fact: a half-embedded fact would
			// look complete to the queue and leave a permanent hole in it.
			out[i] = FactVectorsResult{Err: fmt.Errorf("memstore: embedding fact %d: %w", facts[i].ID, r.Err)}
			continue
		}
		c := spans[j]
		c.Vector = r.Vector
		out[i].Vectors.Chunks = append(out[i].Vectors.Chunks, c)
	}

	// Pool each fact's chunk vectors into its whole-fact vector.
	for i := range out {
		if out[i].Err != nil || len(out[i].Vectors.Chunks) == 0 {
			continue
		}
		vecs := make([][]float32, len(out[i].Vectors.Chunks))
		for k, c := range out[i].Vectors.Chunks {
			vecs[k] = c.Vector
		}
		out[i].Vectors.Whole = poolVectors(vecs)
	}
	return out
}
