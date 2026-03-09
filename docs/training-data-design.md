# Training Data Design for Retrieval Specialization

## Background

The memstore hint pipeline has five LLM-powered roles: Extractor, Summarizer, Scorer,
Curator, and Synthesizer. Of these, Scorer (urgency classification) and Curator (fact
relevance ranking) are the strongest candidates for specialization into smaller, faster
models trained on accumulated session data.

The `context_feedback`, `context_injections`, and `context_hints` tables provide the
foundation for training data collection, but several gaps make the data incomplete for
learning-to-rank (LTR) and DPO/preference fine-tuning. This document describes what
was missing, why it matters, and the schema changes that close the gaps.

## Research basis

Key papers informing this design:

- [Unbiased Learning to Rank Meets Reality: Baidu's Large-Scale Dataset (2024)](https://arxiv.org/abs/2404.02543) — listwise ranking loss is the single highest-leverage choice; debiasing techniques are secondary.
- [Investigating the Robustness of Counterfactual LTR Models (2024)](https://arxiv.org/html/2404.03707) — CLTR fails when training sessions are small; DLA+PBM is the most robust debiasing approach.
- [Distilling LLMs into Cross-Encoders for Reranking (2024)](https://arxiv.org/html/2405.07920v1/) — LLM-score distillation outperforms training on binary clicks; cross-encoders achieve comparable accuracy to LLM rerankers at orders-of-magnitude lower latency.
- [Towards Disentangling Relevance and Bias in ULTR (2022)](https://arxiv.org/pdf/2212.13937) — without position-varied data, propensity estimation is structurally unidentified when the production ranker is good.
- [DPO Survey: Datasets, Theories, Variants, Applications (2024)](https://arxiv.org/html/2410.15595v3) — quality over quantity; 500–5,000 clean preference pairs beat 50,000 noisy ones.
- [Less is More: Preference Data Selection (2025)](https://arxiv.org/html/2502.14560v1) — diversity + margin filtering of preference pairs consistently outperforms raw data dumps.

## Gaps in the original schema

### Gap 1: No query stored (critical)

For any LTR or preference-learning use case, the canonical training example requires
the query that drove retrieval:

```
pointwise:    (query, document, relevance_score)
contrastive:  (query, positive_doc, negative_doc)
```

`context_injections` recorded what was injected but not what query retrieved it.
`generateHints` builds a query string via `buildSearchQuery` but never persisted it.
Without the query, the dataset is `(document, score)` — insufficient for training a
reranker, since the same document can be relevant for one query and noise for another.

### Gap 2: No rank/position stored (critical)

Position bias is the primary confound in all implicit feedback datasets. Items shown
at rank 0 receive more attention than items at rank 4 regardless of quality. Without
the rank at which each item appeared, inverse propensity weighting (IPS) and Dual
Learning Algorithm (DLA) debiasing are impossible.

### Gap 3: Negatives not logged (high)

`ref_ids` on `context_hints` recorded which facts were selected (positives). The
rejected candidates — retrieved by the Searcher but not selected — were discarded.
Contrastive training requires negative examples. Without them, only positive-unlabeled
(PU) learning is possible, which needs more data and careful class-prior estimation.

### Gap 4: Serving policy not stored (high)

Propensity estimation requires knowing *why* a document appeared at its position —
specifically, which ranker version produced the list. As the hint pipeline evolves,
historical logs become confounded without a `ranker_version` field. A document at
rank 0 under pipeline v1 has a different propensity interpretation than rank 0 under
v2.

### Gap 5: Soft labels not stored (medium)

Binary ±1 explicit feedback is sparse (requires voluntary `memory_rate_context` calls).
The Searcher already computes vector similarity scores for every candidate. Persisting
these scores per-candidate enables LLM-distillation-style training: treat the vec score
as a soft relevance label for any (query, document) pair. This generates training signal
on every retrieval event without any user action.

## Schema changes

### `context_hints` — five new columns

```sql
search_query     TEXT NOT NULL DEFAULT ''     -- query fed to the Searcher
ranker_version   TEXT NOT NULL DEFAULT ''     -- pipeline version string
retrieved_ids    JSONB NOT NULL DEFAULT '[]'  -- all candidate fact IDs (before selection)
candidate_scores JSONB NOT NULL DEFAULT '{}'  -- {fact_id_str: vec_score} for all candidates
```

`ref_ids` (already present) continues to record the *selected* subset. `retrieved_ids`
records the full retrieval set. Together they identify positives (in both) and negatives
(in `retrieved_ids` but not `ref_ids`).

### `context_injections` — one new column

```sql
rank INT NOT NULL DEFAULT -1  -- 0-based position in the candidate list at injection time
```

Default -1 preserves backward compatibility for rows inserted before this migration.

## Training data construction

Once the schema is populated, training examples can be constructed as:

**Pointwise (Scorer training):**
```sql
SELECT
    ch.search_query                   AS query,
    f.content                         AS document,
    (ci.rank + 1.0) / n.total         AS position_fraction,  -- propensity proxy
    cf.score                          AS label,               -- +1 / -1
    ch.ranker_version
FROM context_hints ch
JOIN context_injections ci ON ci.ref_id = ci.ref_id  -- join via ref_ids
JOIN context_feedback cf ON cf.ref_id = ...
```

**Contrastive (Curator training):**
```sql
-- positives: fact IDs in ref_ids
-- negatives: fact IDs in retrieved_ids but not in ref_ids
-- soft label: candidate_scores[fact_id]
```

**LLM-distilled reranker training (no feedback required):**
```sql
SELECT
    ch.search_query     AS query,
    f.content           AS document,
    (ch.candidate_scores ->> f.id::text)::float AS vec_score  -- soft label
FROM context_hints ch, jsonb_array_elements_text(ch.retrieved_ids) AS fid
JOIN facts f ON f.id = fid::bigint
WHERE ch.search_query != ''
```

## Intervention logging (future)

The [Disentangling paper](https://arxiv.org/pdf/2212.13937) recommends deliberately
randomizing rankings for 1–5% of retrieval events. Without position-varied data, the
identifiability condition for propensity estimation fails once the production ranker
is good (high-quality facts always appear at rank 0).

Implementation: add a `shuffled BOOL NOT NULL DEFAULT false` column to `context_hints`
and occasionally shuffle `searchResults` before selection. Flag those hints. Use only
flagged rows for propensity estimation. Not implemented in this iteration; tracked as
a follow-up.

## Automatic feedback (future)

Voluntary `memory_rate_context` calls generate too little signal (~0–5/session) for
the training volume required (~1,000–5,000 preference pairs for DPO; ~10,000 sessions
for reliable listwise LTR). The path to sufficient volume is automatic rating at session
start: after 2–3 turns of a new session, check whether the consumed hint was relevant
to what was actually asked, and write a `context_feedback` record automatically.

This is architecturally straightforward (the model is in context, `FeedbackStore` is
already wired through), but requires prompt design and testing. Tracked as a follow-up.

## Dataset size thresholds

From the literature:

| Task | Practical floor | Target |
|---|---|---|
| DPO/preference pairs (Scorer, Curator) | 500 pairs | 1,000–5,000 |
| Cross-encoder domain fine-tune | 1,000–5,000 (q,d) pairs | 10,000–50,000 |
| Naive listwise click training | ~5,000–10,000 sessions | Much more tractable |
| Full ULTR with propensity debiasing | ~50,000 sessions | 500,000+ |

At single-user session volume, DPO on clean preference pairs is the reachable near-term
target. Full ULTR debiasing is not practically achievable without automatic feedback.
