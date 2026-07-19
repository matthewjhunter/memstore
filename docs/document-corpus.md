# The document corpus: verbatim source, stored apart from facts

Status: design, not started
Author: Matthew + Claude
Date: 2026-07-18

## Why

Memstore needs to hold ingested material -- source files, notes, transcripts --
and the question that blocks it is trust. A fact carries no usable provenance
today: extraction stamps `{"source":"session"}` into `metadata`, and metadata is
model-writable, so it records what the model said about where something came
from rather than where it came from. Any design that answers "can I trust this
fact" with a label the model can write has answered nothing.

The first attempt at fixing that was a trust ladder: tiers for user-asserted,
file-ingested, model-asserted and derived facts, minted by entry point, with
attestation against session transcripts to make "the user told me this"
non-forgeable. It works, and it is a lot of machinery to make assertions
auditable.

The reframe is cheaper. Ingested files are not assertions and should not be
stored as facts at all. They are a second corpus with different rules, and once
they are separate the trust question stops being a policy problem and becomes a
checkable property.

## Two corpora, different rules

| | facts | documents |
|---|---|---|
| content | a claim, synthesized | a verbatim span of a file |
| written by | model, user | the ingest path only |
| lifecycle | supersession chains, confirm counts | replaced on re-ingest |
| wrongness | possible, expected, tracked | not a category -- it either matches the source or it is stale |
| linked | into the fact graph | not into the fact graph |

## The invariant that replaces the trust ladder

A chunk's bytes are identical to a span of the recorded file at the recorded
hash.

That is checkable at write time and re-checkable at any point after, against the
file or against git. It is not a claim anyone made, so it does not need to be
believed, attested, or promoted. The whole tier system existed to make
assertions auditable; documents are not assertions.

The corollary is the constraint the rest of the design serves: **nothing in the
ingest path may reason.** A summary, a paraphrase, or an extracted claim breaks
the invariant, because none of them are substrings of the file.

## No LLM in the ingest path

Structural, not aspirational:

- The ingest binary does not link a `Generator` and does not call
  `FactExtractor`. There is no LLM to call.
- Ingest runs under its own token scope (`ingest`), distinct from `write`. A
  token that can ingest cannot write facts; the MCP server's token can write
  facts and cannot ingest.
- No MCP tool creates documents. The model's only document capability is search,
  so there is nothing to forge into.

Chunking is mechanical. Structural boundaries where the format gives them --
markdown headings, Go declarations via `go/ast` -- and a fixed line window with
overlap where it does not. Deterministic, so re-ingesting an unchanged file
produces identical chunks.

## Chunk fields

All derived by the ingester at ingest time. None accepted as an argument, since
an argument is something a caller asserts.

    repo_url      canonical remote origin URL
    commit        SHA at ingest
    path          repo-relative
    basename      indexed as its own field
    lang          from path and content, recorded not inferred at query time
    byte_start    span within the file
    byte_end
    line_start    for citations
    line_end
    file_sha256   of the whole file
    mtime
    dirty         working tree unclean at ingest

`dirty` matters: content that is not committed has not been reviewed and did not
come through anyone's PR. It is worth ingesting and worth marking.

Trust is one field, `trusted` or `untrusted`, resolved from a per-user repo
policy table at ingest. Default untrusted. Rules key on canonical remote URL plus
a path prefix, because owning a repo does not mean writing every file in it --
`_external/` under `~/git` is vendored third-party text sitting inside a tree
that is otherwise ours. One field, used as a filter and a label. No algebra.

## Two metadata systems, and both are needed

The flexible `metadata` JSON on facts is not the mistake. Overloading it to carry
provenance was. Those are separate systems with opposite requirements, and the
store keeps both:

| | flexible metadata | provenance metadata |
|---|---|---|
| written by | the model, the caller | the entry point only |
| shape | freeform JSON, unversioned | fixed schema, typed columns |
| purpose | domain extensions -- `chapter`, `priority`, `surface: startup` | where this came from |
| trusted for | nothing security-relevant | exactly the questions it answers |
| queried by | opportunistic `MetadataFilter` / `json_extract` | indexed predicates |

Flexible metadata earns its keep. `{"surface":"startup"}` driving task
resurfacing, `{"is_draft":true}`, per-domain fields on novel-continuity facts --
none of that should be schema'd in advance, and a model inventing a useful key is
a feature. It stays exactly as it is.

Provenance is the opposite. It is queried structurally (every chunk from repo R
at commit C; every chunk whose file hash no longer matches), it needs referential
integrity, and its whole value is that no one can write it by hand. That makes it
typed columns, not JSON.

**The rule that keeps them apart:** provenance field names are reserved, and the
write path strips them from caller-supplied metadata rather than merging them.
Silently dropping a key the model set is correct here -- a model writing
`{"repo_url": "..."}` into flexible metadata has not recorded provenance, it has
recorded a claim, and letting the two share a namespace is how the current design
got into trouble. Log the strip; it is a useful signal about what callers expect.

**Facts get provenance later, on the same terms.** The schema is rigid there too
and populated from the entry point, not the arguments: which identity asserted
this, over which transport, when. `httpapi.Identity` already carries the material
(`Name`, `Source`, `UserID`, token name). That is the part of the trust-ladder
design worth keeping -- recording who asserted a fact is cheap and useful. The
part not worth keeping was ranking those assertions into tiers and attesting them
against session transcripts.

Sequencing: documents first, since they need it to function. Facts after, as an
additive migration -- existing facts get a null-ish "unknown, predates
provenance" origin rather than a backfilled guess.

## Retrieval

Separate index, separate tool. Document results are never merged by score with
fact results.

This is the same constraint as the multi-space embedding work in
`docs/embedding-model-routing.md`: scores computed over different populations
mean different things, and ranking them against each other is arithmetic on
incomparable numbers. Two result sections, not one list. Where both are wanted in
one call, they come back labeled and separately ranked.

Every chunk returns a citation: `repo@commit path:L120-160`. Mandatory, not
optional -- an answer built on the corpus has to be traceable back to a file, and
that traceability is the deliverable.

Untrusted chunks come back fenced, using the mechanism in
`docs/prompt-fencing-internal-llm.md`. A document corpus is a prompt-injection
surface by construction: the text is written by whoever wrote the repo, and it is
about to be injected into a context window. A README that says "ignore prior
instructions" is a normal thing to find in a corpus of real repositories.

## Synthesis is separated, not lost

The corpus stays verbatim. Synthesis over it still happens -- it lands in the
fact layer, as a fact that cites the chunks it was built from.

So "what is the auth design" has more than one answer available:

1. A stored synthesis, if one exists -- a fact, with citations to the chunks it
   came from.
2. The chunks themselves, from a document search.
3. Both, in two calls: read the synthesis, then fetch what it cites to check it.

A synthesis is a fact and keeps a fact's properties. It can be wrong, it can be
superseded when the code changes, it carries the reliability of whatever produced
it. What it cannot do is contaminate the corpus, because it is not stored there.

The link runs one way. A fact may cite chunk IDs; a chunk never becomes a fact.
No auto-promotion, no background job that reads documents and distills them into
the fact graph -- that is exactly the synthesis step this separation exists to
keep visible and attributable.

## What this deletes

Relative to the trust-ladder design, and relative to treating ingested files as
facts:

- No trust tiers, no tier arithmetic on derived facts.
- No attestation, no quote verification against session archives.
- No supersession chains for documents. Git has history. Re-ingest replaces on
  `(repo, path)`.
- No confirm counts, no use-count decay, no drift analytics for documents.
  `file_sha256` answers "is this current" in one comparison.

## Multiuser

Documents are read-only replicas of files the ingesting user could already read,
so ownership is simpler than it is for facts: scope by user and namespace, and
the repo policy table is per-user. A trusted repo for one user is not
automatically trusted for another.

Facts still need the asserter question answered -- who claimed this, under which
identity -- and `httpapi.Identity` already carries the material. That is separate
work and is not blocked by this.

## Open questions

**Chunk boundaries are the real failure mode.** With trust reduced to a checkable
invariant, the thing that determines whether the corpus is useful is where the
chunks are cut. A boundary that splits a function from its doc comment degrades
retrieval quietly, and unlike a bad trust label there is no obvious symptom.
Worth building an evaluation before tuning the chunker.

**The FTS measurements need re-running.** The findings recorded in
`docs/embedding-model-routing.md` -- whole-path tokenization, the decomposed
tsvector fallback, +54% GIN index and +83% lexemes -- were measured on a corpus
of 3865 facts containing almost no code. A document corpus inverts that token
distribution: identifiers and paths become the majority, the fallback stops being
a fallback and becomes the primary lexical mechanism, and the cost numbers will
not hold. Re-measure before committing to the decomposition.

Note that `basename` as a stored field makes "show me sqlite.go" a metadata
lookup rather than a full-text problem, which removes the sharpest instance of
that defect for documents. It remains a real defect for facts that mention file
paths in prose.

**Dependency.** This needs the code embedding space from
`docs/embedding-model-routing.md` to be worth building. Source
embedded with a general text model retrieves poorly, which is part of why the
last attempt at ingestion left almost no code in the store.

## Relationship to tier 4

`docs/tier4-bulk-ingestion.md` scoped bulk ingestion as facts-with-links, on the
premise that an import landing 10K isolated fact nodes leaves the graph no richer
than before. This design supersedes that premise for file ingestion: ingested
files land as documents, where graph connectivity is not the goal and citations
carry the structure instead.

The link-creation problem does not disappear, it narrows. It applies to
syntheses -- facts built over the corpus -- which is a much smaller population
than every chunk, and one where links are meaningful rather than mechanical.
Tier 4's remaining scope is bulk ingestion of material that genuinely is
assertions, such as a notes archive.
