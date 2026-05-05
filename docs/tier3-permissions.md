# Tier 3 — User / Group / Project Permissions

Status: placeholder, design not yet started
Author: Matthew + Claude
Date: 2026-05-05

Tier 3 introduces multi-user access control to memstore. Today the system
is effectively single-user (namespace-scoped at construction time); tier 3
adds a notion of caller identity and per-fact, per-link, and per-namespace
authorization. The model spans users, groups, and project-scoped grants
so a fact created in one project can be selectively shared without
collapsing namespace boundaries. Tier 1 (and by extension tier 2) has
already left forward-looking hooks for this work — a nilable `*Caller`
parameter on graph handlers, predicate-shaped recursive CTEs, and an
explicit "permissions are in-engine, never post-filter, and topology
leaks count as data leaks" rule. Detailed design deferred until this
phase begins.

---

**Writeup reminder:** when tier 3 ships, write a homepage/blog post
covering the threat model, the user/group/project model, how
permissions integrate with the graph layer (especially the topology-leak
rules), and the caller-identity sourcing decision across MCP transports.
