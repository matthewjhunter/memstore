# Tier 2 Graph Features — Menu

Status: menu of candidates, not a committed roadmap
Author: Matthew + Claude
Date: 2026-05-05

This document collects the deeper graph-analytics features that were
intentionally deferred from tier 1 (`docs/tier1-graph-basics.md`). Tier 2 is a menu
of candidate features, not a committed roadmap. Items move from this doc into
their own design when there's evidence of need from real usage.

## Triage at a glance

| Feature                          | Build | Ops | Value     | Notes                                           |
|----------------------------------|-------|-----|-----------|-------------------------------------------------|
| Connected components             | S     | S   | high      | Cheapest signal toward consolidation; ship early|
| Memory-consolidation workflow    | M     | M   | high      | Composes existing pieces — see dedicated section|
| Path enumeration (`find_paths`)  | S     | S   | medium    | Direct extension of tier 1 shortest path        |
| Edge lifecycle (supersede + temporal) | M | S   | medium    | One coherent design; touches schema             |
| Edge weights + weighted path     | S     | S   | medium    | Trivial once a `weight` convention exists       |
| Importance ranking (PageRank)    | M     | M   | speculative | Wait for link density evidence                |
| Community detection (Louvain)    | L     | M   | high-if-true | Killer feature *if* clusters emerge          |
| Graph visualization output       | S     | S   | medium    | Curation aid; no DB changes                     |
| CLI surface                      | S     | S   | low-now   | Add when a non-agent caller appears             |
| Cypher / Apache AGE              | L     | L   | medium    | Big bet; reach for it only when CTEs hurt       |
| KuzuDB backend swap              | XL    | L   | low       | Documented for completeness; do not pursue      |
| Materialized neighborhood caches | M     | M   | low-now   | Premature until tier 1 latency is measured      |

Build = engineering effort. Ops = operational complexity (deps, refresh, monitoring). Value = expected payoff *given memstore's actual mission*, not abstract graph-DB merit.

## When to start tier 2

Tier 2 work is gated on signal from tier 1. Worth revisiting when one or more
of these are true:

- Link density is high enough that `memory_get_neighborhood` results are
  routinely hitting `MaxNodes=1000` (often a downstream effect of bulk
  ingestion landing — see `docs/tier4-bulk-ingestion.md`).
- Concrete, repeated agent or user requests for an operation that tier 1
  doesn't cover (path enumeration, importance ranking, cluster discovery).
- A workflow emerges where the agent would benefit from precomputed graph
  structure (e.g. "give me the most central facts about subject X").

Without that signal, tier 2 is speculative work and should not start. Tier 2
also has a hard dependency: the link graph must be *non-trivial* — see
the bulk ingestion doc for why ingestion that creates facts without links
leaves graph analytics with nothing to compute over.

## Candidate features

### Path enumeration — `memory_find_paths` (plural)

Deferred from tier 1 (Q7). All distinct paths between two facts up to a
depth and result-count cap, rather than just the shortest.

- **Use case:** "How do these two facts/entities relate?" — different paths
  often go through different intermediate concepts, which is meaningful for
  exploratory and narrative work.
- **Risk:** Combinatorial blowup. Path counts can grow exponentially with
  depth in dense graphs. Hard caps on `MaxDepth` and `MaxPaths` are
  mandatory.
- **Permissions:** Paths through invisible nodes drop silently (consistent
  with tier 1 rules).
- **Implementation:** Recursive CTE without early termination, plus
  `LIMIT MaxPaths`. Sort by path length ascending so the cap keeps the
  most useful results.
- **Status:** Not started. Add when an actual use case appears.

### Edge weights and weighted shortest path

Currently link metadata is opaque to graph queries. Adding a recognized
`weight` field would enable Dijkstra-style shortest path.

- **Use case:** Confidence-weighted reasoning, cost-of-traversal modeling.
- **Implementation choices:** Promote `weight` to a column (faster) or
  query via JSONB (slower, no schema change). pgRouting's `pgr_dijkstra`
  is an option but adds a heavyweight dependency for one algorithm.
- **Status:** Speculative. No concrete demand yet.

### Importance ranking — PageRank / centrality

Compute per-fact importance scores from graph structure. Surfaces "what's
central" without the agent needing to ask.

- **Implementation choices:**
  - In-Go via `gonum/graph` — load the (filtered) edge list into memory,
    compute, write back to a `memstore_fact_metrics` table. Fine to ~100K
    edges, recompute on schedule.
  - `pg_graphblas` extension — sparse-matrix algorithms in PG. Niche,
    operationally heavier.
  - Apache AGE — Cypher's PageRank function, if AGE lands.
- **Use case:** Memory consolidation candidate ("these facts are central,
  consider summarizing the cluster"), retrieval ranking signal beyond
  pure embedding similarity.
- **Status:** Speculative. Needs link density evidence before it's worth
  the operational complexity.

### Community detection — Louvain / Leiden

Find tight clusters of facts that could be summarized into higher-order
facts. The killer feature for memory consolidation.

- **Implementation:** No native PG support; gonum doesn't ship Louvain
  either. Likely candidates: a Go port of Louvain, or shell out to a
  Python tool (networkx) periodically.
- **Use case:** Suggest "fact summarization" — when a cluster of
  N small facts share a topic, propose superseding them with one
  consolidated fact. This is the highest-value tier 2 feature for
  memstore's actual mission.
- **Status:** Speculative but interesting. Worth a small spike to
  estimate cluster quality on real fact data once link density is up.

### Connected components — `memory_find_components`

Identify isolated subgraphs. Useful for finding "memory islands" — facts
or clusters that aren't linked into the broader knowledge graph.

- **Implementation:** Recursive CTE in PG (union-find via repeated
  traversal), or in-Go via `gonum`. Cheap operation.
- **Use case:** Diagnostic ("you have 47 disconnected clusters") and
  the cheapest possible *consolidation signal* — small isolated clusters
  are exactly the candidates for fact summarization or linking. This
  doesn't require Louvain to be useful; it's a poor-man's community
  detector that works on day one.
- **Output shape:** `{component_id: [fact_ids...]}` with size and
  representative-fact (highest-degree) for each component.
- **Status:** Strong candidate for tier 2's first ship — high
  signal-to-cost ratio, no operational complexity.

### Cypher query language — Apache AGE

Embed Apache AGE in pgstore to support OpenCypher queries. Trades pure
SQL for graph-native query syntax.

- **Pros:** `MATCH (a)-[:REL*1..3]->(b) WHERE ...` is dramatically more
  expressive than recursive CTEs for non-trivial graph patterns. Cypher
  is well-known.
- **Cons:** AGE is an additional PG extension to install/maintain. Its
  Cypher coverage is a subset; project has had uneven maintenance —
  needs current health check before betting on it. Expanding the MCP
  tool surface to expose Cypher to agents is a separate large design
  question (do agents write Cypher? a DSL on top? canned templates?).
- **Status:** Defer until a graph pattern surfaces that's painful to
  express as a recursive CTE.

### KuzuDB as alternative backend

KuzuDB is an embedded columnar graph database that speaks Cypher and
outperforms PG-based graph queries at scale.

- **Pros:** Native graph performance, Cypher built in, embedded
  (no separate process).
- **Cons:** Backend rewrite. Splits the storage strategy. Operationally
  duplicates pgstore. Memstore's scale (thousands to low-millions of
  facts) almost certainly doesn't justify the overhead.
- **Status:** Not recommended unless memstore grows past PG's practical
  ceiling for graph queries. Document as an option, don't pursue.

### Materialized neighborhood caches

For frequently-queried seed facts, precompute and cache the N-hop
neighborhood as a materialized view, refreshing on a schedule or on link
churn.

- **Use case:** Hot facts (e.g. the user's primary identity, or a
  central project node) that get queried every session. Avoid re-walking
  the graph each time.
- **Implementation:** PG materialized views with concurrent refresh, or
  a memstore-managed cache table updated by triggers on `memstore_links`.
- **Status:** Premature optimization until tier 1 latency is measured
  and found wanting.

### Edge lifecycle — supersession + temporal validity

Today edges are immutable except via delete + re-create, and have no
notion of validity over time. These two gaps form a single design
problem: how does an edge change?

- **Supersession:** edges grow a nullable `supersedes` column matching
  the facts pattern. `UpdateLink` (or a new `SupersedeLink`) preserves
  the prior version in a history table or via a `superseded_at`
  timestamp. Existing `UpdateLink` mutates in place — that probably
  needs to change.
- **Temporal validity:** edges grow nullable `valid_from` and
  `valid_until` columns. Queries default to "valid right now"; an
  optional `as_of` parameter on graph operations enables historical
  queries.
- **Combined use cases:** narrative timelines (a character's
  relationships over story time), evolving project structure (a
  repo's dependency on another repo, valid for a release window),
  changing confidence in a relationship as evidence accumulates.
- **Cost:** Schema change to `memstore_links` (V4 migration), query
  rewrites in every graph operation to filter by current time or
  `as_of`, history preservation rules. Touches all four tier 1 tools.
- **Why bundle them:** if you ship supersession alone you re-litigate
  the schema when temporal lands. If you ship temporal alone you can't
  cleanly model "this edge is wrong, replaced by that one" — there's
  no successor relationship. Together they're one coherent edge-lifecycle
  story.
- **Status:** Real and meaningful, but bigger than it looks. Needs its
  own design doc when scoped. Don't start until either (a) a use case
  for `as_of` queries appears, or (b) edge metadata starts churning
  enough that lossy `UpdateLink` becomes painful.

### Graph visualization output

Render a subgraph as Graphviz DOT, JSON for D3/Cytoscape, or similar
for human inspection.

- **Use case:** Curation, debugging, "show me what memstore actually
  knows about X."
- **Implementation:** Pure formatting layer over `Subgraph` results —
  no DB changes.
- **Status:** Cheap and self-contained. Could ship as a CLI command
  (`memstore graph render --seed X --depth 2 --format dot`) without
  affecting the MCP surface.

### CLI surface

Tier 1 is MCP-only. Tier 2 should add CLI commands for graph queries
once a non-agent caller (script, human exploration) materializes.

- **Status:** Add when needed. Trivial to wire on top of the Store
  interface.

## The memory-consolidation workflow

The single highest-value thing the graph layer enables isn't any one
algorithm — it's a *workflow* that composes pieces from across memstore
to make memory self-curating. Calling it out explicitly because it's
the strategic case for several tier 2 features.

**The loop:**

1. **Detect a candidate cluster.** Either via connected components
   (cheap, isolated clusters jump out) or community detection (more
   sophisticated, finds tightly-knit clusters inside the main component).
2. **Score the cluster.** Is it small enough to summarize (3-15 facts)?
   Are the facts genuinely on-topic together (vector similarity over
   their content)? Are they aging (last-confirmed timestamp old)?
3. **Propose a consolidated fact.** Use the existing curator/generator
   layer to draft a single higher-order fact summarizing the cluster.
4. **Supersede.** New fact replaces the cluster members via `supersedes`
   (existing fact-supersession), with link rewiring (every edge into a
   superseded fact gets a new edge into the consolidated one).
5. **Surface for human approval.** Memstore should not auto-apply this;
   propose via a tool the agent can call (`memory_propose_consolidation`)
   that returns the candidate cluster, the draft fact, and the link
   rewiring plan. The user (or a curator agent) approves or edits.

**Why this matters strategically:** memstore today only grows. Without
consolidation it accumulates noise — old project facts that are 80%
overlapping, abandoned-project debris, near-duplicate notes from
different sessions. Search and recall get noisier over time. Consolidation
is how memstore stays useful as it ages.

**What's needed to ship it:**

- Connected components or Louvain (graph layer).
- Cluster scoring (combines existing embedding similarity, fact metadata).
- Proposal generation (existing curator/generator).
- Link rewiring on supersession (extension to the existing fact
  supersession path — currently doesn't touch the link graph).
- A new `memory_propose_consolidation` MCP tool.
- An approval surface (CLI or MCP tool to accept/reject a proposal).

**Recommended ordering:** ship connected components first, even before
community detection. It will surface the cheapest consolidation
candidates (isolated clusters) and let us validate the workflow end-to-end
before adding Louvain's complexity.

## Link type vocabulary

`link_type` is a free-form string column today. Tier 1's graph
operations all accept `link_types` as a filter, which means agents are
about to start *querying* by link type — and the moment that happens,
inconsistency hurts. Two facts that should both be `"references"` but
are stored as `"reference"` and `"refs"` are now invisibly different.

- **Use cases driving this:** any tier 2 analytic that filters by
  link type (most of them). The TTRPG/world-building case (link_type
  encoding traversal predicates like "halfling-passable"). The
  citation-source case (link_type or metadata distinguishing
  scientific-study vs news citation).
- **Options:**
  - **Curated enum** — define a small set of canonical link types,
    reject others. Loses flexibility, hard to extend.
  - **Convention + lint** — keep free-form, but ship a
    `memory_list_link_types` tool and a "near-duplicate" warning when
    creating a link with a type close to an existing one. Cheap, soft.
  - **Per-namespace registries** — namespaces declare their link
    vocabulary. Heavier, more correct.
- **Status:** No urgency until tier 2 analytics begin filtering by
  link type. Worth deciding then. The convention+lint option is the
  least disruptive default.

## Permissions

All tier 2 features inherit the same permission rules as tier 1
(see `docs/tier1-graph-basics.md` "Permissions (forward-looking)"):

- Permission filters are mandatory and in-engine.
- Topology, counts, edge existence, IDs, and error/empty distinction all
  leak signal — must not depend on invisible facts.
- For analytics (PageRank, communities), invisible facts are excluded
  from the input graph entirely. Scores returned are computed over the
  visible subgraph only.

## Forward-looking hooks already in tier 1

Tier 1 deliberately left seams for tier 2 to slot into. When tier 2
work starts, these are the existing hooks to use rather than
re-litigating:

- **`*Caller` parameter on graph handlers** — tier 1's design specifies
  that MCP graph handlers accept a nilable `*Caller` (currently unused)
  so the multi-user permission system can wire in without signature
  changes. Tier 2 graph handlers must accept the same `*Caller`.
- **Permission predicate seam in recursive CTEs** — tier 1's CTEs are
  shaped to accept a `WHERE fact_visible_to($caller, fact_id)` JOIN
  predicate. Tier 2 CTEs should follow the same shape so the same
  predicate slots in everywhere at once.
- **GIN index on `memstore_links.metadata`** — shipped in tier 1's V3
  migration, unused by tier 1 itself. Tier 2 features that filter on
  edge metadata (weights, source-attribution, traversal predicates)
  inherit the index for free.
- **`GraphReader` capability interface** — the type-assertion gating
  pattern at MCP registration is the precedent for tier 2 capability
  interfaces. New capabilities should follow the same pattern (separate
  interface, separate registration block) rather than widening
  `GraphReader`.

## Architectural decisions (open)

- **Where do analytics live?** In-Go (gonum), in-PG (pgRouting,
  pg_graphblas, AGE), or a sidecar (Python + networkx)? Likely a
  per-feature decision rather than a single bet, but a default
  preference would simplify reasoning. Provisional default: in-Go via
  `gonum` for anything Go-supported, fall back to a sidecar only when
  gonum can't help (Louvain). Avoid PG extensions — they're operational
  weight that locks the backend.
- **Refresh model for derived metrics?** On-demand (compute per query),
  scheduled (cron-style refresh of a metrics table), or event-driven
  (recompute on link churn)? Different features will want different
  answers. Provisional default: scheduled, with an explicit
  `memory_refresh_metrics` MCP tool for forced refresh. Event-driven
  on every link write is too chatty; on-demand is too slow for
  algorithms that scan the whole graph.
- **Capability interface split?** Tier 1 uses a single `GraphReader`.
  Tier 2 likely adds `GraphAnalyzer` (metrics), `GraphPathfinder`
  (path enumeration), maybe `CypherRunner` (AGE). Decide split when
  the first tier 2 feature lands.
- **Where does the `Caller` come from in MCP?** Tier 1 plans for a
  nilable `*Caller` parameter but doesn't define the source. Options:
  HTTP Authorization header (httpapi), OS user / connection identity
  (stdio MCP), explicit `caller_id` in tool input (insecure but
  simple). Decide as part of the permission system design (see
  `docs/tier3-permissions.md`), not in isolation.

---

**Writeup reminder:** when tier 2 features ship, write a homepage/blog
post covering what was built, why these features over the deferred
ones, and what consolidation/analytics workflows they unlock in
practice.
