# Local-LLM Features — Menu

Status: menu of candidates, not a committed roadmap
Author: Matthew + Claude
Date: 2026-05-06

This document collects features that put a local LLM in front of (or behind)
existing memstore primitives. It is a menu, not a committed roadmap. Items
move from this doc into their own design when the prerequisites land and a
concrete payoff is visible.

The graph layer ships first (`docs/tier1-graph-basics.md`). Most features
here either build on the graph or compose with it; without it they're
either trivial or unanchored.

## Constraints

These are the operating constraints for any LLM work inside memstore. They
narrow the design space; they do not forbid it.

1. **No recursive / infinite-loop hooks.** Any spawned LLM work must form a
   DAG. A model call that could re-trigger the same memstore code path that
   spawned it is forbidden. (See `MEMORY.md` → "No post-session hooks for
   memstore" for the May 2026 incident that established this rule.)
2. **No secondary paid LLMs by default.** Don't route memstore work through
   a second Anthropic/OpenAI API key. Local inference only, unless a
   specific feature has an explicit cost ceiling and an opt-in.
3. **Embeddings stay in-process.** They're called on every write and every
   search; the network hop dominates. Already true today via
   `embedding.ConfigFromEnvPrefix("MEMSTORE_EMBED")`.
4. **Generation is already env-driven.** `MEMSTORE_GEN_URL` +
   `MEMSTORE_GEN_MODEL` (`config.go:139-144`) point at any OpenAI-compatible
   endpoint. `OpenAIGenerator` (`openai.go:34`) is the existing wrapper.
   Lemonade speaks the OpenAI API, so the daemon ↔ Lemonade path is
   wired today and this doc inherits it.

## Hardware budget

The Strix Halo workstation (Framework Desktop, AMD Ryzen AI MAX+ 395,
128 GB unified LPDDR5X, ~123 GiB GPU-addressable via Vulkan) running
Lemonade at port 13305 with the vulkan llama.cpp backend can serve
gpt-oss-120b at usable speed. This means *model size is not the
constraint* — latency budget is. Two operating tiers:

| Path                                | Latency budget | Practical model size | Use case                                              |
|-------------------------------------|----------------|----------------------|-------------------------------------------------------|
| Per-tool-call (router, re-rank)     | 50–200 ms      | 3B–7B, hot           | Re-ranking top-K, hybrid weight selection             |
| Per-search-burst (planner)          | 500 ms – 2 s   | 7B–30B               | Multi-step retrieval planning, link-walk decisions    |
| Background / write-time             | seconds–minutes| 30B–120B             | Entity extraction, link proposals, drift sweeps       |
| Async / batch                       | minutes        | 120B                 | Whole-namespace re-clustering, narrative compression  |

Lemonade can host more than one recipe; the daemon picks the endpoint by
call type. `MEMSTORE_GEN_URL` becomes "primary endpoint"; a second env
var (`MEMSTORE_GEN_BIG_URL` or similar) addresses the larger model. The
daemon, not the agent, makes this choice.

## Triage at a glance

| Feature                                  | Build | Ops | Value     | Depends on        |
|------------------------------------------|-------|-----|-----------|-------------------|
| Write-time entity extraction → link proposals | M | M   | high      | KG tier 1         |
| Background drift sweep                   | M     | M   | high      | nothing           |
| Read-time hybrid-search re-ranker        | S     | S   | medium    | nothing           |
| Memory-consolidation drafting            | M     | M   | high      | KG tier 2 (components/Louvain) + this doc's extraction |
| Auto-categorization on store             | S     | S   | medium    | nothing           |
| Summary trigger generation               | S     | S   | medium    | KG tier 2 summary triggers |
| Query decomposition / multi-step planner | M     | M   | speculative | KG tier 1       |
| Natural-language fact editor             | M     | M   | medium    | nothing           |

Build = engineering effort. Ops = the standing complexity once shipped
(queue management, prompt maintenance, model drift). Value = expected
payoff *given memstore's mission*, not abstract LLM-feature merit.

## When to start

Most of this is gated on KG tier 1 shipping and on the link/fact graph
becoming non-trivial. Candidate triggers:

- KG tier 1 is in production and `memory_get_neighborhood` returns
  meaningful subgraphs.
- The fact corpus is large enough that manual `memory_link` curation is
  visibly inadequate (linkage density well below 1 link/fact average for
  facts >30 days old).
- Drift between code-referencing facts and current code is provably
  causing bad agent recommendations.
- A specific workflow surfaces where the local model would compress
  Claude's context measurably (e.g. routing decisions burning >500
  tokens of reasoning per `memory_search`).

Without that signal, this work is speculative and should not start.

## Candidate features

### Write-time entity extraction → link proposals

The single highest-leverage feature in this doc. Memstore today is a
pile of mostly-disconnected facts; `memory_link` is manual; the graph
is sparse. Entity extraction at store time, with link proposals based
on entity overlap, densifies the graph without operator effort.

**Workflow:**

1. `memory_store` writes the fact (existing behavior, unchanged).
2. The daemon enqueues a background job: "extract entities and propose
   links for fact ID X."
3. A background worker pulls jobs, calls the *big-tier* model
   (gpt-oss-120b or a 30B equivalent) with a structured-extraction
   prompt. Returns: `{entities: [...], candidate_link_targets:
   [{fact_id, link_type, confidence, rationale}, ...]}`.
4. The worker resolves candidate targets via vector similarity over the
   entity strings against existing facts, intersected with the model's
   suggestions.
5. Each proposal above a confidence threshold is written to a new
   `memstore_link_proposals` table with `status="proposed"`.
6. New MCP tool `memory_review_link_proposals(fact_id?, status?,
   limit?)` returns proposals; companion `memory_resolve_link_proposal
   (id, action="accept"|"reject"|"edit", ...)` finalizes them.
   Accepted proposals become real edges in `memstore_links`.

**Cost:**

- New table + V4 migration.
- Worker queue (could be as simple as a `proposed_at IS NULL` poll on
  a `memstore_extraction_jobs` table; could be an in-memory channel
  with crash recovery via DB state).
- Two new MCP tools.
- Prompt + JSON schema for the extraction call.
- A "near-existing-link" suppression check so proposals don't duplicate
  existing edges.

**Risks:**

- Model hallucinates fact IDs or link types not in the system. Mitigation:
  resolve all proposed `fact_id`s through the store before persisting;
  drop unresolvable proposals. Use a curated link-type vocabulary (see
  KG tier 2 link-type vocabulary discussion).
- Proposal flood drowns the user. Mitigation: high default confidence
  threshold (0.85+), per-fact proposal cap (5), summary surface that
  groups proposals by source fact.
- Extraction prompt drifts as model versions change. Mitigation: pin
  the model in `MEMSTORE_GEN_BIG_MODEL`; treat the extraction prompt
  as code, version it, test against fixtures.

**Depends on:** KG tier 1 (need link primitives and the GIN index on
link metadata for filtering proposals).

**Status:** Top candidate to start once KG tier 1 ships and the corpus
has enough facts to make the densification visible.

### Background drift sweep

Code-referencing facts (`store.go:97`, function names, file paths,
invariants tied to specific files) decay as code evolves. The
`memory_check_drift` tool exists today but is operator-driven —
nobody runs it on a schedule.

**Workflow:**

1. Periodic background sweep (cron-style, configurable; default daily
   off-hours) iterates over facts whose content matches a "looks like
   a code reference" heuristic (regex over `path/to/file:line` patterns,
   Go-identifier-shaped tokens with package prefixes, etc.).
2. For each candidate, the worker runs the existing
   `memory_check_drift` logic *and* a model pass: feed the fact text
   plus the current contents of the referenced files (or a grep
   excerpt for symbols), ask the model to score "still accurate /
   stale / contradicted" with a one-line rationale.
3. Stale candidates land in a new `memstore_drift_findings` table,
   surfaced via `memory_list_drift_findings`. Don't auto-supersede.

**Cost:**

- New table.
- One MCP tool (the listing tool — supersession is already covered).
- The "looks like code" heuristic; tunable later.
- Scheduling glue (could ride on whatever runs the embedding
  back-fill jobs today, if any; otherwise a small cron).

**Risks:**

- False positives. Code that looks similar but isn't the same. Mitigation:
  rely on the model's rationale, surface the snippet alongside the
  finding, never auto-act.
- Cost of running the model over thousands of facts. Mitigation:
  incremental — only re-check a fact if (a) it has never been checked,
  or (b) its referenced files have mtime newer than the last check.
  Track `last_drift_check_at` on facts.

**Depends on:** nothing strictly required; benefits from KG tier 1 if
drift findings are also exposed as a fact-attribute filter on graph
queries ("show me the neighborhood of X, excluding stale facts").

**Status:** strong second candidate. The pain is real and growing as
the corpus ages.

### Read-time hybrid-search re-ranker

`memory_search` returns top-K from hybrid FTS+vector with current
weighting (see `searchFTS` per the storage invariants). A small
local model re-scoring the top-K against the query can lift precision
on ambiguous queries.

**Workflow:**

1. `memory_search` runs as today, returns top-K (K configurable, default
   maybe 20 internally, 10 to caller).
2. Daemon optionally calls a small-tier model with `(query, [K fact
   summaries])` and gets back a reordered ID list with a relevance
   score per fact.
3. Returns the reordered top-N to the caller.

**Cost:**

- One small-tier model loaded in Lemonade (3B-class; latency target
  100ms for K=20).
- Config knob to enable/disable per-call (`rerank=true|false` in
  search params; off by default until proven).
- Logging to compare ranked vs. unranked results for evaluation.

**Risks:**

- Latency hit on the hot path. Mitigation: opt-in per call, hard
  timeout that falls back to unranked results.
- Marginal vs. measurable improvement unclear without an eval set.
  Mitigation: build the eval set first (capture real queries +
  human-labeled relevance), measure, then ship if the lift is real.
  Don't ship a re-ranker on faith.

**Depends on:** nothing.

**Status:** lowest-priority of the read-time options. The current
hybrid search is decent; the bar to justify added latency and
complexity is high. Wait for evidence of search-quality complaints.

### Memory-consolidation drafting

Composes with the KG tier 2 consolidation workflow (see
`docs/tier2-graph-analytics.md` → "The memory-consolidation
workflow"). KG tier 2 detects clusters; the local LLM drafts the
consolidated fact.

**Workflow:** KG tier 2 surfaces a candidate cluster of N facts. The
big-tier model takes the N facts + their existing links and drafts a
single higher-order fact summarizing the cluster, plus a link
rewiring plan. The proposal goes through the same review surface as
extraction proposals.

**Cost:**

- Reuses extraction's proposal table + review tools (or a parallel
  `memstore_consolidation_proposals` table — decide when scoping).
- Prompt design for "summarize without losing load-bearing detail."
- Link rewiring logic that walks edges into superseded facts and
  re-points them at the consolidated fact.

**Depends on:** KG tier 2 connected components or Louvain (cluster
detection); this doc's extraction pipeline (review tools, proposal
table); existing fact supersession with a link-rewiring extension.

**Status:** the strategic case for memstore staying useful as it
ages. Schedule once KG tier 2 components ship.

### Auto-categorization on store

Facts today require the caller to supply `subject`, `category`,
`kind`, `subsystem`. Claude usually fills these well; humans at the
CLI often don't. A small-tier model could fill missing fields from
content + recent fact patterns.

**Workflow:** `memory_store` with missing fields → daemon calls
small-tier model with the fact content + a recent sample of
`(subject, category, kind, subsystem)` tuples for context → model
returns suggested values → daemon writes the fact with those values
and tags the fact metadata `auto_categorized=true` for later audit.

**Cost:** small. One prompt, no schema changes, no new tools.

**Risks:** drift in categorization standards over time. Mitigation:
the `auto_categorized` metadata flag lets a periodic audit re-evaluate.

**Depends on:** nothing.

**Status:** quality-of-life feature, easy win, but only matters if
human/CLI usage of `memory_store` grows. Currently most writes come
from Claude, which already categorizes well.

### Summary trigger generation

KG tier 2 introduces summary triggers (one curated précis per
file/cwd pattern, with linked detail facts). Writing those précis by
hand is the operational cost; a model can draft them.

**Workflow:** new tool `memory_propose_summary_trigger(pattern)`
collects facts that *would* fire on `pattern` today, asks the
big-tier model to draft a précis covering the durable invariants
across them and a link-rewire plan to the détail facts. User
approves, edits, or rejects.

**Depends on:** KG tier 2 summary triggers.

**Status:** ship once KG tier 2 summary triggers are real.

### Query decomposition / multi-step planner

The article that motivated this doc treats decomposition as Level 2
agentic RAG: an LLM breaks a query into sub-queries, walks the graph,
self-critiques, re-queries.

**Why this is *not* a top candidate** despite the framing: Claude is
already an excellent planner and is the caller. Putting a second
planner inside memstore creates two reasoning loops that may
disagree. The article's framing assumes the answering model is dumb
or absent; that assumption doesn't hold for Claude Code as the MCP
client.

**Where it might pay:** non-Claude callers. If memstore grows
clients that aren't strong reasoners (a CLI for humans, a small
worker script, a less-capable model embedded in another tool), a
server-side planner gives them the same retrieval quality Claude
gets for free.

**Status:** speculative. Don't build until a non-Claude caller with
real retrieval needs shows up.

### Natural-language fact editor

`memory_update` requires the caller to specify exactly which fields
to change. A natural-language interface ("add a note that this is
deprecated as of 2026-05") lets the model translate to the structured
update.

**Workflow:** new tool `memory_edit(fact_id, instruction)` →
daemon fetches the fact, calls small-tier model with `(fact, instruction)`,
gets back a diff or a full replacement, applies via existing
`memory_update` / `memory_supersede`.

**Risks:** silent corruption if the model misinterprets. Mitigation:
return the proposed change to the caller for confirmation rather
than applying immediately.

**Status:** quality-of-life. Low priority unless human/CLI usage
grows.

## The two-tier endpoint pattern

Most features above need either a small fast model or a big
analytical model. To keep the daemon's prompt sites simple, codify
two endpoints:

```
MEMSTORE_GEN_URL      → small-tier endpoint (Lemonade, 3B-7B model)
MEMSTORE_GEN_MODEL    → small-tier model name
MEMSTORE_GEN_BIG_URL  → big-tier endpoint (Lemonade, 30B-120B model)
MEMSTORE_GEN_BIG_MODEL → big-tier model name
```

Two `OpenAIGenerator` instances at startup, accessed via
`daemon.smallGen` and `daemon.bigGen`. Background jobs default to
big; per-call paths default to small. Either may be unset; features
that need a missing tier silently disable themselves (same gating
pattern KG uses for `GraphReader`).

Add to memstore conventions: features must declare which tier they
require, so a deployment without a big-tier endpoint can still run
the small-tier features and vice versa.

## Permissions

All features here inherit the same permission rules as KG (see
`docs/tier1-graph-basics.md` → "Permissions (forward-looking)") and
add one of their own:

- **Model context isolation.** The model never sees facts the caller
  is not authorized to see. For background workers, "the caller" is
  the namespace owner — workers run with full namespace access but
  must not leak across namespaces. A worker processing namespace A's
  fact never reads namespace B's facts into its prompt context, even
  if the model is the same instance.

- **Proposals are caller-scoped.** Link proposals, drift findings,
  consolidation proposals are all visible only within the namespace
  they were generated in. Multi-namespace deployments don't
  cross-pollinate.

## Operational concerns

- **Idempotency.** Background jobs must be idempotent. A worker that
  crashes mid-extraction and re-runs must not produce duplicate
  proposals. Use a `(fact_id, job_type, generation)` uniqueness
  constraint on the jobs table; bump generation only on explicit
  re-extract requests.
- **Backpressure.** A burst of `memory_store` calls shouldn't
  overwhelm Lemonade. Rate-limit the worker pool; queue depth is
  observable via the jobs table. Worst case the worker falls behind;
  facts are still stored, just unenriched.
- **Cost (CPU/wattage, not dollars).** Big-model background work has
  real power cost on halo. Default to off-hours scheduling for
  sweeps; foreground extraction can run continuously since it's
  triggered by user activity.
- **Observability.** Log every model call with fact_id, prompt
  tokens, response tokens, latency, model name. Track per-feature
  token totals so a runaway prompt is visible.

## Open questions

1. **Worker hosting.** In-process goroutine pool inside `memstored`,
   or a separate `memstore-worker` binary? In-process is simpler;
   separate is more resilient and lets the worker scale independently.
   Recommendation: in-process for the first feature; split later if
   the worker's failure modes start affecting daemon uptime.

2. **Proposal review surface.** One unified
   `memory_review_proposals(type=...)` tool covering link/drift/
   consolidation/etc., or one tool per type? Unified is fewer tools
   for the agent to learn; per-type is cleaner schemas. Recommendation:
   one unified tool with a discriminated `type` field, since most
   review workflows are "show me everything pending."

3. **Feedback loop.** When a proposal is rejected, should the model
   learn from that? Cheap version: log rejections, periodically
   audit; the model itself doesn't update. Expensive version: build
   a fine-tune dataset from accepted/rejected pairs. Recommendation:
   start with the cheap version; revisit if rejection patterns
   reveal a systematic prompt failure that re-prompting can fix.

4. **Drift detection scope.** Just code-referencing facts, or
   broader (URL freshness, project-status facts that mention dates,
   etc.)? Recommendation: start with code references because the
   detection signal is concrete (file mtime + grep). Generalize once
   the workflow proves out.

5. **Model selection per feature.** One big-tier model for everything,
   or different models for different jobs (e.g. a code-tuned model
   for drift, a general model for extraction)? Recommendation: one
   model to start; specialize when a feature visibly underperforms.

## Sequencing

Build order, assuming KG tier 1 has shipped:

1. Two-tier endpoint pattern + small/big generator wiring (groundwork
   for everything else).
2. **Write-time entity extraction → link proposals.** Big-tier feature.
   Highest-leverage. Dense the graph that KG just made queryable.
3. **Background drift sweep.** Big-tier feature. Independent of
   extraction; can ship in either order, but extraction's proposal
   infrastructure (review tools, proposal table shape) is the
   pattern drift findings should reuse.
4. *(KG tier 2 ships connected components / consolidation workflow
   in parallel.)*
5. **Memory-consolidation drafting.** Composes #2 + KG tier 2.
6. **Read-time re-ranker.** Only after building the eval set and
   confirming the lift.
7. Everything else, on demand.

## Out of scope

- **Server-side multi-step retrieval planner.** Already covered above
  ("Query decomposition / multi-step planner") and intentionally
  deprioritized — Claude is the planner today.
- **Server-side reflection on Claude's draft answers.** Same reason.
- **Auto-supersession.** Drift findings, link proposals, and
  consolidation proposals all surface for review. Memstore never
  silently rewrites a user-confirmed fact based on a model decision.
- **Embedding model swap via local LLM.** Embeddings come from
  go-embedding's configured embedder. Not in scope for this doc.

---

**Writeup reminder:** when the first feature here ships (likely
extraction → link proposals), write a homepage/blog post covering
the local-LLM architectural choice, the two-tier endpoint pattern,
and the proposal/review workflow. Worth contrasting against the
"agentic RAG" framing this doc was prompted by, since memstore
deliberately keeps agency in the caller (Claude) for read paths
and only adds it for background curation.
