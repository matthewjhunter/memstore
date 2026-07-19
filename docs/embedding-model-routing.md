# Multi-space embeddings: code, text, and model migration

Status: design, not started
Author: Matthew + Claude
Date: 2026-07-18

## Why

Two problems share one mechanism.

**Code ingestion.** Memstore holds almost no code today -- of 3865 active facts,
one contains a fenced code block. That is not evidence code does not belong
here; it is the residue of a capability that did not work. Ingestion was painful
and retrieval over what did land was poor, partly because a general text
embedding model is a bad fit for source. Tier 4 revisits ingestion, and it needs
somewhere for code to land that retrieval can actually reach.

**Model migration.** Today the embedding model cannot be changed at all.
`validateEmbedder` compares `memstore_meta['embedding_model']` at open and
returns `MismatchError` on any difference, with no migration path: switching
models means dropping every vector and re-embedding the corpus with the store
offline. That is a bad property for a store meant to run for years, and it will
bite the moment a better model appears -- including the code model above.

Both need the same thing: more than one vector space in the store at once.

## The constraint everything follows from

Cosine similarity between vectors from different models is meaningless. Not
noisy -- meaningless. The spaces are unrelated, so a score of 0.8 in one says
nothing about a score of 0.8 in the other, and a single ANN index over both
returns garbage no matter how results are filtered afterward.

So each model gets its own column and its own index, and results from different
spaces can never be compared by score.

## Schema

Per-space columns rather than a discriminator column:

```
embedding        vector(768)   -- existing; the text space
embedding_code   vector(N)     -- new; whatever the code model produces
```

Dimensions differ by model (nomic-embed-text 768, jina-code 768, Qwen3-0.6B
1024, nomic-embed-code 3584), and pgvector columns are fixed-width, so a shared
column is not an option even when two models happen to agree on width. Each
column carries its own HNSW index.

The `memstore_meta` fingerprint becomes per space: `embedding_model.text`,
`embedding_dim.text`, `embedding_model.code`, `embedding_dim.code`. The existing
mismatch guard keeps its current meaning within a space, which is the part worth
keeping -- silently mixing two models inside one column is the failure it exists
to prevent.

Facts may be embedded in one space or several. A fact embedded in no space is
retrievable by FTS only, which is the status quo for anything the embed queue has
not reached yet.

## Write-side routing

**Explicit, not sniffed.** Bulk ingestion knows what it is reading: a `.go` file
is code because of where it came from, not because a heuristic counted braces. A
content-type is set by the ingesting caller and stored on the fact; the embed
queue routes on it.

Sniffing is the wrong default here for the same reason regex screening is a weak
detector: it fires on shape, and memstore is full of prose that mentions code.
"scanFact and factColumns must stay in sync" is a sentence about code, and should
land in the text space; a function body should not. The caller knows which it has
and no classifier does better.

Interactive `memory_store` defaults to text, which is what it stores today.

## Query-side routing, and why the reranker settles it

The hard question is which space to query, since a natural-language question can
target code ("how does scanFact handle metadata") and a code-shaped query can
target prose.

Do not classify the query. Retrieve from **every** populated space independently,
each with its own model's embedding of the query, then union the candidates.

That union cannot be ordered by score -- see the constraint. It does not need to
be: memstore already runs a cross-encoder reranker as the third stage of search,
and a cross-encoder scores (query, document) pairs directly in one comparable
space. It is exactly the primitive the multi-space problem needs, and it is
already there.

So the pipeline becomes:

1. FTS candidates (unchanged, space-agnostic)
2. Vector candidates from each space, by rank within that space
3. Union, deduplicate by fact ID
4. Cross-encoder rerank the union -- one comparable score for everything
5. Threshold and return

Where no reranker is configured, fall back to interleaving spaces by rank
(reciprocal rank fusion), which is order-preserving within a space and makes no
cross-space score claims.

Cost: one query embedding per space. Query embeddings are already cached, and
the candidate pool per space can shrink so the union stays the size the reranker
sees today.

## Backfill and migration

The same machinery covers a model change:

1. Add the new space, leave the old one serving.
2. The embed queue fills the new space in the background, oldest first.
3. Search reads both. Coverage climbs; results improve as it does.
4. When coverage is complete, retire the old column.

No downtime, no offline re-embed, and a store that is half-migrated is
degraded-but-correct rather than broken -- each space is internally consistent
throughout, which is the property the current single-model guard protects and
this must not give up.

## Model candidates for the code space

The fleet's embedding tier is two Intel A380s (6 GB) running nomic-embed-text at
0.3 GB, so a code model that fits alongside is preferred; the lemonade boxes can
take a larger one if the quality difference justifies moving embedding traffic
there.

| model | params | dim | notes |
|---|---|---|---|
| jina-embeddings-v2-base-code | 161M | 768 | 8192 ctx, fits the A380 easily |
| CodeRankEmbed | 137M | 768 | retrieval-tuned for code search |
| Qwen3-Embedding-0.6B | 600M | 1024 | general, strong on code |
| nomic-embed-code | 7B | 3584 | best quality, needs a lemonade box |

Selection wants a bake-off against real ingested code rather than a leaderboard,
and that cannot happen until ingestion lands something to test with.

## Open questions

- Where does content-type live: a first-class `Fact` field, or metadata? A
  first-class field is honest about it being routing input rather than
  domain data, at the cost of touching `factColumns`/`scanFact`/transfer.
- Should a code fact also be embedded in the text space? Its doc comment is
  prose and may retrieve better there. Doubles storage and embed cost for that
  class of fact; worth measuring, not assuming.
- Does FTS need per-space tokenization? Code identifiers tokenize badly under an
  English stemmer, which may matter more for retrieval than the vector space
  does. Possibly the higher-value fix, and independent of this work.
