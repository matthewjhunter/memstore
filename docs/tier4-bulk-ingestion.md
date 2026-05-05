# Tier 4 — Bulk Ingestion

Status: placeholder, design not yet started
Author: Matthew + Claude
Date: 2026-05-05

Tier 4 covers bulk fact ingestion — the ability to load large quantities
of pre-existing material (notes, transcripts, prose, code commentary,
research) into memstore in a single operation, rather than one fact at
a time during interactive use. The critical design constraint is that
bulk ingestion must *create links*, not just facts: an import that lands
10K facts as 10K isolated nodes leaves the graph layer no richer than
today, and tier 2 analytics (PageRank, communities, neighborhoods)
become uniformly meaningless over isolated nodes. The extraction
subsystem's link-creation strategy during bulk ingestion is therefore
a hard prerequisite for tier 2's value — even if tier 4 is scoped and
shipped before tier 2 starts. Detailed design deferred until this phase
begins.

---

**Writeup reminder:** when tier 4 ships, write a homepage/blog post
covering the ingestion pipeline, the link-creation heuristics that
make the resulting graph non-trivial, the source-provenance model, and
worked examples of typical bulk imports (e.g. a notes archive, a
transcript backlog).
