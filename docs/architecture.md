# Memstore Architecture

## Overview

Memstore is a persistent memory system for AI agents. The design is built around four principles:

- **Durable memory** — facts survive process restarts, session boundaries, and machine reboots. Nothing is lost unless explicitly deleted or superseded.
- **Semantic understanding** — hybrid search combines exact-term matching (FTS5) with semantic similarity (vector embeddings) so both precise lookups and fuzzy "what do I know about X" queries work well.
- **Fact provenance** — knowledge is never silently overwritten. Supersession chains preserve the full history of how a fact has evolved, enabling auditing and rollback.
- **Zero external infrastructure** — the only runtime dependencies are SQLite (embedded via `modernc.org/sqlite`, pure Go, no CGO) and Ollama for embeddings. There is no separate database server to run or manage.

The `Store` interface (`store.go`) decouples the storage engine from the MCP layer. Memstore can be used as a Go library directly, independent of the MCP server.

---

## Storage Engine

### Tables

**`memstore_facts`** — the primary facts table.

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER PK | Auto-increment surrogate key |
| `namespace` | TEXT | Partition key for multi-tenant isolation |
| `content` | TEXT | The factual claim |
| `subject` | TEXT | Entity being described (lookup key) |
| `category` | TEXT | Freeform: `preference`, `identity`, `project`, etc. |
| `metadata` | TEXT | JSON object for domain extensions (nullable) |
| `superseded_by` | INTEGER | FK to the replacing fact (NULL = active) |
| `superseded_at` | TEXT | RFC3339 timestamp of supersession |
| `confirmed_count` | INTEGER | Times explicitly confirmed accurate |
| `last_confirmed_at` | TEXT | Most recent confirmation timestamp |
| `use_count` | INTEGER | Auto-incremented on each search retrieval |
| `last_used_at` | TEXT | Most recent retrieval timestamp |
| `embedding` | BLOB | Little-endian float32 vector (NULL until computed) |
| `created_at` | TEXT | RFC3339 creation timestamp |

**`memstore_facts_fts`** — FTS5 virtual table, content-backed against `memstore_facts`. Indexes `content`, `subject`, and `category`.

**`memstore_meta`** — key/value store for schema-level metadata. Currently stores `embedding_model` and `embedding_dim`, recorded on the first embedding operation. These are used to reject model mismatches on subsequent opens.

**`memstore_version`** — single-row table tracking the current schema version. Uses its own table (rather than SQLite's `PRAGMA user_version`) to avoid conflicting with any other schema the caller has in the same database.

### Indexes

```sql
idx_memstore_subject   ON memstore_facts(subject)
idx_memstore_category  ON memstore_facts(category)
idx_memstore_namespace ON memstore_facts(namespace)
idx_memstore_active    ON memstore_facts(id) WHERE superseded_by IS NULL
```

The partial index on active facts speeds up the common case of `OnlyActive=true` queries.

### FTS Sync Triggers

The FTS5 index is kept in sync with `memstore_facts` via three triggers using the standard `ai`/`ad`/`au` pattern:

- `memstore_facts_ai` (after insert) — inserts the new row into the FTS index.
- `memstore_facts_ad` (after delete) — removes the deleted row from the FTS index.
- `memstore_facts_au` (after update) — deletes the old FTS entry, inserts the new one.

Trigger-based sync guarantees index consistency without application-level coordination. Because metadata is not indexed, `UpdateMetadata` skips re-indexing entirely.

### Schema Migrations

Migrations run in `NewSQLiteStore` before the store is usable. Each version is applied in sequence; the version number is written to `memstore_version` after all migrations complete.

| Version | Change |
|---------|--------|
| V1 | Initial schema: `memstore_facts`, FTS5 table, triggers, indexes |
| V2 | `memstore_meta` key/value table |
| V3 | `namespace` column + namespace index |
| V4 | `superseded_at` column |
| V5 | `confirmed_count` and `last_confirmed_at` columns |
| V6 | `use_count` and `last_used_at` columns |

### Concurrency

`SQLiteStore` uses a `sync.RWMutex`: write operations (insert, supersede, delete, update, embed) take an exclusive lock; reads take a shared lock. The MCP server opens the database with `max_open_conns=1` and WAL mode + a 5-second busy timeout to handle the single-writer constraint cleanly.

---

## Search System

Search runs two passes in parallel (within a single lock hold), then merges and ranks the results.

### FTS5 Query Construction

Each word in the query is individually double-quoted before being passed to the FTS5 `MATCH` expression:

```go
func quoteFTSQuery(raw string) string {
    words := strings.Fields(raw)
    for _, w := range words {
        escaped := strings.ReplaceAll(w, `"`, `""`)
        quoted = append(quoted, `"`+escaped+`"`)
    }
    return strings.Join(quoted, " ")
}
```

This prevents query injection — FTS5 operators like `OR`, `AND`, `-`, and column prefix syntax (`subject:foo`) are treated as literal terms rather than search operators. Words are joined with implicit AND, so a query like `matthew commit style` matches documents containing all three terms.

The FTS query fetches `MaxResults * 2` rows to give the merge step enough candidates from each source.

### Vector Search

The query is embedded via Ollama, then cosine similarity is computed in-process against every stored embedding:

```go
sim := CosineSimilarity(queryEmb, f.Embedding)
if sim > 0 {
    candidates = append(candidates, scored{fact: *f, score: sim})
}
```

Only positive-similarity results are kept. Results are sorted descending and capped at `MaxResults * 2` before merging.

Cosine similarity is computed as:

```
dot(a, b) / (|a| * |b|)
```

Returns 0 for zero-magnitude vectors or dimension mismatches.

### Score Merging

FTS and vector result sets are merged by fact ID. FTS scores (raw BM25, negative by convention) are negated and normalized to [0, 1] by dividing by the maximum score in the FTS result set. Vector scores are already in [0, 1] from cosine similarity.

The combined score:

```
combined = (FTSWeight * fts_score) + (VecWeight * vec_score)
```

Default weights: FTS 0.6, vector 0.4. These are configurable per `SearchOpts`. Facts appearing in only one result set have a zero score for the missing component.

### Temporal Decay

An optional exponential decay can be applied per category after scoring:

```
combined *= 0.5 ^ (age_seconds / half_life_seconds)
```

The MCP server configures `note` facts with a 30-day half-life. `preference` and `identity` facts are not configured, so they receive no decay and remain at full score regardless of age. The `CategoryDecay` map allows per-category overrides; a zero value for a category explicitly disables decay for that category.

### Batch Search

`SearchBatch` amortizes the Ollama embedding cost across multiple queries by issuing a single batched `POST /api/embed` call for all query strings, then running the FTS + vector pass separately for each query using the pre-fetched embeddings.

---

## Embedding Pipeline

### Embedder Interface

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Model() string
}
```

`Model()` returns a stable identifier for the embedding model. The store records this on the first embedding operation and rejects mismatched embedders on subsequent opens. This prevents silently mixing embeddings from different models, which would corrupt similarity scores.

### OllamaEmbedder

`OllamaEmbedder` calls `POST /api/embed` on the configured Ollama instance. It accepts a batch of strings and returns a batch of float32 vectors. The HTTP client is a plain `http.Client` with no custom timeout — timeouts are managed via the context passed by the caller.

Transient failures (e.g., Ollama model still loading) are retried up to two additional times (`embedMaxRetries = 2`) before returning an error. Context cancellation short-circuits the retry loop.

### Binary Encoding

Embeddings are stored as SQLite BLOBs using little-endian float32 encoding:

```go
func EncodeFloat32s(v []float32) []byte {
    buf := make([]byte, len(v)*4)
    for i, f := range v {
        binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
    }
    return buf
}
```

This is compact (4 bytes per dimension, no base64 overhead) and directly decodable without parsing.

### Model Validation

On `NewSQLiteStore`, if an embedder is provided, the store checks `memstore_meta` for a stored `embedding_model` key. If found and it doesn't match `embedder.Model()`, the store returns an error immediately. This prevents silently degraded search quality from model drift.

On the first actual embedding operation, the model name and dimension are written to `memstore_meta` for future validation.

---

## Supersession Model

### Chain Structure

Facts form singly-linked forward chains via the `superseded_by` foreign key:

```
fact 10 (superseded_by=15) → fact 15 (superseded_by=22) → fact 22 (superseded_by=NULL, ACTIVE)
```

The `superseded_by IS NULL` predicate identifies active facts. The partial index `idx_memstore_active` makes this filter efficient.

`History` walks the chain in both directions starting from any member: backward by querying `WHERE superseded_by = current_id` (finding predecessors), then forward by following `SupersededBy` pointers.

### Explicit Supersession

Two mechanisms for explicit supersession:

1. **`memory_store` with `supersedes`** — stores the new fact and calls `Supersede(oldID, newID)` in one operation.
2. **`memory_supersede`** — links two already-stored facts. The MCP server validates that both facts exist and the old one is not already superseded before calling `Supersede`.

`Supersede` uses an `UPDATE ... WHERE superseded_by IS NULL` guard to prevent double-supersession.

### Automatic Supersession

The `FactExtractor.trySupersedeExisting` method runs after each successful insert during extraction:

1. Search for active facts with the same subject (up to 10 candidates).
2. Skip self, skip facts with no embedding.
3. Check `MetadataConflicts` — if both facts have metadata and any shared keys have different values, skip. This prevents facts from different projects or sources from superseding each other across context boundaries.
4. Compute cosine similarity against each candidate.
5. If the best match exceeds `similarityThreshold` (0.85), supersede it.

The 0.85 threshold is intentionally conservative to minimize false positives. A threshold this high means the facts are nearly saying the same thing — genuine updates rather than loosely related information.

### Metadata Conflict Prevention

```go
func MetadataConflicts(a, b json.RawMessage) bool {
    // parse both as map[string]any
    // for each shared key, compare string representations
    // return true if any shared key has different values
}
```

If either fact has no metadata, conflicts returns false (no context to compare). This allows bare facts (no metadata) to supersede each other freely while preventing cross-context contamination when metadata is present.

---

## Task System

Tasks are regular facts stored under `subject="todo"` with a structured metadata schema enforced by the MCP server:

| Metadata key | Values | Description |
|-------------|--------|-------------|
| `kind` | `"task"` | Discriminator — required for task tool operations |
| `scope` | `matthew`, `claude`, `collaborative` | Task ownership |
| `status` | `pending`, `in_progress`, `completed`, `cancelled` | Lifecycle state |
| `priority` | `high`, `normal`, `low` | Execution priority |
| `surface` | `"startup"` | Present on pending/in-progress tasks; removed on completion/cancellation |
| `project` | string | Optional grouping label |
| `due` | string | Optional free-form due date |
| `note` | string | Optional transition note set by `memory_task_update` |

### Startup Surfacing

The `surface="startup"` pattern allows an MCP client to retrieve all pending work at session start with a single call:

```
memory_list(metadata: {"surface": "startup"})
```

This returns all facts (across all subjects and categories) where `metadata.surface = "startup"`. Because tasks are facts, this integrates naturally with the existing metadata filter infrastructure — no special-casing required.

When a task transitions to `completed` or `cancelled`, `memory_task_update` patches `surface` to null, removing it from the startup list.

### Status Transitions

`memory_task_update` validates:
- The fact exists
- `metadata.kind = "task"` (rejects non-task facts)
- The requested status is a valid value

Then applies a metadata patch via `UpdateMetadata`. `UpdateMetadata` does not trigger FTS re-indexing or re-embedding, keeping task status updates cheap.

---

## Fact Extraction

`FactExtractor` distills unstructured text into structured facts using an LLM via the `Generator` interface.

The extraction prompt asks the LLM to return a JSON array of `{content, subject, category}` objects. If the generator implements `JSONGenerator`, structured JSON output mode is used for more reliable parsing. Otherwise the response is parsed with a fallback that strips markdown fences and finds the outermost `[...]` block.

The full pipeline for each extracted fact:

1. **Dedup check** — `Exists(content, subject)` skips exact-content duplicates.
2. **Embed** — compute the embedding immediately (if an embedder is available).
3. **Insert** — store the fact with embedding.
4. **Auto-supersede** — call `trySupersedeExisting` (see Supersession Model above).

`ExtractFacts` is a stateless variant that returns parsed facts without inserting them, for caller-managed insertion workflows.

---

## Design Decisions

### SQLite over an external database

SQLite is embedded in the binary via `modernc.org/sqlite` (pure Go, no CGO). There is no server process to run, no network dependency, and no configuration beyond a file path. WAL mode gives concurrent reads with single-writer semantics. The entire memory store is a single file that can be backed up with `cp`.

The tradeoff is that vector search is an in-process scan rather than an index-accelerated query. For the expected scale of agent memory (thousands to tens of thousands of facts), this is fast enough — embedding dimension is typically 768-4096 floats, and the scan runs in milliseconds.

### Hybrid search over pure vector

Full-text search is fast and precise for exact-term matches. If you search for "matthew commit style", FTS finds facts containing those exact words immediately. Vector search catches semantic similarity — paraphrases, related concepts — but can miss exact matches when embeddings of common words are noisy.

Combining both with a weighted merge captures the strengths of each. The default 60/40 FTS/vector split slightly favors precision while still surfacing semantically similar results that FTS would miss.

### Supersession over deletion

Deleting a fact destroys the information that it was ever believed. Supersession preserves the history of how knowledge evolved, which is valuable for understanding why the agent believes what it currently believes, debugging incorrect behavior, and restoring prematurely superseded facts.

`memory_delete` exists but the tool description discourages its use for outdated facts. The preferred workflow is `memory_store` with `supersedes`.

### Namespace isolation

The `namespace` column partitions facts for multi-tenant use. All reads and writes are automatically scoped to the store's namespace. Cross-namespace search is opt-in via `SearchOpts.Namespaces`. This allows multiple agents or projects to share a single SQLite file without data leakage between them.

### Trigger-based FTS sync

Maintaining the FTS index in triggers rather than application code guarantees consistency regardless of how the database is modified. Any `INSERT`, `UPDATE`, or `DELETE` on `memstore_facts` — including direct SQL, migrations, or import — automatically keeps the FTS index correct.
