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

## Lexical retrieval is broken for code first -- fix that before this

Measured against the production corpus, 2026-07-18. FTS, not the vector space, is
where code-adjacent retrieval currently fails, and the failures are concrete.

**Postgres indexes a file path as one whole-path token.**

    to_tsvector('english', 'see memstore/sqlite.go for detail')
      -> 'memstore/sqlite.go', 'detail', 'see'

    query 'sqlite.go'          -> no match
    query 'sqlite'             -> no match
    query 'memstore/sqlite.go' -> match

Only the exact full path retrieves it. 300 facts mention a `.go` file; their
filenames are effectively unsearchable by name. This is the clearest retrieval
defect found, and it has nothing to do with embeddings.

**camelCase is never split, in either backend.** `scanFact` indexes as one token,
so `scan` does not match it. Exact identifier lookup works (`factColumns` finds
its 2 facts); component queries do not. Arguably right -- the vector side is what
should catch "the function that scans facts" -- but it should be a decision
rather than an accident.

**Postgres stems identifiers as English.** `factColumns` -> `factcolumn`,
`NewSQLiteStore` -> `newsqlitestor`, `EvidenceMaxRunes` -> `evidencemaxrun`.
Linguistically nonsense, mostly harmless in practice since query and document go
through the same stemmer.

**The two backends disagree.** SQLite's FTS5 `unicode61` splits on `.` and `/`
and does not stem, so `sqlite` DOES match `memstore/sqlite.go` there, while
`sqlite.go` is a query syntax error. Same corpus, same query, different results
depending on backend -- worth pinning in the conformance suite either way.

### Experiment: decomposed tsvector, measured on the real corpus

Run 2026-07-18 against a local copy of all 3865 active facts. Document side:
keep the english tsvector and append a decomposed form -- split on camelCase
boundaries and on `/ . _ - :` , run through `simple` (no stemming), weight `D`.

**Blending the query hurt more than it helped.** Querying
`english(q) || simple(decompose(q))` raises raw hit counts a lot (`sqlite.go`
0 -> 23) but the number that matters is where the true match lands:

    query              rank now   rank blended
    MaxContentLength      -            5        fixed
    sqlite.go             -           23        still not surfaced
    NewSQLiteStore        1            7        REGRESSED
    SQLiteStore           1            1
    factColumns           1            1
    memory_store          1            1

Decomposing the query turns an identifier into an AND of its parts, which matches
every fact mentioning those parts unrelatedly -- `sqlite.go` returned 23 hits of
which 1 contained the string. The flood pushes exact matches down: a query that
worked perfectly went from rank 1 to rank 7. Recall bought with precision, and
the trade was bad.

**Falling back instead of blending is strictly better.** Decompose the document
always, so the tokens exist; decompose the QUERY only when the exact query comes
up short, and rank those results on their own:

    query              exact  exact_rank   fallback_rank
    memory_store          28       1           (not needed)
    NewSQLiteStore         2       1           (not needed)
    SQLiteStore            1       1           (not needed)
    factColumns            2       1           (not needed)
    scanFact               2       1           (not needed)
    MaxContentLength       0       -                4      fixed
    sqlite.go              0       -                9      fixed

Every query that already worked keeps rank 1, because the fallback never fires
for it. The two that were unfindable become findable inside a top-10 window. No
regressions.

Better still, append rather than replace: exact hits first, decomposed hits below
them. That is strictly additive -- it can only fill space an exact match was not
using.

Cost: GIN index 2192 kB -> 3376 kB (+54%), average lexemes per fact 41 -> 75
(+83%). Cheap for the corpus sizes involved.

Contained to the `fts` generated-column expression, a rebuild migration, and the
query builder. Worth landing and re-running these queries against the live
corpus before the dual-space embedding work starts.

Caveat on the numbers: this corpus contains almost no code, so these queries test
retrieval of *mentions* of identifiers in prose. Once ingestion lands actual
source, re-run the same measurement -- the token distribution will be entirely
different and the fallback threshold may want retuning.

## Model candidates for the code space

The embedding tier is two Intel A380s (6 GB) running nomic-embed-text at 0.3 GB,
but that is not a ceiling. lemonade/olla are reliable and performant now, so
embedding traffic can move to a lemonade box if a larger model earns it -- the 7B
option is genuinely on the table rather than aspirational.

| model | params | dim | notes |
|---|---|---|---|
| jina-embeddings-v2-base-code | 161M | 768 | 8192 ctx, fits the A380 easily |
| CodeRankEmbed | 137M | 768 | retrieval-tuned for code search |
| Qwen3-Embedding-0.6B | 600M | 1024 | general, strong on code |
| nomic-embed-code | 7B | 3584 | best quality; needs a lemonade box, now an acceptable home |

Selection wants a bake-off against real ingested code rather than a leaderboard,
and that cannot happen until ingestion lands something to test with.

## Open questions

- Where does content-type live: a first-class `Fact` field, or metadata? A
  first-class field is honest about it being routing input rather than
  domain data, at the cost of touching `factColumns`/`scanFact`/transfer.
- Should a code fact also be embedded in the text space? Its doc comment is
  prose and may retrieve better there. Doubles storage and embed cost for that
  class of fact; worth measuring, not assuming.
- Answered: FTS tokenization is a real and separable defect -- see above. Fix and
  measure it first.
