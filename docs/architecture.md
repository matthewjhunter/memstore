# Memstore Architecture

## Overview

Memstore is a persistent memory system for AI agents. The design is built around four principles:

- **Durable memory** -- facts survive process restarts, session boundaries, and machine reboots. Nothing is lost unless explicitly deleted or superseded.
- **Hybrid retrieval** -- exact-term matching (FTS) plus semantic similarity (vector embeddings) plus optional cross-encoder reranking, so both precise lookups and "what do I know about X" queries land the right fact.
- **Fact provenance** -- knowledge is never silently overwritten. Supersession chains preserve the full history of how a fact has evolved, enabling auditing and rollback.
- **Honest layering** -- the `Store` interface separates storage from MCP, the HTTP API mirrors it for daemon mode, hooks are the optional ergonomic layer on top. Each piece is replaceable without rewriting the others.

> **⚠ v0.3.0 is single-user by deployment, not by enforcement.** The `Identity` plumbing exists end-to-end on the request path but no read or write path filters by user yet. Two valid tokens see the same facts. See [`MIGRATING.md`](MIGRATING.md). v0.4.0 closes this gap.

The `Store` interface (`store.go`) decouples the storage engine from the MCP layer. Memstore can be used as a Go library directly, independent of the MCP server, in either mode.

---

## Deployment Shape

One shape since 0.6.0:

```
  MCP client (Claude Code, Cursor, Zed, ...)      memstore CLI / hooks
        │                                                │
        └────────────── HTTPS + bearer token ────────────┘
                                │
                                ▼
                         memstored (daemon)
                                │
                                ▼
                          PostgresStore
                                │
                                ▼
                        Postgres + pgvector

                        Optional: cross-encoder reranker endpoint
                        (Lemonade, llama.cpp --reranking, or any
                         Cohere/Jina-shaped /v1/rerank)
```

The daemon serves the REST API and MCP over HTTP under `/memstore/`; the CLI and the hooks are clients of it, configured by `remote` in `config.toml` or `MEMSTORE_REMOTE`. The local SQLite mode and the stdio `memstore-mcp` binary were removed in 0.6.0 (announced in 0.4.0); `memstore export --db` still reads an old SQLite file so it can be imported into a daemon, and that reader is removed in 0.7.0.

---

## Storage

The `Store` interface (`store.go`) defines the operations: `Insert`, `Search`, `SearchFTS`, `Get`, `List`, `Update`, `UpdateMetadata`, `Supersede`, `Delete`, `History`, `Link`, `Unlink`, `GetLinks`, plus the session/feedback helpers (`SessionStore`, `FeedbackStore`). One implementation, `pgstore`; `httpclient.Client` implements the same interface over the daemon's REST API so the CLI and the MCP server are written against the interface rather than the database.

### PostgresStore

Implemented in `pgstore/`.

**Embeddings via pgvector.** The `embedding` column is `vector(N)` where N is fixed at construction time. An HNSW index over the column bounds latency as the corpus grows. Changing embedding models is a column-type change + full re-embed -- intentional, to make model migrations explicit.

**FTS via tsvector.** The `content` column is mirrored into a `tsvector` column populated by a generated expression, indexed with GIN, queried via `plainto_tsquery`. Scoring is `ts_rank_cd` (BM25-flavored), normalized per query.

**Concurrency** is just Postgres -- multiple writers, MVCC, no application-level locking. The Go side uses a `pgxpool` connection pool.

**Async embed queue** (`httpapi/embedqueue.go`) decouples write latency from embedder latency. New facts land with `embedding IS NULL`; a background worker polls via `NeedingEmbedding`, embeds one at a time (per-fact, not batched, for resilience against poisoned inputs), and writes the vector back. Facts whose embed call hits a permanent error (post adaptive shrink, see *Embedding* below) get `embed_failed_at` and `embed_error` stamped via `MarkEmbedFailed`; `NeedingEmbedding` filters them out. The schema V2 migration adds a hard `CHECK (length(content) <= 8000)` at the database layer to refuse oversized facts before they reach the queue.

### Schema migrations

Migrations run before the store is usable; the version is written to `memstore_version` after each migration completes. Every schema change is a new `migrateVN()` in `pgstore/store.go`, wired into `migrate()`.

| Version | Change |
|---------|--------|
| V1 | Full v0.3.0 baseline: `memstore_facts`, `memstore_meta`, `memstore_links`, tsvector + GIN index, HNSW over `vector(N)`, all column shape that SQLite reached over V1-V9, plus `api_tokens` and the session-capture tables |
| V2 | `CHECK (length(content) <= 8000)` enforced at the DB layer |
| V3 | Embed-queue quarantine columns (`embed_failed_at`, `embed_error`) |

The V1 baseline consolidates the shape the SQLite schema reached over nine migrations before the Postgres backend existed; the SQLite series itself is gone with that backend.

### Indexes

Indexes on `subject`, `category`, `kind`, `subsystem`, `namespace`, plus a partial index on `(id) WHERE superseded_by IS NULL` to accelerate `OnlyActive=true` queries.

---

## The Search Pipeline

Search runs in three stages. The first two run in parallel; the third is optional and runs after the merge.

### Stage 1: FTS

`tsvector` indexed with GIN. Query via `plainto_tsquery`. Scoring with `ts_rank_cd`. Same per-query min-max normalization to `[0, 1]` so the score is comparable to cosine.

The FTS query fetches `MaxResults * 2` rows (or `RerankCandidates` if larger) to give the merge enough candidates.

### Stage 2: Vector

The query is embedded via the configured embedder (or fetched from the in-memory cache on the daemon for repeat prompts). Cosine similarity is computed against the stored chunk vectors through the HNSW index. Only positive-similarity results are kept; sorted descending; capped at the same fetch limit as FTS.

### Score merging

FTS and vector result sets are merged by fact ID (`mergeFirstStage` in `score.go`). The combined first-stage score is:

```
combined = (FTSWeight * fts_score_normalized) + (VecWeight * vec_score)
```

Default weights: FTS 0.6, vector 0.4. Configurable per query via `SearchOpts`. Facts appearing in only one result set have a zero score for the missing component.

### Stage 3: Cross-encoder rerank (optional)

When a reranker is configured and the rerank mode is not `off`, the top-K candidates from the merge are sent to a sidecar (typically llama.cpp's `llama-server --reranking`, speaking the Cohere/Jina `/v1/rerank` wire shape). Raw logits are sigmoided to `[0, 1]`. Four fusion modes:

| Mode | Behavior |
|------|----------|
| `off` | No rerank. First-stage order is final. |
| `balanced` | `combined = weight * rerank + (1 - weight) * first_stage` (default weight 0.7). The cross-encoder influences ordering without overruling first-stage entirely. |
| `dominant` | `combined = rerank + 1e-6 * first_stage`. Rerank is authoritative; first-stage only breaks ties. |
| `gate` | First-stage order is preserved; rerank is used only to drop candidates below the threshold. |

A separate relevance threshold drops candidates whose rerank score is below it. Default 0; common values are 0.1-0.3.

Per-path document truncation budgets keep latency bounded. The search path uses ~2800 bytes per candidate; the recall path uses ~1200 bytes. Cross-encoder cost is superlinear in sequence length (attention is O(N²)), so document length, not pool size, is the dominant latency lever.

When the sidecar is unreachable, the rerank stage gracefully degrades to first-stage order rather than failing the search. The `IsRerankAvailable(err)` predicate distinguishes transient 5xx (degrade) from permanent 4xx (surface as error). The model can adjust mode, threshold, weight, candidate counts, and doc-byte budgets per session via the `memory_rerank_settings` MCP tool.

### Temporal decay

After scoring (and after rerank), an optional exponential decay can be applied per category:

```
combined *= 0.5 ^ (age_seconds / half_life_seconds)
```

The MCP server configures `note` facts with a 30-day half-life. `preference` and `identity` facts have no decay (the `CategoryDecay` map handles the per-category override; a missing entry means no decay).

### Batch search

`SearchBatch` amortizes the embedder round-trip across multiple queries by batching the embed call.

---

## Embedding

### go-embedding

Embedding code lives in the standalone [go-embedding](https://github.com/matthewjhunter/go-embedding) module. The interface:

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Model() string
}
```

`Model()` returns a stable identifier. The store records this on the first embedding operation and rejects mismatched embedders on subsequent opens. This prevents silently mixing embeddings from different models, which would corrupt similarity scores.

go-embedding supports Ollama's native API and any OpenAI-compatible `/v1/embeddings` endpoint (LiteLLM, vLLM, Lemonade, OpenAI itself). Configuration is env-driven via `MEMSTORE_EMBED_*` (cascading to `EMBEDDING_*`).

### Adaptive shrink

A fact too long for the embedder's context window would naively be a permanent error. go-embedding handles this transparently: on a context-length error, it shrinks the input (default 80% per iteration) and retries, down to a configured floor. The floor is the point where the input is so short the embedding would be useless. If the embedder still rejects at the floor, that's the permanent error the queue sees.

### IsRetryable classification

go-embedding distinguishes transient failures (5xx, timeout, model still loading) from permanent failures (4xx for invalid input, auth, context-length after shrink) via `PermanentError` and the `IsRetryable(err)` predicate. The embed queue uses this to decide whether to retry or quarantine.

### Query embedding cache (read side)

Per-prompt recall fires on every user message in Claude Code. To avoid re-embedding the same prompt repeatedly within a short window, the daemon caches query embeddings in a small in-memory LRU (a few hundred entries, minutes-long TTL). A stale entry is just a slightly older vector for the same text, which is still the right vector.

---

## The Daemon (memstored)

`cmd/memstored` is a Go binary wrapping `PostgresStore` in HTTP. Most endpoints mirror the `Store` interface; a few are higher-level workflows.

### HTTP surface

| Endpoint | Purpose |
|----------|---------|
| `GET /v1/health` | Unauthenticated liveness probe |
| `POST /v1/facts` / `GET /v1/facts/{id}` | Insert / fetch |
| `POST /v1/search` | Full hybrid search with optional rerank |
| `POST /v1/recall` | Per-prompt context injection (called by `memstore-prompt.mjs`) |
| `POST /v1/context/touch` | File access tracking |
| `GET /v1/context/hints`, `POST /v1/context/hints/{id}/consume` | Proactive hints surfaced from the extraction pipeline |
| `POST /v1/sessions/turns`, `/v1/sessions/turns/finalize` | Session capture pipeline (see below) |
| `POST /v1/context/injections`, `/v1/context/feedback` | Injection records + post-session feedback |

### Authentication

Every endpoint except `/v1/health` requires a bearer token. Tokens are issued via `memstore admin issue-token --user <user> <user>@<host>`; the plaintext is shown once. The daemon stores SHA-256 hashes (high-entropy input by construction -- 32 random bytes -- so a slow hash isn't necessary). Verification uses `crypto/subtle.ConstantTimeCompare` against a timing oracle.

The `api_tokens` table (`pgstore/tokens.go`):

```sql
CREATE TABLE api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    token_hash   BYTEA       NOT NULL UNIQUE,
    name         TEXT        NOT NULL,
    scopes       TEXT[]      NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
```

### Identity context (the gap)

The auth middleware populates an `httpapi.Identity` struct on the request context. The struct carries `Name`, `Scopes`, `Source`. This is threaded through the HTTP handlers.

**The identity does not yet reach the Store layer.** Every `Search`, `List`, `Get`, `Insert` runs without consulting the request identity. Two tokens with different `name` values see the same facts. v0.3.0 ships the plumbing; v0.4.0 wires the enforcement. See [`tier3-permissions.md`](tier3-permissions.md) for the schema design.

### TLS

The daemon does its own TLS termination. TLS 1.2 minimum, 1.3 in practice. Optional mTLS via `--client-ca` (sets `tls.RequireAndVerifyClientCert`). The stdlib-only CA in `internal/caetl` exists to bootstrap a self-signed PKI for homelab deployments without pulling in a third-party CA library.

### Container

Published to GHCR on every push to main. The Dockerfile builds a minimal image (`golang:1.26-alpine` builder → distroless-style runtime). The container expects `MEMSTORE_PG_SECRET`, embedder env vars, and optionally `MEMSTORE_TLS_CERT_FILE` / `MEMSTORE_TLS_KEY_FILE` / `MEMSTORE_TLS_CLIENT_CA_FILE`.

---

## Session Capture Pipeline

Beyond storing and searching facts, the daemon captures session activity for later analysis and to feed back into recall ranking.

### Tables

| Table | Purpose |
|-------|---------|
| `session_turns` | Every user/assistant message: `session_id`, `uuid`, `turn_index`, `role`, `content`, `cwd`, `created_at` |
| `context_hints` | Extractor outputs: short observations worth surfacing on a future session in the same `cwd`. Each carries `retrieved_ids`, `candidate_scores`, `search_query`, `ranker_version`, `consumed_at` |
| `context_injections` | Record of which facts were injected during a session: `ref_id`, `ref_type` (`fact` or `hint`), `rank`. Powers in-session deduplication and the feedback loop |
| `context_feedback` | Post-session ratings: `score` in `[-1, +1]`, optional `reason`. Unique on `(ref_id, ref_type, session_id)` |

### Flow

```
SessionStart hook → no-op for capture (other hooks fire)
   │
UserPromptSubmit hook → /v1/recall → context_injections row per surfaced fact
   │
each user/assistant turn → /v1/sessions/turns → session_turns row
   │
Stop hook → /v1/sessions/turns/finalize → triggers two jobs:
   │
   ├─ ExtractQueue Stage 2: local Ollama reads the transcript, generates hints,
   │    writes context_hints rows for the next session in the same cwd to consume
   │
   └─ autoRateFacts: local Ollama reads the transcript, rates each
        injection in [-1, +1], writes context_feedback rows
```

`backfill-feedback` re-runs the auto-rating pass over historical sessions on daemon startup, so sessions that landed while the daemon was down get rated retroactively.

### Feedback into ranking

The per-fact aggregate of `context_feedback` ratings is a multiplier on the fact's recall score. Default behavior: confidence-weighted by the number of ratings (a fact with 1 rating gets a gentle nudge; a fact with 5+ gets the full multiplier in either direction). Asymmetric: a fact that's consistently helpful in many sessions earns a real boost, but a single bad session can't bury it.

See [`training-data-design.md`](training-data-design.md) for the longer story on what the captured data could be used for beyond recall ranking.

---

## Supersession Model

### Chain Structure

Facts form singly-linked forward chains via the `superseded_by` foreign key:

```
fact 10 (superseded_by=15) → fact 15 (superseded_by=22) → fact 22 (superseded_by=NULL, ACTIVE)
```

The `superseded_by IS NULL` predicate identifies active facts. The partial index on `(id) WHERE superseded_by IS NULL` makes this filter efficient.

`History` walks the chain in both directions starting from any member: backward by querying `WHERE superseded_by = current_id` (finding predecessors), then forward by following `SupersededBy` pointers.

### Explicit supersession

Two mechanisms:

1. **`memory_store` with `supersedes`** -- stores the new fact and calls `Supersede(oldID, newID)` in one operation.
2. **`memory_supersede`** -- links two already-stored facts. The MCP server validates both exist and the old one is not already superseded.

`Supersede` uses an `UPDATE ... WHERE superseded_by IS NULL` guard to prevent double-supersession races.

### Automatic supersession

`FactExtractor.trySupersedeExisting` runs after each successful insert during extraction:

1. Search for active facts with the same subject (up to 10 candidates).
2. Skip self, skip facts with no embedding.
3. Check `MetadataConflicts` -- if both facts have metadata and any shared keys have different values, skip. This prevents facts from different projects or sources from superseding each other across context boundaries.
4. Compute cosine similarity against each candidate.
5. If the best match exceeds the similarity threshold (0.85), supersede it.

The 0.85 threshold is intentionally conservative. A threshold this high means the facts are nearly saying the same thing -- genuine updates rather than loosely related information.

---

## Task System

Tasks are regular facts stored under `subject="todo"` with a structured metadata schema enforced by the MCP server:

| Metadata key | Values | Description |
|-------------|--------|-------------|
| `kind` | `"task"` | Discriminator -- required for task tool operations |
| `scope` | `matthew`, `claude`, `collaborative` | Task ownership |
| `status` | `pending`, `in_progress`, `completed`, `cancelled` | Lifecycle state |
| `priority` | `high`, `normal`, `low` | Execution priority |
| `surface` | `"startup"` | Present on pending/in-progress tasks; removed on completion/cancellation |
| `project` | string | Optional grouping label |
| `due` | string | Optional free-form due date |
| `note` | string | Optional transition note set by `memory_task_update` |

### Startup surfacing

The `surface="startup"` pattern allows an MCP client to retrieve all pending work at session start with a single call:

```
memory_list(metadata: {"surface": "startup"})
```

This returns all facts (across subjects and categories) where `metadata.surface = "startup"`. Because tasks are facts, this integrates naturally with the existing metadata filter infrastructure.

When a task transitions to `completed` or `cancelled`, `memory_task_update` patches `surface` to null, removing it from the startup list.

---

## Fact Extraction

`FactExtractor` distills unstructured text into structured facts using an LLM via the `Generator` interface. The extraction prompt asks for a JSON array of `{content, subject, category}` objects. If the generator implements `JSONGenerator`, strict JSON schema is used; otherwise the response is parsed with markdown-fence stripping.

The full pipeline for each extracted fact:

1. **Dedup check** -- `Exists(content, subject)` skips exact-content duplicates.
2. **Embed** -- compute the embedding (synchronously in the extraction path; async in the post-write embed queue for direct `memory_store` calls).
3. **Insert** -- store with embedding.
4. **Auto-supersede** -- call `trySupersedeExisting` (see Supersession).

In daemon mode, **ExtractQueue Stage 2** runs after session end: the local Ollama instance reads the session transcript and produces hint records (separate from facts -- hints are session-scoped suggestions, not durable knowledge). See *Session Capture Pipeline* above.

---

## Design Decisions

### Postgres only

Through 0.5.x there were two backends: SQLite for one human on one machine, Postgres for the daemon. The daemon was where every feature landed first -- session capture, hint generation, the screening queue, per-user isolation, the document corpus -- and the SQLite side either lagged or was single-user by construction, so "same code surface for both" had stopped being true. Claude Code ships its own local-only memory; memstore's value is the cross-session, cross-machine, server-backed layer, and one backend is cheaper to keep correct than two. Postgres + pgvector gives real concurrency, a vector index that scales past a few thousand rows, and a boring backup story. 0.6.0 removed the SQLite store; `examples/docker-compose/` is the one-machine deployment.

### Hybrid search over pure vector or pure FTS

Pure vector misses exact-term matches (identifiers, command flags, acronyms) where the embedding's semantic neighborhood is broader than the user's intent. Pure FTS misses paraphrases and conceptual neighborhoods. Hybrid catches both lanes' failure modes.

### Cross-encoder rerank as a third stage

The hybrid first-stage produces a candidate set that contains the right answer; the rerank stage discriminates within that set. Bi-encoders are fast and approximately right; cross-encoders are slow and accurately right. The combination is "broad recall, precise ordering." The cost is one extra network call per search and a sidecar to operate, but the tunables let the model trade latency for relevance per session.

### Async embed queue, per-fact

Embedding is a network call. Doing it inline on `INSERT` ties write latency to embedder tail latency. Decoupling it via the queue makes writes fast and accepts a few-second window where new facts are FTS-only.

Per-fact instead of batched because a batched call lets one poisoned input stall the entire queue. Per-fact serializes failures; one bad row gets quarantined and the rest move on.

### Supersession over deletion

Deleting a fact destroys the information that it was ever believed. Supersession preserves the history of how knowledge evolved, which matters for understanding why the agent believes what it currently believes, debugging incorrect behavior, and restoring prematurely superseded facts.

`memory_delete` exists but the tool description discourages its use for outdated facts. The preferred workflow is `memory_store` with `supersedes`.

### Bearer tokens + TLS, not OAuth

The deployment shape is a single-operator homelab. OAuth would be over-engineered for the scale. Bearer tokens issued by the operator via `memstore admin issue-token` are simple, auditable (the `api_tokens` table records issuance, last-used, revocation), and adequate against the threat model. TLS covers the wire; constant-time hash comparison covers the verification timing.

When the deployment shape changes -- multi-tenant, external users, regulatory attestation -- the auth model will need to grow. v0.3.0 is single-operator; v0.4.0 adds per-user enforcement; further milestones may need a real federated identity story. See [`tier3-permissions.md`](tier3-permissions.md).

### Single-user by deployment, not enforcement (for now)

The shipped state acknowledges what's true: authentication knows who's calling; the store doesn't ask. This is honest about the security posture rather than implying isolation that doesn't exist. The work to close the gap is scoped in [`tier3-permissions.md`](tier3-permissions.md) and tracked for v0.4.0.

### Namespace isolation

The `namespace` column partitions facts for multi-instance use within a single backend. All reads and writes are scoped to the store's namespace; cross-namespace search is opt-in via `SearchOpts.Namespaces` and, since 0.4.0, still filtered by the caller's user. This is **not** a multi-user boundary -- it's a per-deployment partition for cases where one human runs multiple memstore instances against the same database.

## Further reading

- [`MIGRATING.md`](MIGRATING.md) -- upgrade between releases
- [`tier1-graph-basics.md`](tier1-graph-basics.md) -- explicit links and neighborhood queries
- [`tier2-graph-analytics.md`](tier2-graph-analytics.md) -- connected components, summary triggers
- [`tier3-permissions.md`](tier3-permissions.md) -- multi-user isolation design (v0.4.0)
- [`tier4-bulk-ingestion.md`](tier4-bulk-ingestion.md) -- bulk import/export and offline ETL
- [`local-llm-features.md`](local-llm-features.md) -- proposed local-LLM features menu
- [`training-data-design.md`](training-data-design.md) -- what session capture is for
- [`multi-user-data-model.md`](multi-user-data-model.md) -- the projects-and-roles layer beyond v0.4.0
- [`web-ui-brief.md`](web-ui-brief.md) -- proposed admin UI
