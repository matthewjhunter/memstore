package memstore

import (
	embedding "github.com/matthewjhunter/go-embedding"
)

// FactChunk is one embeddable span of a fact's content, and the unit a vector
// is actually stored against.
//
// A fact is a set of vectors rather than a point. Most facts are short enough
// to be a single chunk, but a long one is split so that each vector describes
// one passage: a vector has fixed capacity, and averaging a whole document
// into it dilutes exactly the specificity retrieval depends on. See ChunkFact.
type FactChunk struct {
	// Ordinal is the chunk's position within the fact, from 0.
	Ordinal int
	// Vector is the embedding of this chunk.
	Vector []float32
	// ByteStart and ByteEnd address the span of Fact.Content this chunk was
	// produced from, so a chunk hit resolves back to a passage of the fact
	// rather than only to the fact. Overlap makes these impossible to
	// recompute after the fact, so they are stored rather than derived.
	ByteStart int
	ByteEnd   int
}

// FactVectors is everything embedding a fact produces: the retrieval vectors,
// one per chunk, and the single whole-fact vector used for comparing facts to
// each other.
//
// The two are separate because they answer different questions. Retrieval wants
// the nearest passage, so it needs a vector per chunk. Deduplication asks
// whether two facts say the same thing, which is a property of the whole fact;
// comparing a whole-fact vector against a chunk would compare an average with
// an opening passage, score systematically low, and silently stop deduplicating
// long facts. Auto-supersession is destructive, so that asymmetry is not one to
// leave in place.
type FactVectors struct {
	// Whole is the vector over the fact's entire content, and the one stored
	// as the fact row's marker. For a fact short enough to be a single chunk
	// it is that chunk's vector -- they are the same text.
	Whole []float32
	// Chunks holds one vector per chunk, in ordinal order.
	Chunks []FactChunk
}

// Chunk sizing for fact embeddings.
//
// These are retrieval targets, deliberately far below the models' registered
// budgets (nomic-embed-text and embeddinggemma are both 6000 bytes). A vector
// has fixed capacity, so filling the whole context window averages away the
// specificity retrieval depends on; 256-512 tokens per chunk is the usual
// working range. go-embedding's Split defaults to the model budget because
// that is the only figure it knows, and documents that the caller should pass
// something smaller -- this is memstore passing it.
//
// Keeping the target separate from the backend ceiling matters: the ceiling
// exists so a request is not rejected, and the two answer different questions.
// Deriving chunk size from the ceiling means raising the ceiling silently
// widens every chunk and quietly degrades retrieval, with nothing reporting it
// (see matthewjhunter/herald#297).
const (
	// ChunkTargetTokens is the size chunks aim for, in tokens.
	//
	// Tokens rather than bytes because that is the unit chunk size is reasoned
	// about in, and the bytes-per-token ratio varies by model and by corpus.
	// go-embedding converts it through the ratio it has actually observed for
	// the model (BudgetForTokens), falling back to a conservative figure until
	// enough has been seen.
	ChunkTargetTokens = 512
	// ChunkOverlapBytes repeats the tail of each chunk at the head of the
	// next, so a boundary landing mid-argument does not strand the two halves
	// in vectors that each describe half a thought.
	ChunkOverlapBytes = 128
	// ChunkMinBytes absorbs a trailing sliver into its predecessor. A few
	// words on their own embed to a vector that matches almost nothing.
	ChunkMinBytes = 200
)

// ChunkFact splits a fact's content into the spans that will each become one
// vector. Most facts are short and come back as a single chunk; only content
// past the chunk target is split.
//
// ceiling is a hard upper bound in bytes, normally the effective input budget
// of the configured embedder (Config.Limits().MaxBytes). It only ever makes
// chunks smaller: a ceiling below the target clamps it, so a backend stricter
// than the target still gets requests it will accept, while a ceiling above
// the target is ignored rather than used to inflate chunks. Pass 0 for no
// ceiling.
//
// Chunk.Text is exactly content[Start:End], so a stored vector can be traced
// back to the span of the fact it describes.
func ChunkFact(model, content string, ceiling int) []embedding.Chunk {
	return embedding.Split(model, content, embedding.SplitOptions{
		// The token target and the backend ceiling, whichever binds first. The
		// ceiling can only ever make chunks smaller; a ceiling above the target
		// is ignored rather than used to inflate them.
		MaxTokens: ChunkTargetTokens,
		MaxBytes:  ceiling,
		Overlap:   ChunkOverlapBytes,
		MinBytes:  ChunkMinBytes,
	})
}
