# Memstore Improvements

Proposed changes in priority order. These are designed to be domain-agnostic
so memstore remains a general-purpose hybrid search library.

## 1. Namespace Sets in SearchOpts ✓

**Problem:** `AllNamespaces` is all-or-nothing. When multiple applications
share a database (each using its own namespace), a caller often needs to
search its own namespace plus a shared one, not every namespace in the
database.

**Change:** Replace the `AllNamespaces bool` field with `Namespaces []string`.

```go
type SearchOpts struct {
    // ...existing fields...
    Namespaces []string // if non-empty, search these namespaces instead of the store's default
}
```

Behavior:
- `nil`/empty: search the store's own namespace (current default behavior).
- Non-empty: search only the listed namespaces.

The same field should be added to `QueryOpts` for `List` queries.

`AllNamespaces` should be kept temporarily as a deprecated alias that
produces a query with no namespace filter, then removed in a future version.

**SQL change:** Replace `AND namespace = ?` with `AND namespace IN (?, ?, ...)`
when `Namespaces` is populated. When empty, fall back to the existing single-
namespace filter.

## 2. Batch Search ✓

**Problem:** Both current consumers issue multiple sequential searches per
operation (e.g., one per question during an editing pass, or POV + scene
prompt during outlining). Each search embeds the query independently, so N
searches make N embedding API calls.

**Change:** Add a batch search method.

```go
// SearchBatch performs multiple hybrid searches, sharing a single batched
// embedding call for all queries. Each query may carry its own opts override;
// opts that are nil fall back to the provided defaults.
func (s *SQLiteStore) SearchBatch(
    ctx context.Context,
    queries []string,
    defaults SearchOpts,
    perQuery []SearchOpts, // nil, or same length as queries
) ([][]SearchResult, error)
```

Implementation:
1. Collect all query strings, call `e.Embed(ctx, queries)` once.
2. For each query, run `searchFTS` + `searchVector` + `mergeResults` with the
   corresponding embedding and opts.
3. Return results in the same order as queries.

If `perQuery` is nil, every query uses `defaults`. If non-nil, it must
have the same length as `queries`; each entry overrides `defaults` for that
query (zero-value fields fall back to the default).

## 3. Inclusive-Null Metadata Filters ✓

**Problem:** `MetadataFilter` excludes rows where the metadata key is missing
or null. This forces callers to post-filter when the absence of a key means
"applies universally" rather than "doesn't match."

Common example: facts scoped to a range (chapter, time period) mixed with
unscoped facts that apply everywhere. A filter like `chapter <= 5` correctly
matches chapter-scoped facts but drops unscoped ones.

**Change:** Add an `IncludeNull` field to `MetadataFilter`.

```go
type MetadataFilter struct {
    Key        string
    Op         string
    Value      any
    IncludeNull bool // if true, also include rows where Key is absent/null
}
```

SQL generation when `IncludeNull` is true:

```sql
AND (json_extract(metadata, '$.chapter') IS NULL
     OR json_extract(metadata, '$.chapter') <= ?)
```

This eliminates the need for application-layer post-filtering (currently done
in scene-chain's `FilterByChapter`).

## 4. Temporal Filtering ✓

**Problem:** Both consumers need time-scoped queries. One needs "facts from
the last N hours" for recency; the other uses chapter-based scoping (already
handled via metadata). But general-purpose temporal filtering on `CreatedAt`
is useful across the board and currently requires the caller to post-filter.

**Change:** Add `CreatedAfter` and `CreatedBefore` to both `SearchOpts` and
`QueryOpts`.

```go
type SearchOpts struct {
    // ...existing fields...
    CreatedAfter  *time.Time // exclude facts created before this time
    CreatedBefore *time.Time // exclude facts created after this time
}
```

SQL: `AND created_at >= ?` / `AND created_at <= ?`, applied in both the FTS
and vector search queries.

## 5. Temporal Decay Scoring ✓

**Problem:** For long-lived stores, older facts can crowd out recent ones at
equal relevance. A fact from a year ago and a fact from yesterday with the
same FTS+vector scores should not rank equally when recency matters.

**Change:** Add an optional decay function to `SearchOpts`.

```go
type SearchOpts struct {
    // ...existing fields...
    DecayHalfLife time.Duration // if >0, apply exponential time decay to combined score
}
```

When `DecayHalfLife` is set, after computing the combined FTS+vector score,
multiply by a decay factor:

```go
age := time.Since(fact.CreatedAt)
decay := math.Pow(0.5, age.Seconds()/halfLife.Seconds())
result.Combined *= decay
```

This is opt-in. Callers that don't set it get current behavior. A half-life
of 720h (30 days) would mean a 30-day-old fact scores at 50% of an
identical new one; a 60-day-old fact at 25%.

The decay is applied after FTS+vector merge and before the final sort, so it
modifies ranking without affecting the underlying relevance signals. The raw
`FTSScore` and `VecScore` fields remain unmodified for callers that want to
inspect them.

## 6. Additional Embedder Implementations (tracked: #1)

**Problem:** Only `OllamaEmbedder` exists. Deployments without local Ollama
need an alternative.

**Change:** Add one or two additional implementations behind the existing
`Embedder` interface:

- **`HTTPEmbedder`**: Generic embedder that POSTs to a configurable URL and
  parses a configurable response path. Covers OpenAI-compatible APIs
  (including vLLM, llama.cpp server, LiteLLM proxy) without vendor-specific
  code.

- **`OpenAIEmbedder`** (optional convenience wrapper): Targets the OpenAI
  embeddings endpoint with API key auth. Could be a thin wrapper around
  `HTTPEmbedder` with preset URL and response parsing.

The `Embedder` interface doesn't need to change. This is purely additive.

## 7. Subject Reverse Lookup

**Problem:** Relationship-type facts are directional. "A trusts B" is stored
with `Subject: "A"`. Searching for "B" won't find it unless "B" happens to
appear in the content and FTS picks it up. This is fragile.

**Change:** Add a `SubjectAlso` search mode that, for relationship-category
facts, also matches when the query term appears in the content of facts
whose category indicates a relationship.

Alternative (simpler): document a convention where callers store
bidirectional facts at insert time. This keeps memstore simple but pushes
the burden to the application layer.

Recommendation: start with the convention (document it), revisit if
multiple consumers need the same logic.
