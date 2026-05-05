# Tier 1 Graph Features — Design

Status: draft, awaiting decisions on open questions
Author: Matthew + Claude
Date: 2026-05-05

## Goals

Add a small set of graph operations on top of the existing `memstore_links` edge
table so that agents can answer "what's near this fact?", "is there a connection
between A and B?", and "how connected is this fact?" without walking the graph
one `GetLinks` call at a time.

Targets pgstore only. SQLite implementation is out of scope; the Store interface
will gain capability interfaces that pgstore satisfies and the SQLite store does
not.

## Non-goals (explicitly tier 2, not now)

- PageRank, centrality, community detection, connected components.
- Cypher / OpenCypher query language (Apache AGE).
- Temporal edges, edge versioning, edge supersession.
- Graph visualization output (e.g. Graphviz, JSON for D3).
- Cross-namespace traversal.

## Background

Today (`store.go:97-182`, schema in `pgstore/store.go:149-162`):

- `memstore_links` is a directed edge table with `(source_id, target_id, link_type, bidirectional, label, metadata, created_at)`.
- Indexes exist on `(namespace, source_id)`, `(namespace, target_id)`, `(namespace, link_type)`.
- Edges may be marked `bidirectional`, in which case they participate in both
  inbound and outbound traversals.
- `metadata` is JSONB on pgstore. No GIN index today.
- `GetLinks(factID, direction, linkTypes...)` returns one hop. Anything deeper
  is the caller's problem.

Existing precedent for capability gating in MCP registration:
- `mcpserver/server.go:484` — `memory_curate_context` only registered if curator is non-NopCurator.
- `mcpserver/server.go:520` — `memory_rate_context` only registered if `sessionStore` is set.

Same pattern applies to graph tools.

## Scope

Tier 1 ships four MCP tools and the Store-side methods that back them:

| MCP tool                    | Store method        | Purpose                                       |
|-----------------------------|---------------------|-----------------------------------------------|
| `memory_get_neighborhood`   | `Neighborhood`      | All facts within N hops of a seed fact        |
| `memory_find_path`          | `ShortestPath`      | Shortest path (by edge count) between two facts |
| `memory_get_degree`         | `Degree`            | Inbound/outbound/total edge counts for a fact |
| `memory_get_subgraph`       | `Subgraph`          | Edges induced by a set of seed fact IDs       |

Plus one infrastructure addition:

- **JSONB GIN index on `memstore_links.metadata`** — enables efficient `metadata @>` filters during traversal without per-field schema changes.

## Design

### Capability interfaces (Store side)

Add to `store.go`, alongside `Store`:

```go
// GraphReader exposes multi-hop graph queries on the link graph.
// Implementations must enforce namespace scoping and respect
// bidirectional edges as participating in both directions.
type GraphReader interface {
    Neighborhood(ctx context.Context, seedID int64, opts NeighborhoodOptions) (Subgraph, error)
    ShortestPath(ctx context.Context, fromID, toID int64, opts PathOptions) (Path, error)
    Degree(ctx context.Context, factID int64, opts DegreeOptions) (DegreeCounts, error)
    Subgraph(ctx context.Context, seedIDs []int64, opts SubgraphOptions) (Subgraph, error)
}
```

Rationale: a single capability interface (rather than four) keeps the type
assertion at registration time simple. Tier 2 will add `GraphAnalyzer` etc. as
separate interfaces; if pgstore eventually grows AGE-backed methods, a
`CypherRunner` interface joins it.

> **Open question 1:** One capability interface vs. four separate ones?
> Recommendation: one. Splitting buys nothing today and complicates
> registration. Reconsider when tier 2 lands and we have heterogeneous
> implementations.

### Result types

```go
// Subgraph is the result of neighborhood/subgraph queries.
type Subgraph struct {
    Nodes []Fact // unique, includes the seed(s)
    Edges []Link // unique, only edges where both endpoints are in Nodes
}

// Path is an ordered traversal from one fact to another.
// Empty Nodes/Edges means no path exists within the depth bound.
type Path struct {
    Nodes []Fact // length N+1; first is fromID, last is toID
    Edges []Link // length N; Edges[i] connects Nodes[i] to Nodes[i+1]
}

type DegreeCounts struct {
    In         int            // edges where this fact is target (incl. bidirectional)
    Out        int            // edges where this fact is source (incl. bidirectional)
    Total      int            // unique edges touching this fact
    ByLinkType map[string]int // breakdown by link_type
}
```

> **Open question 2:** Should `Subgraph.Nodes` be full `Fact` objects or just
> IDs? Full facts are more useful (one round trip) but inflate response size at
> high depth. Recommendation: full facts, with a hard cap on result size (see
> Limits below). Agents that only want IDs can call `Degree` or paginate via
> repeated `GetLinks`.

### Options

```go
type NeighborhoodOptions struct {
    MaxDepth     int           // required, 1..MaxTraversalDepth
    Direction    LinkDirection // LinkOutbound | LinkInbound | LinkBoth
    LinkTypes    []string      // empty = all types
    NodeFilter   *FactFilter   // optional, prune walk by fact attributes
    MaxNodes     int           // hard cap; 0 = use server default
}

type PathOptions struct {
    MaxDepth     int           // required
    LinkTypes    []string
    Direction    LinkDirection // LinkOutbound = forward only; LinkBoth = treat as undirected
}

type DegreeOptions struct {
    LinkTypes []string // empty = count all
}

type SubgraphOptions struct {
    LinkTypes []string
    MaxNodes  int // 0 = server default; the seed set itself doesn't count toward the cap
}

// FactFilter prunes traversal in-engine.
type FactFilter struct {
    Subjects   []string
    Categories []string
    Kinds      []string
    Subsystems []string
}
```

> **Open question 3:** Filter-during-walk vs post-filter? The `NodeFilter`
> above prunes in-engine via JOIN against `memstore_facts`, which is faster but
> couples graph + content concerns. Alternative: return everything, let the
> agent filter. Recommendation: in-engine filter — at depth 3 with no filter,
> a moderately connected fact could explode to thousands of nodes. Pruning is
> the only way to keep results bounded for an agent.

### Permissions (forward-looking)

Memstore is single-user today but a multi-user permission system is planned.
This design must accommodate that without rework when it lands.

**Distinction enforced throughout:**

- *User-driven filters* (link types, future fact attribute filters) — caller's
  choice, cheap to apply at any layer. Post-filter is fine.
- *Permission filters* — mandatory, in-engine, applied before any result
  shape is observable to the caller. Topology, counts, edge existence, IDs,
  and even "did I get an error or an empty result" all leak signal. Leaks
  through the response shape are leaks.

**Design hooks (not implemented in tier 1):**

- The recursive CTE for `Neighborhood` and `ShortestPath` will gain a JOIN
  or `WHERE` predicate of the form `WHERE fact_visible_to($caller, fact_id)`
  applied at every step of the walk. Invisible facts are treated as
  nonexistent — the walk does not traverse through them and they do not
  appear in result counts.
- `ShortestPath` returns "no path" if any node on the otherwise-shortest
  path is invisible to the caller; the next-shortest visible path is
  returned only if it satisfies `MaxDepth`. This is correct behavior — a
  path through a redacted node leaks the existence and position of that
  node.
- `Degree` counts only edges to visible facts. ByLinkType breakdowns
  similarly.
- `Subgraph` from a seed set: invisible seeds are dropped silently (NOT
  errored — erroring on an invisible ID confirms the ID exists).
- The MCP layer will need a notion of caller identity, currently absent.
  Likely sourced from transport auth (HTTP Authorization for httpapi, OS
  user / connection identity for stdio MCP). Out of scope for tier 1, but
  graph handlers should accept a `*Caller` (nilable for now) so the wiring
  can land later without signature changes.

> **Open question (deferred to permission system design):** Should the
> permission predicate be a SQL function, a CTE-injected ID list, or
> embedded in `memstore_facts` via row-level security? Defer.

### Limits

```go
const (
    MaxTraversalDepth = 4    // beyond this trends toward connected-component queries
    DefaultMaxNodes   = 100  // soft cap; configurable per-call up to MaxMaxNodes
    MaxMaxNodes       = 1000
)
```

These are deliberately conservative. Bulk fact ingestion has not yet started;
once it has and link density grows, the caps should be revisited — easier to
raise than to tighten.

### Pgstore implementation

Recursive CTE for `Neighborhood` (BFS, depth-bounded, with cycle detection):

```sql
WITH RECURSIVE walk(fact_id, depth, path) AS (
    SELECT $1::bigint, 0, ARRAY[$1::bigint]
  UNION ALL
    SELECT
        CASE
            WHEN l.source_id = w.fact_id THEN l.target_id
            ELSE l.source_id
        END,
        w.depth + 1,
        w.path || CASE
            WHEN l.source_id = w.fact_id THEN l.target_id
            ELSE l.source_id
        END
    FROM walk w
    JOIN memstore_links l ON
        l.namespace = $2
        AND (
            -- outbound or bidirectional from w.fact_id
            (l.source_id = w.fact_id AND ($3 = 'out' OR $3 = 'both' OR l.bidirectional))
         OR (l.target_id = w.fact_id AND ($3 = 'in'  OR $3 = 'both' OR l.bidirectional))
        )
        AND ($4::text[] IS NULL OR l.link_type = ANY($4))
    WHERE w.depth < $5
      AND NOT (CASE
            WHEN l.source_id = w.fact_id THEN l.target_id
            ELSE l.source_id
        END = ANY(w.path))
)
SELECT DISTINCT fact_id FROM walk;
```

Then a second query collects the induced edges and joins to `memstore_facts`
(applying `NodeFilter` if present). Cycle detection is via `path` membership;
PG's native `CYCLE` clause is an alternative but the array-membership form is
explicit and works on older PG versions.

`ShortestPath` is the same shape with two changes: stop at the first depth that
reaches `toID`, and reconstruct the path from the `path` array.

`Degree` is a single non-recursive query:

```sql
SELECT
    link_type,
    COUNT(*) FILTER (WHERE target_id = $1 OR (bidirectional AND source_id = $1)) AS in_count,
    COUNT(*) FILTER (WHERE source_id = $1 OR (bidirectional AND target_id = $1)) AS out_count
FROM memstore_links
WHERE namespace = $2 AND (source_id = $1 OR target_id = $1)
  AND ($3::text[] IS NULL OR link_type = ANY($3))
GROUP BY link_type;
```

`Subgraph` (induced from a seed set) is one query joining `memstore_links` to
`memstore_facts` filtered by `seed_id = ANY($1)`.

### Schema changes

One migration, `pgstore` V3:

```sql
CREATE INDEX IF NOT EXISTS idx_memstore_links_metadata
  ON memstore_links USING GIN (metadata);
```

Per the storage invariant (id=3111): bump `schemaVersion` from 2 to 3 in
pgstore, add `migrateV3()`, wire it into `migrate()`. No table changes — only
the GIN index.

Per id=599: no `factColumns`/`scanFact` change since we're not touching the
facts table. Transfer is unaffected (links don't go through `ExportedFact`).

Decision: ship the GIN index in tier 1. Concrete near-term uses include
filtering edges by traversal predicates (e.g. "passage requires X" in
graph-shaped narrative content) and filtering edges by source attribution
(e.g. distinguishing scientific-study citations from news citations on
medical facts). Memstore usage is read-biased, so the GIN write-amplification
cost is doubly tolerable.

### MCP tool surface

Four new tools, registered in `mcpserver/server.go` after the type assertion:

```go
if g, ok := ms.store.(memstore.GraphReader); ok {
    mcp.AddTool(s, &mcp.Tool{
        Name: "memory_get_neighborhood",
        Description: `Return all facts within N hops of a seed fact, plus the edges connecting them.

Use this when you have a fact and want to know what's adjacent to it in the
knowledge graph — supporting facts, related decisions, dependent concepts.

depth: 1-4. depth=1 is equivalent to memory_get_links + the linked facts.
direction: "out" (follow source→target), "in" (follow target→source), "both" (default).
link_types: optional list to constrain which edge types to follow.
max_nodes: cap on returned nodes (default 100, max 1000).

Returns a subgraph: {nodes: [FactSummary, ...], edges: [Link, ...]}.
Node entries are summaries (id, subject, category, kind, subsystem, metadata,
content_preview); call memory_get for full content.
`,
    }, ms.HandleGetNeighborhood(g))

    mcp.AddTool(s, &mcp.Tool{Name: "memory_find_path",     ...}, ms.HandleFindPath(g))
    mcp.AddTool(s, &mcp.Tool{Name: "memory_get_degree",    ...}, ms.HandleGetDegree(g))
    mcp.AddTool(s, &mcp.Tool{Name: "memory_get_subgraph",  ...}, ms.HandleGetSubgraph(g))
}
```

Decision: verb-prefixed names for consistency with the existing tool surface
(`memory_get_context`, `memory_get_links`, `memory_check_drift`, etc.). All
existing tools start with a verb; the graph tools follow that pattern.
`find_path` rather than `get_path` since "get path" is ambiguous and `find`
hints at search semantics.

Decision: shortest-only for tier 1. All-paths (`memory_find_paths`) is
deferred to tier 2 — see `docs/tier2-graph-analytics.md`. The query-cost risk of
all-paths is real and the use case is exploratory rather than core; ship
it when actual usage demands it, not preemptively.

### Trigger fact

Add a trigger fact (subject="memstore", category="project", kind="convention",
subsystem="links") that auto-loads when these tools are used or when graph
files are edited:

```
Tier 1 graph operations live behind the GraphReader capability interface
(only pgstore implements). MCP tools: memory_get_neighborhood,
memory_find_path, memory_get_degree, memory_get_subgraph. Hard caps:
depth ≤ 4, default 100 / max 1000 nodes per response. Bidirectional edges
count in both directions. Cycle detection is via path-array membership in
the recursive CTE.
```

> **Open question 8:** Is a trigger fact the right surface, or should this go
> in CLAUDE.md? CLAUDE.md is loaded every session whether relevant or not;
> trigger facts load on demand. Recommendation: trigger fact, since graph
> operations are a niche subsystem.

## Test plan

Tests live in `pgstore/store_test.go` (new section) and `mcpserver/server_test.go`.

Fixture: a small, hand-built graph with known structure:

```
   A ──ref──> B ──ref──> C ──ref──> D
   │                      ▲
   └──ref──> E ──ref──────┘
   F  (isolated)
   G <══bidir══> H
```

Cases:

1. **Neighborhood depth bounds** — depth=1 from A returns {A,B,E}; depth=2 returns {A,B,C,E}; depth=3 returns {A,B,C,D,E}.
2. **Direction filtering** — neighborhood of D with direction=in,depth=2 returns {C,D,E} (and B at depth=3); with direction=out returns just {D}.
3. **Bidirectional edges** — neighborhood of G includes H regardless of direction.
4. **Cycle handling** — add C→A edge; neighborhood of A doesn't loop forever.
5. **Link type filter** — add an edge with link_type="event"; verify it's excluded when link_types=["ref"].
6. **Shortest path** — A→D returns [A,B,C,D] (length 3) or [A,E,C,D] (length 3) — accept either.
7. **No path** — A→F returns empty.
8. **Path depth cap** — A→D with max_depth=2 returns empty.
9. **Degree counts** — verify In/Out/Total/ByLinkType for B (in=1, out=1) and bidirectional G (in=1, out=1, total=1).
10. **Node filter pruning** — exclude facts with subject="X" mid-walk; downstream nodes only reachable through X are excluded.
11. **MaxNodes cap** — synthetic large graph, request with max_nodes=10, verify exactly 10 returned and no error.
12. **Namespace isolation** — fixture in namespace="A", query in namespace="B" returns empty.
13. **MCP registration** — server backed by sqlite store (not GraphReader) does not advertise the four tools; pgstore-backed server does.

## Out of scope, deferred to tier 2

- PageRank, centrality, Louvain, connected components.
- All-paths, k-shortest-paths.
- Cypher (AGE).
- Edge weights / weighted shortest path.
- Materialized neighborhood caches.
- CLI surface (`memstore neighborhood ...` etc.) — MCP-only for tier 1; add CLI when there's a non-agent caller.
- SQLite implementation — sqlite users do not get graph tools, by design.

## Decisions

All open questions resolved 2026-05-05:

1. **Capability interface:** single `GraphReader`. Split when tier 2 introduces a backend with partial coverage.
2. **Subgraph node shape:** fact summaries — id, subject, category, kind, subsystem, metadata, content_preview. No embedding, no full content. Caller fetches full content via `memory_get` for the few facts they need.
3. **Node filtering:** post-filter only (user-driven). `NodeFilter` dropped from `NeighborhoodOptions`. Permission filters are separate and always in-engine — see "Permissions (forward-looking)".
4. **Caps:** `MaxTraversalDepth=4`, `DefaultMaxNodes=100`, `MaxMaxNodes=1000`. Revisit after bulk fact ingestion lands.
5. **GIN index on `memstore_links.metadata`:** ship in V3. Concrete uses (traversal predicates, source-attribution filtering); usage is read-biased so write-amplification cost is fine.
6. **Tool names:** verb-prefixed for consistency — `memory_get_neighborhood`, `memory_find_path`, `memory_get_degree`, `memory_get_subgraph`.
7. **Path enumeration:** shortest-only in tier 1. All-paths deferred to tier 2 (see `docs/tier2-graph-analytics.md`).
8. **Convention surface:** trigger fact, slotted into the existing `links` subsystem. No CLAUDE.md change.
9. **Migration deployment:** standard `CREATE INDEX` (Option A). Memstore is personal infrastructure; brief write-lock during migration is acceptable. Revisit only if a "can't pause writes" constraint emerges.

Ready for implementation.

---

**Writeup reminder:** when tier 1 ships, write a homepage/blog post
covering what was built (the four graph tools, the capability-interface
pattern, the permission seams), the design rationale, and a worked
example showing what graph queries enable that flat fact recall didn't.
