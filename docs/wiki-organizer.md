# The wiki organizer: the pattern applied to the fact corpus

Status: design, proposed 2026-08-30
Author: Matthew + Claude

Companion to `docs/document-synthesis.md`, which designed the wiki layer over
the document corpus. This doc is the same pattern pointed at the corpus we
actually have. Source: Karpathy's "LLM Wiki" gist, 2026-04-04
(https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f).

## Why this doc exists

`document-synthesis.md` builds the wiki on documents: stage one stores verbatim
chunks, stage two reads them and writes derived facts citing those chunks. Its
sequence therefore starts with an ingested corpus, and production holds one
document. Every step after step 1 is blocked on material that does not exist.

Meanwhile there are 4,858 live facts with a median content length of 136
characters, produced by session extraction over six months, and nothing reads
them except retrieval at query time -- which is the thing the pattern argues
against.

Those facts already occupy the pattern's raw-source slot. They are short,
atomic, append-and-supersede rather than rewritten, and they are what the
sessions produced rather than what anyone composed. The layer missing here is
not sources. It is the layer above them.

So the synthesis design re-points from documents to facts, and the schema cost
of doing so is one generalization: `memstore_fact_citations` gains a fact-to-fact
form alongside its fact-to-chunk form. Agents, pages-as-views, revise, lint, and
the reconciliation rules all transfer unchanged. When documents do arrive they
feed the same machinery from the other side.

## What the corpus looks like

Measured against production 2026-08-30, after the normalization and link
backfill work of PRs #196 through #204.

| subject size | subjects | facts |
|---|---|---|
| 1 | 2151 | 2151 |
| 2-3 | 312 | 703 |
| 4-10 | 118 | 628 |
| 11-50 | 16 | 354 |
| 50+ | 6 | 1022 |

The six largest are `todo` (352), `matthew` (223), `homelab` (154), `memstore`
(146), `oldschoolgamers` (84), `herald` (63).

Every fact is embedded (4,858 of 4,858) and the graph holds 9,293 links, so the
input to any clustering step is already in the database.

The distribution is bimodal and both ends fail the same test. A subject holding
one fact is not a page. Neither is a subject holding 352. Task 8409 names only
the first half; the six dumping grounds are 21% of the corpus and have not been
tracked as a defect at all.

## The schema layer

The pattern's third layer, and the one we have nothing resembling. Karpathy
describes it as "a document (e.g. CLAUDE.md for Claude Code or AGENTS.md for
Codex) that tells the LLM how the wiki is structured, what the conventions are,
and what workflows to follow", co-evolved with the model over time rather than
authored once.

Two adjustments for our case.

His schema tells the model how to lay markdown files out in directories. We have
no filesystem to describe, so the equivalent content is semantic: what a subject
is, what belongs on an existing subject versus what earns a new one, what the
`category` and `kind` vocabularies mean, and when a group of facts is large
enough to deserve a synthesis page.

And the index and the log are *not* part of the schema in his framing -- they
are separate special files the schema describes, the index content-oriented and
the log chronological and append-only. We already have both, as data rather than
as files: supersession chains plus `memstore_links` are the log, and an index is
a query over subjects. So our schema document is smaller than his, covering
conventions and workflows only.

Why it comes first: `category` and `kind` today are whatever each extraction run
improvised, and without a written schema every organizer run re-derives the
taxonomy and drifts from the last one. It is also the cheapest item here.

It lives in the repo, human-owned and human-edited, and the organizer prompts
read it.

## Operations

Ordered by dependency, not by value.

### 1. Agents, `agent_id`, and citations

Step 1 of the synthesis design's sequence, unchanged and still not skippable.
No model involved. Every operation below writes derived facts, and today a
derived fact is indistinguishable from one the user wrote by hand. Building the
generators before the provenance produces a second mess that is harder to clean
than the first.

The one addition is the fact-to-fact citation form described above.

### 2. Cluster and name: the singletons

Task 8409, inverted. The task as filed asks the model, for each fact on a
singleton subject, to propose the nearest existing subject. That is 2,151
independent decisions, each made without sight of the others, and it can only
ever match against subjects that already exist -- so it can attach facts to the
dumping grounds but can never discover that forty singletons are collectively
one missing topic.

Bottom-up instead: cluster the singleton vectors, hand the model each cluster,
and ask it to name the group and say whether that name is new or a spelling of
something already present. A couple hundred clusters is reviewable where 2,151
facts is not.

Output is a proposal queue. The operation writes nothing.

### 3. Cluster and name: the dumping grounds

The same operation pointed inward, run within a subject that has grown past a
threshold. Cluster inside `todo` or `homelab`, name the sub-groups, propose
child subjects.

`SubjectPattern` already admits `/` and `:`, so `homelab/network` is legal today
with no schema change.

This is where the recall improvement is largest: those six subjects are a fifth
of the corpus, and every query touching them currently competes against hundreds
of siblings.

Shares all its machinery with operation 2. Build once, point it at both ends of
the distribution.

### 4. Near-duplicate merge

Lint catches byte-identical content. The mass is above that: pairs that say the
same thing in different words. Candidate generation is a pgvector query over
existing embeddings and costs nothing; the model only adjudicates merge versus
distinct and proposes the supersession. Cheap enough to run nightly.

### 5. Synthesis pages over facts

The payoff, and the reason the rest is worth doing. For each subject with enough
live facts, one `kind = synthesis` fact citing the fact ids it drew on,
superseded on re-run so the supersession chain is the page history. Exactly the
page shape in `document-synthesis.md`, with fact citations instead of chunk
citations.

Today a recall on `homelab` returns a dozen disconnected one-liners. With a page
it returns one coherent account plus the atoms behind it.

Depends on operation 1 for citations and on 2 and 3 for subjects worth writing a
page about.

### 6. Contradiction lint

Task 8403, and much cheaper after 2 and 3: it runs per subject over a page's
worth of facts, so the model compares ten related claims rather than scanning a
corpus. Facts 3152 and 3398, which both describe memstore internals that 0.6.0
deleted, are the standing evidence that nothing currently notices.

Produces a flag, never a resolution. The reconciliation rules in
`document-synthesis.md` govern: a derived fact never overrules a user-authored
one.

### 7. Never-surfaced triage

1,968 facts, 41% of the corpus, never searched, injected, or confirmed. Not a
defect today -- the pattern has no archive concept and neither do we -- but
after 2 through 5 the question becomes answerable. A fact that lands in a named
cluster and gets cited by a page has been promoted. One still sitting alone and
unread after that is a genuine archive candidate.

Deliberately last, and measurement-driven.

## Sequence

1. The schema document, and operation 1's migration. Prose and a migration; they
   do not depend on each other and can land together.
2. The cluster-and-name tool, run over the singletons and the dumping grounds
   (operations 2 and 3). This is what turns `subject` into a usable grouping key,
   which operations 5 and 6 are both waiting on.
3. The lint queue in the web UI (task 8404, `docs/web-ui.md`).
4. Synthesis pages (operation 5), then contradiction lint (operation 6).
5. Near-duplicate merge (operation 4) whenever convenient; it is independent.

Operation 4 is sequenced last despite being independent because it is the one
step whose value does not compound with the others.

## The queue problem

Operations 2, 3, 4, and 6 all produce proposals for a person to accept or
reject, and lint already produces findings today. A CLI queue is a queue worked
by typing, which is the constraint this whole setup is built around. Task 8404
exists for that reason and it stops being deferrable the moment the first
cluster run finishes: a review queue nobody works becomes a second backlog of
the same size as the first.

## Not decided here

- **Clustering method.** Agglomerative over cosine distance is the obvious first
  cut and needs no new dependency beyond what pgvector gives us; whether it needs
  something with a density notion is a measurement, not a design choice.
- **The oversize threshold** at which a subject is split, and the minimum at
  which a subject earns a synthesis page. Both are numbers to pick after looking
  at real clusters.
- **Whether reassignment supersedes or updates.** Changing a fact's subject is
  not a new claim, so a supersession chain per reassignment may be noise; but the
  design's rule that a derived judgement never silently rewrites a user-authored
  fact points the other way. Decide with the schema document.
- **Prompt design** for naming and for synthesis, deferred to the same place
  `document-synthesis.md` left it.
