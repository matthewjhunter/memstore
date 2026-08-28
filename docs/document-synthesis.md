# Document synthesis: the wiki, in memstore's data structures

Status: design, settled 2026-08-27
Author: Matthew + Claude

Companion to `docs/document-corpus.md` (the verbatim layer this builds on) and
`docs/web-ui.md` (the surface that makes it visible). Origin: a design
discussion of Karpathy's "LLM Wiki" pattern (gist, 2026-04-04) against what
memstore already has.

## The pattern, and what memstore already had

Karpathy's pattern has three layers: immutable raw sources a human curates,
an LLM-owned wiki of summary, entity, and concept pages compiled from them, and
a schema document that says how the wiki is organised. Three operations keep it
alive: ingest (read a source, write its summary, revise every page it touches),
query (read the index, answer with citations, file good answers back), and lint
(find contradictions, stale claims, orphans). The claim is that the tedious part
of a knowledge base was never the reading or the thinking but the bookkeeping,
and that is the part an LLM makes free.

Memstore has the first two layers with stronger guarantees than flat markdown:
the document corpus is verbatim by a checkable invariant rather than by trust,
and the fact layer has supersession, links, confirm counts, and hybrid recall,
which is roughly the list of things the pattern's early adopters found they had
to bolt on. What memstore lacked was the operation joining them. Documents were
ingested into a searchable corpus and nothing read them: retrieval at query
time, which is the thing the pattern argues against.

Two things from the pattern were passed over in that comparison and matter
most. A markdown wiki is browseable by a human, and a human can edit it. Those
are requirements here, not conveniences. The wiki is memstore: derived facts
are the pages, the corpus is the source they link back to, and the web UI is
how a person reads, searches, corrects, and removes them.

## The rule

Ingest has two stages.

**Stage one** stores the source verbatim. The client uploads bytes and asserted
git metadata; the daemon chunks mechanically, records `file_sha256`, and
verifies that every chunk equals `bytes[start:end]`. No model is involved. This
is `docs/document-corpus.md` unchanged.

**Stage two** is LLM-driven analysis over stage one's chunks. Its output is
facts -- summaries, entities, concepts, cross-references -- each citing the
chunks it was built from. Stage two is part of the ingest process and never
writes to the document corpus. The corpus invariant is what makes it safe to
run: whatever the model gets wrong, the source is there to check against, and
the page always links to it.

The old prohibition in the corpus doc ("no background job that distills
documents into the fact graph") was protecting attributability, and this design
keeps the protection while dropping the prohibition: every derived fact records
who it was produced for, what produced it, and what it was produced from.

## Provenance: owner, agent, citations

A derived fact carries three things beyond an ordinary fact.

**Owner.** The user the analysis ran on behalf of -- the ingesting user, in the
single-user case. This is the existing `user_id` on `memstore_facts`; nothing
new. Visibility, scope, and any later multiuser predicate follow the owner
exactly as for a fact the user wrote by hand. There is no synthesis system
user. Stage two runs daemon-internal on behalf of a user, the same way the
extraction queue does today, and holds no credential of its own.

**Agent.** A record of *how* the fact was produced: model, prompt, and a role
name. It is not a principal: it cannot own anything or authenticate. It is a
reproducibility record. Stored once and referenced by id, so the prompt text
lives in one place and a change to it mints a new agent rather than silently
changing what old facts mean.

    memstore_agents
      id            bigserial PK
      namespace     text     NOT NULL
      role          text     NOT NULL   -- "summarizer", "entity-extractor", "linter"
      model         text     NOT NULL   -- as configured, e.g. "qwen2.5:14b"
      prompt_sha256 bytea    NOT NULL
      prompt        text     NOT NULL   -- kept verbatim so a fact's origin is readable
      created_at    timestamptz NOT NULL

      UNIQUE (namespace, role, model, prompt_sha256)

Two users with different summarizer prompts for the same document produce two
derived facts: different owners, different agents, the same citations. They are
siblings, not a supersession chain. Which agent runs for which user is a
per-user configuration (`user -> agent` per role); the default deployment has
one row per role, and multiuser configuration of it is out of scope here but
not precluded.

**Citations.** The chunk ids the fact was built from. Required: the store rejects a
derived fact with no citations.

    memstore_fact_citations
      fact_id       bigint   NOT NULL REFERENCES memstore_facts(id) ON DELETE CASCADE
      chunk_id      bigint   NOT NULL REFERENCES memstore_document_chunks(id) ON DELETE CASCADE
      namespace     text     NOT NULL
      user_id       bigint   NOT NULL      -- denormalized, same reason as the corpus tables

      PRIMARY KEY (fact_id, chunk_id)

`ON DELETE CASCADE` from chunks is deliberate and is the staleness signal: when
a document is re-ingested at a new hash its chunks are replaced, the citations
vanish, and a derived fact whose citation count drops to zero is by definition
stale. Lint finds those mechanically, without a model call.

On `memstore_facts` itself this needs one column, `agent_id bigint NULL
REFERENCES memstore_agents(id)`, subject to the four-place update rule
(`factColumns`, `scanFact`, `searchFTS`, `ExportedFact`). A fact with a
non-null `agent_id` is derived; a fact with a null one was written by the owner
through an ordinary write path. That distinction is the whole reason the column
exists. It answers the corpus doc's opening complaint that provenance was
recorded in model-writable metadata.

Session extraction fits the same shape once it adopts it: owner is the session's
user, agent is `(model, extraction prompt, "extractor")`, citations are empty or
a transcript reference. That replaces `{"source":"session"}` in metadata with
the same mechanism rather than a second one; it is not part of this design's
first delivery but should not be built any other way.

## Pages

A page is a view, not a stored object. The page for subject S is assembled at
read time from:

1. the current synthesis fact for S, if one exists: `kind = synthesis`, derived,
   citing chunks and linking to the facts it drew on;
2. the live facts on S, derived and user-authored alike, with their provenance
   shown;
3. the documents whose chunks are cited by any of the above.

The synthesis fact is superseded on every ingest that touches S, which is the
"rewrite on ingest" behaviour that append-only wikis were found to need, and
the supersession chain is the page history. No new object type, and the page
for a subject with no synthesis fact is still a page -- just the facts and
documents, which is what `memory_get_context` returns today.

Per-source summary pages are the same shape with the document as the subject:
a synthesis fact whose citations cover the document's chunks.

## Operations

**Summarize (per ingest).** When stage one lands a document, stage two writes
or supersedes its summary fact and extracts the entities and concepts it names
as candidate facts. Bounded by one document, so it can run inline on the ingest
queue and the upload visibly did something. On a repo-scale sync this is one
call per changed file, which is the same order as the chunking work.

**Revise (scheduled).** For each subject touched by recent summaries, rewrite
the subject's synthesis fact against its current live facts and citations.
Scheduled rather than per-ingest because a subject can be touched by many
documents and a commit that changes forty files should not fan out into forty
rewrites of the same page. Nightly is the pattern's own cadence and a
reasonable default.

**Lint (scheduled).** Per subject, without a model where possible:

- derived facts with zero remaining citations (source re-ingested or removed);
- live facts on the same subject that contradict -- the one check that needs a
  model, and it produces a flag, never a resolution;
- facts with no links in or out;
- subjects with documents but no synthesis fact.

Lint output is a queue, surfaced in the UI, that a person works through. Lint
does not write facts.

**File back (on query).** A good answer synthesised from facts and chunks may be
stored as a derived fact by the model that produced it, citing what it used.
This is `memory_store` with citations, under the session's agent; the mechanism
exists and only the convention is new. The pattern is right that this is what
makes accumulation real.

## Reconciliation

Never automatic across provenance kinds. A fact may be superseded by:

- a fact with the same owner and the same agent -- a re-run, which is what
  summarize and revise do;
- a fact with the same owner and no agent -- a human correction through the UI
  or the CLI, which outranks any derived fact for that owner.

Anything else -- a different agent, a different owner -- is a sibling or a lint
flag. A derived fact that contradicts a user-authored one is flagged and shown;
the model does not get to overrule the person. Cross-owner reconciliation is a
multiuser question and is not designed here.

Confidence scores are deliberately absent. A score without an evidence chain
is a number the model invented, and citations plus agent plus supersession
history already are the evidence chain, so a score adds nothing to them.

## What stage two runs as

Stage two is a daemon-side worker, the same shape as the extraction queue: a
queue table, a loop, the configured generator. It writes through the store
with the owner's identity resolved server-side, not through the HTTP API with
a token, so there is no synthesis credential to issue, rotate, or leak. The
`ingest` scope still cannot write facts; stage two is not the ingest client and
holds no scope at all -- it is the daemon acting on a user's behalf after that
user's ingest client has finished.

The import-graph test from the corpus doc keeps chunking away from the
generator; stage two lives on the generator side of that line and reads chunks
through the store like any other consumer.

## Not decided here

- **Prompt design** for summarize and revise, and how much of the corpus doc's
  fencing applies when a chunk of untrusted text is fed to the summarizer.
  Untrusted chunks are still untrusted inside stage two; the derived fact must
  inherit the worst trust of its citations, and how that is displayed is a UI
  question.
- **Entity resolution.** Whether "webauth" in one document and "the auth
  server" in another are the same subject is a problem the pattern hand-waves
  and this design does not solve; first delivery uses the model's subject
  choice and lint's orphan check to find the misses.
- **Scale of revise.** Nightly over every touched subject is fine at personal
  scale; a corpus of many repositories may need a budget. Measure first.
- **Multiuser** sharing of derived facts (user-only, user-write-only, global
  read, global write) is a visibility predicate on the fact and belongs to the
  tier 3 phase 1 work. Owner plus agent on the fact does not preclude any of
  the four.

## Sequence

1. `memstore_agents`, `agent_id` on facts, `memstore_fact_citations`, and the
   store rule that a fact with an agent must cite something. This is the
   prerequisite for everything else and the one piece that must not be skipped:
   it is what makes a derived fact distinguishable from a session fact.
2. Summarize on ingest, with the result visible through the existing document
   and fact search paths.
3. The web UI's page view (`docs/web-ui.md`), so derived facts can be read and
   corrected before there are many of them.
4. Revise and lint as scheduled workers.
5. Session extraction moved onto the agent/citation mechanism.
